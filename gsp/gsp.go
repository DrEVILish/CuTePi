package gsp

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
	"CuTePi/logs"
	"CuTePi/ws"
)

// manager holds the single active playback pipeline. All access is guarded
// by mu so that concurrent HTTP requests (Play/Load/ShowTest/Stop/Panic)
// can't race on the pipeline handle - the historical cause of "multiple
// pipelines" / "losing reference, can't stop playback" bugs.
type manager struct {
	mu           sync.Mutex
	pipeline     *gst.Pipeline
	currentFile  string // filename currently loaded, "" if none/test pattern
	version      uint64 // bumped on every client-visible state change/position tick
	lastPos      float64
	inPoint      float64      // seconds; playback starts here (0 = start of file)
	outPoint     float64      // seconds; playback auto-stops here (0 = end of file)
	hold         bool         // freeze the last frame at end-of-stream / trim-out
	loop         bool         // restart from the in-point when the clip reaches its end
	loopRemain   int          // passes left in a finite loop (loopCount); 0 = infinite
	volumeEl     *gst.Element // per-audio-branch "volume" element (last wins)
	volume       float64      // requested cue volume in dB
	loudnessGain float64
	rate         float64
	balance      float64
	mute         bool
	panEl        *gst.Element
	fadeIn       int
	fadeCurve    string
	fitMode      string  // fit|stretch frame fitting ("", fit = letterbox)
	rotation     int     // 0|90|180|270 clockwise degrees
	flip         string  // none|h|v mirror ("", none = off)
	fadeLevel    float64 // shared audio/video envelope, 0..1
	fadeSerial   uint64  // cancels an earlier ramp on the same pipeline
	starting     bool
	brightEl     *gst.Element  // per-video-branch "videobalance" element (last wins)
	cuePos       int           // cue position associated with the active clip (0 = not a cue)
	onCueEnd     func(pos int) // invoked (in a goroutine) when an active cue reaches its end
	gen          uint64        // bumped on every playback-decision change (load/stop/panic/teardown)
	warm         *gst.Pipeline // prebuilt+prerolled next cue; see Warm. Audio-only callers
	warmFile     string        // only, since a prerolled video pipeline paints its
	warmOpts     LoadOpts      // first frame onto the live wall (realtime-first rule)
	warmBuildGen uint64        // generation the arm was made under (stale-arm check)
}

var (
	mgr      manager
	initOnce sync.Once
)

func gstInit() {
	initOnce.Do(func() {
		gst.Init(nil)
		// Position ticker: while a clip is playing, push one sync per
		// displayed second over the WebSocket so connected clients advance
		// their progress clock WITHOUT HTTP polling (the pollers are now the
		// WS-disconnected fallback only). CurrentPosition bumps the version
		// when the displayed second changes and stays silent while paused/
		// stopped/idle, so the broadcast only fires on a real change.
		go func() {
			for range time.Tick(time.Second) {
				before := StateVersion()
				CurrentPosition()
				if StateVersion() != before {
					broadcastSoon()
				}
			}
		}()
	})
}

// bump increments the change counter the Now Playing poller relies on. It is
// called only when something the widget displays (filename, play state,
// position) actually changes.
// bump coalescer: playback state changes arrive in bursts (a 500ms fade
// steps ~50 times, a cue swap bumps several times). Every bump is one WS
// broadcast, and every broadcast makes connected clients re-fetch their
// partials — so bursts starve the realtime paths of the same CPU the
// pipeline needs. Trailing-edge debounce: at most one broadcast per 100ms
// window, and the LAST state always reaches clients.
var (
	bumpTimerMu sync.Mutex
	bumpTimer   *time.Timer
)

// broadcastAsap sends at most one "sync" broadcast per 100ms window;
// trailing edge, so the newest version always lands.
func broadcastSoon() {
	bumpTimerMu.Lock()
	defer bumpTimerMu.Unlock()
	if bumpTimer != nil {
		return
	}
	bumpTimer = time.AfterFunc(100*time.Millisecond, func() {
		bumpTimerMu.Lock()
		bumpTimer = nil
		bumpTimerMu.Unlock()
		ws.Broadcast()
	})
}

// Realtime rule: nothing but playback is prioritized — the deck can't spend CPU on bursts that don't change pixels.

func (m *manager) bump() {
	m.mu.Lock()
	m.version++
	m.mu.Unlock()
	broadcastSoon()
}

// StateVersion returns the change counter: clients re-render the Now Playing
// widget only when the value they last saw differs from this.
func StateVersion() uint64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.version
}

// Generation is a monotonic counter of playback-decision changes: pipeline
// load/swap, stop, panic, and end-of-clip teardown all advance it. Chain
// runners (auto-continue) capture it when they arm and abort if it moved.
// Unlike a CurrentCuePos equality check, it also detects the same cue being
// replayed — the operator re-triggering cue N during its own postWait window
// leaves cuePos unchanged but bumps the generation.
func Generation() uint64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.gen
}

// swap atomically stops/releases the current pipeline (if any) and installs
// newPipeline as the active one, under the manager lock.
func (m *manager) swap(newPipeline *gst.Pipeline, currentFile string, opts LoadOpts) {
	if !opts.KeepBackground {
		stopBackground() // a new playback decision always kills the soundtrack
	}
	m.mu.Lock()
	m.dropWarm() // a warm slot from a previous decision is somebody else's memory
	if m.pipeline != nil && m.pipeline != newPipeline {
		retirePipeline(m.pipeline)
	}
	m.clearPlayback()
	m.pipeline = newPipeline
	m.currentFile = currentFile
	m.inPoint = opts.InPoint
	m.outPoint = opts.OutPoint
	m.hold = opts.Hold
	m.loop = opts.Loop
	m.loopRemain = opts.LoopCount
	m.volume = opts.Volume // per-cue gain in dB (0 = 0dB)
	m.loudnessGain = opts.LoudnessGain
	m.rate = clampRate(opts.Rate)
	m.balance = math.Max(-1, math.Min(1, opts.Balance))
	m.mute = opts.Mute
	m.fadeIn = opts.FadeIn
	m.fadeCurve = opts.FadeCurve
	m.fitMode = opts.FitMode
	m.rotation = opts.Rotation
	m.flip = opts.Flip
	m.fadeLevel = 1
	if m.fadeIn > 0 {
		m.fadeLevel = 0
	}
	m.starting = true
	m.gen++
	m.mu.Unlock()
	broadcastSoon()
}

// clearPlayback resets every mutable playback field except the pipeline
// handle itself (the caller decides the pipeline's fate) and bumps the
// version so clients re-render. Callers must hold m.mu.
func (m *manager) clearPlayback() {
	m.currentFile = ""
	m.inPoint = 0
	m.outPoint = 0
	m.hold = false
	m.loop = false
	m.loopRemain = 0
	m.volumeEl = nil
	m.brightEl = nil
	m.panEl = nil
	m.starting = false
	m.fadeSerial++
	m.cuePos = 0
	m.version++
}

// clearIfCurrent nils out the pipeline only if it is still p, so a stale
// EOS/error callback from an already-replaced pipeline can't clobber a
// newer one.
func (m *manager) clearIfCurrent(p *gst.Pipeline) {
	m.mu.Lock()
	if m.pipeline == p {
		retirePipeline(p)
		m.pipeline = nil
		m.clearPlayback()
		m.gen++
		m.mu.Unlock()
		broadcastSoon()
		return
	}
	m.mu.Unlock()
}

// LoadOpts describes how a clip should play: an optional in/out trim window
// (seconds), whether the last frame is held on-screen when it ends, whether
// the clip loops back to its in-point at end-of-stream, and the per-cue
// master audio gain in dB (0 = 0dB). Playback volume is per-cue, never global.
type LoadOpts struct {
	InPoint        float64 // 0 = start of file
	OutPoint       float64 // 0 = end of file
	Hold           bool    // freeze last frame at end / trim-out
	Loop           bool    // restart from in-point at end / trim-out
	LoopCount      int     // finite loop count; 0 = infinite (ignored when Loop is false)
	Volume         float64 // per-cue master gain in dB, 0 = 0dB (range -60..12)
	LoudnessGain   float64 // per-media EBU R128 correction in dB
	Rate           float64 // 0 defaults to 1; valid range 0.25..4, pitch preserved
	Balance        float64 // -1 left, 0 centre, +1 right
	Mute           bool
	FadeIn         int    // milliseconds
	FadeCurve      string // envelope shape: linear|smooth|log|exp (§12.7); "" = linear
	FitMode        string // fit|stretch frame fitting ("" = fit)
	Rotation       int    // 0|90|180|270 clockwise degrees
	Flip           string // none|h|v mirror ("" = none)
	KeepBackground bool   // keep the background playlist alive across this load (slideshow slides are images ON the soundtrack, not new decisions)
}

func Play() {
	mgr.mu.Lock()
	p := mgr.pipeline
	inPoint := mgr.inPoint
	hold := mgr.hold
	starting := mgr.starting
	mgr.mu.Unlock()
	if p == nil || starting {
		return
	}
	_, state := p.GetState(gst.StateNull, 0)
	if state == gst.StateNull {
		startPlayback(p)
		mgr.bump()
		return
	}

	ok, dur := p.QueryDuration(gst.FormatTime)
	okPos, pos := p.QueryPosition(gst.FormatTime)
	atEnd := ok && okPos && dur > 0 && float64(pos) >= float64(dur)-500_000_000

	if hold && atEnd {
		// Restarting a clip frozen on its last frame: jump to the in-point
		// (or start) before going back to PLAYING, since a pipeline parked at
		// EOS will not advance on its own.
		if inPoint > 0 && float64(inPoint*1_000_000_000) < float64(dur) {
			seekAtRate(p, inPoint)
		} else {
			seekAtRate(p, 0)
		}
	} else if inPoint > 0 && okPos && float64(pos) < inPoint*1_000_000_000 {
		// Freshly loaded (or stop->play): never play the part before the
		// in-point, jump straight to the trim start.
		seekAtRate(p, inPoint)
	}

	p.SetState(gst.StatePlaying)
	mgr.bump()
}

func Pause() {
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	if err := p.SetState(gst.StatePaused); err != nil {
		logs.Printf(logs.GSPPauseErr, "gsp: error pausing: %v", err)
	}
	CurrentPosition()
	mgr.bump()
}

func TogglePause() {
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	stateChangeReturn, currentState := p.GetState(gst.StateNull, gst.ClockTimeNone)
	if stateChangeReturn != gst.StateChangeSuccess {
		return
	}
	var err error
	switch currentState {
	case gst.StatePlaying:
		err = p.SetState(gst.StatePaused)
	case gst.StatePaused:
		err = p.SetState(gst.StatePlaying)
	}
	if err != nil {
		logs.Printf(logs.GSPToggleErr, "gsp: error toggling pause: %v", err)
	}
	mgr.bump()
}

func Panic() {
	stopBackground()
	mgr.mu.Lock()
	if mgr.pipeline != nil {
		retirePipeline(mgr.pipeline)
		mgr.pipeline = nil
		mgr.clearPlayback()
		mgr.gen++
	}
	mgr.dropWarm() // PANIC tears everything down, including the warm slot
	mgr.mu.Unlock()
	broadcastSoon()
}

func Stop() {
	stopBackground()
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	if err := p.SetState(gst.StateNull); err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: error stopping: %v", err)
	}
	// Stop keeps the (nulled) pipeline so a later Play() can resume the same
	// clip, but the clip is no longer "current": a natural end or error clears
	// currentFile via clearIfCurrent, so stopping must too — otherwise the
	// media-delete guard keeps blocking deletion of a stopped clip.
	//
	// The cue association must go as well: the auto-continue and slideshow
	// runners guard on CurrentCuePos() ("is the ending cue still the active
	// decision"), and cuePos surviving Stop made an explicit stop invisible to
	// them — the next cue would still fire after the operator hit Stop, and a
	// stopped slideshow kept cycling. Resuming a stopped clip replays to its
	// end without re-arming the chain; that is the operator's explicit choice.
	mgr.mu.Lock()
	mgr.currentFile = ""
	mgr.cuePos = 0
	mgr.lastPos = 0
	mgr.starting = false
	mgr.dropWarm()
	mgr.gen++
	mgr.version++
	mgr.mu.Unlock()
	broadcastSoon()
}

// ShowTest loads and plays a GStreamer video-test-pattern.
func ShowTest(pattern string) error {
	gstInit()
	newPipeline, err := buildPipeline(pipelineSpec{isTest: true, testPattern: pattern})
	if err != nil {
		return err
	}
	mgr.swap(newPipeline, "", LoadOpts{})
	watchAndPlay(newPipeline)
	return nil
}

// Load loads (and starts) playback of filename from the configured media
// directory, replacing any currently active pipeline. Direct media playback
// has no cue-specific hold policy but honours the configured loop default.
func Load(filename string) error {
	return LoadWithOpts(filename, LoadOpts{Loop: config.Loop()})
}

// LoadWithOpts loads filename with an optional trim window and hold policy.
// A warm slot matching file+opts (see Warm) activates instead of a fresh
// build+preroll — the ~0-latency cue path; anything else falls back to the
// normal build (and drops the stale slot).
func LoadWithOpts(filename string, opts LoadOpts) error {
	gstInit()
	if InstallWarm(filename, opts) {
		return nil
	}
	mgr.mu.Lock()
	mgr.dropWarm()
	mgr.mu.Unlock()
	newPipeline, err := buildPipeline(pipelineSpec{isTest: false, filename: filename})
	if err != nil {
		return err
	}
	mgr.swap(newPipeline, filename, opts)
	watchAndPlay(newPipeline)
	return nil
}

func (m *manager) dropWarm() {
	if m.warm == nil {
		return
	}
	retirePipeline(m.warm)
	m.warm = nil
	m.warmFile = ""
	m.warmOpts = LoadOpts{}
}

// Warm prebuilds and prerolls the NEXT cue into a slot (deck-style double
// buffer for audio cues), so a later LoadWithOpts with the same file+opts
// activates it instead of rebuilding. Callers must only Warm audio-only
// media: a prerolled video pipeline paints its first frame on the live
// output, which is exactly what the realtime rule forbids. The build
// validators: a stale arm (a newer playback decision landed meanwhile) is
// retired, never installed.
func Warm(file string, opts LoadOpts) error {
	gstInit()
	mgr.mu.Lock()
	buildGen := mgr.gen
	if mgr.warm != nil {
		retirePipeline(mgr.warm)
		mgr.warm, mgr.warmFile, mgr.warmOpts = nil, "", LoadOpts{}
	}
	mgr.mu.Unlock()
	p, err := buildPipeline(pipelineSpec{isTest: false, filename: file})
	if err != nil {
		return err
	}
	// Preroll parked (paused): first buffers decoded, nothing displayed, no
	// audible output until a later activation starts the pipeline.
	p.SetState(gst.StatePaused)
	result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
	if result == gst.StateChangeFailure {
		retirePipeline(p)
		return fmt.Errorf("prewarm preroll failed: %v", result)
	}
	mgr.mu.Lock()
	current := mgr.gen == buildGen
	if current {
		mgr.warm = p
		mgr.warmFile = file
		mgr.warmOpts = opts
	}
	mgr.mu.Unlock()
	if !current {
		// A newer playback decision landed while we built — warm now.
		retirePipeline(p)
		return nil
	}
	watchWarm(p)
	return nil
}

// watchWarm parks the slot's error watch while the pipeline is idle: any
// bus error or retirement unregisters. EOS cannot occur here — a PAUSED
// pipeline never reaches end-of-stream on its own.
func watchWarm(p *gst.Pipeline) {
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		mgr.mu.Lock()
		current := mgr.warm == p
		mgr.mu.Unlock()
		if !current {
			return false
		}
		if msg.Type() == gst.MessageError {
			logs.Printf(logs.GSPPipeDebug, "gsp: warm pipeline error debug: %s", msg.ParseError().DebugString())
			mgr.mu.Lock()
			if mgr.warm == p {
				retirePipeline(p)
				mgr.warm, mgr.warmFile, mgr.warmOpts = nil, "", LoadOpts{}
			}
			mgr.mu.Unlock()
			return false
		}
		return true
	})
}

// InstallWarm activates the prewarmed pipeline for file+opts if the slot is
// still the one Warm built: retire the current transport, install the warm
// pipeline, play. False = stale/mismatched slot, caller falls back.
func InstallWarm(file string, opts LoadOpts) bool {
	mgr.mu.Lock()
	p, ok := mgr.warm, mgr.warmFile == file && mgr.warmOpts == opts
	if ok {
		mgr.warm, mgr.warmFile, mgr.warmOpts = nil, "", LoadOpts{} // claim before retiring anything else
	}
	mgr.mu.Unlock()
	if !ok {
		return false
	}
	logs.Printf(logs.GSPWarm, "warm pipeline activated: %s", file)
	// swap() owns the retire+install; give it the prerolled handle. Its
	// clearPlayback/dropWarm have already been done above for the slot.
	mgr.swap(p, file, opts)
	watchAndPlay(p)
	return true
}

func CurrentPlaying() string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.currentFile
}

// SetCuePos ties the active clip to a cuesheet position so the end-of-cue
// hook knows which cue finished. 0 clears the association (direct playback).
func SetCuePos(pos int) {
	mgr.mu.Lock()
	mgr.cuePos = pos
	mgr.mu.Unlock()
}

// CurrentCuePos returns the cue position associated with the active clip
// (0 if none / direct playback).
func CurrentCuePos() int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.cuePos
}

// SetCueEndHook registers the app-layer callback fired when an active cue
// reaches its end (volume/hold or teardown, excluding loop restart).
func SetCueEndHook(cb func(pos int)) {
	mgr.mu.Lock()
	mgr.onCueEnd = cb
	mgr.mu.Unlock()
}

func CurrentPosition() float64 {
	// Snapshot the pipeline under the lock, then run the GStreamer query
	// outside it (QueryPosition can block on the bus; holding mgr.mu would
	// stall every other playback call). Same shape as CurrentDuration.
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return 0
	}
	ok, pos := p.QueryPosition(gst.FormatTime)
	if !ok {
		return 0
	}
	seconds := float64(pos) / float64(gst.ClockTime(1_000_000_000))
	// A position tick the client cares about is one that changes the
	// displayed clock (rounded to the nearest second, matching formatClock).
	// While paused/stopped the value is frozen, so no bump and no re-render.
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if int(seconds+0.5) != int(mgr.lastPos+0.5) {
		mgr.version++
	}
	mgr.lastPos = seconds
	return seconds
}

func CurrentDuration() float64 {
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return 0
	}
	ok, dur := p.QueryDuration(gst.FormatTime)
	if !ok {
		return 0
	}
	return float64(dur) / float64(gst.ClockTime(1_000_000_000))
}

// Rate reports the active clip's playback rate (1 when nothing is loaded or
// the clip hasn't counted a rate yet). Remote transports report speed as a
// percentage of this.
func Rate() float64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.pipeline == nil || mgr.rate == 0 {
		return 1
	}
	return mgr.rate
}

// IsPaused reports whether the active pipeline is PAUSED. False when nothing
// is loaded (so callers don't special-case "stopped" separately).
func IsPaused() bool {
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return false
	}
	_, state := p.GetState(gst.StateNull, 0)
	return state == gst.StatePaused
}

// Loop reports whether the active pipeline loops at end-of-stream. With no
// pipeline loaded it reports the configured default so the settings/handlers
// reflect what the next load will do.
func Loop() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.pipeline == nil {
		return config.Loop()
	}
	return mgr.loop
}

// SetLoop toggles loop-at-end for the currently loaded clip and persists it
// as the default for future loads.
func SetLoop(loop bool) {
	mgr.mu.Lock()
	mgr.loop = loop
	mgr.mu.Unlock()
	config.SetLoop(loop)
	mgr.bump()
}

// Volume returns the active cue's playback gain in dB (0 = 0dB). With no
// pipeline loaded there is no cue volume concept, so it reports 0dB.
func Volume() float64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.pipeline == nil {
		return 0
	}
	return mgr.volume
}

// FadeAndStop fades the active clip to black over durMs (volume -> 0 and,
// for video, brightness -> -1) and then tears the pipeline down. It blocks
// until the fade finishes; a zero or negative duration stops at once.
// The tween samples the element pointers under the lock each step and
// bails if the pipeline is swapped out mid-fade.
// ponytail: single tween goroutine at the single-active-pipeline scale.
func FadeAndStop(durMs int) {
	mgr.mu.Lock()
	p := mgr.pipeline
	gen := mgr.gen
	fadeCurve := mgr.fadeCurve
	mgr.fadeSerial++
	serial := mgr.fadeSerial
	startLevel := mgr.fadeLevel
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	if durMs <= 0 {
		Stop()
		return
	}
	steps := 20
	stepMs := time.Duration(durMs) * time.Millisecond / time.Duration(steps)
	for i := 1; i <= steps; i++ {
		time.Sleep(stepMs)
		mgr.mu.Lock()
		if mgr.pipeline != p || mgr.gen != gen || mgr.fadeSerial != serial {
			mgr.mu.Unlock()
			return
		}
		mgr.fadeLevel = startLevel * (1 - fadeShape(fadeCurve, float64(i)/float64(steps)))
		mgr.applyGain()
		if mgr.brightEl != nil {
			mgr.brightEl.Set("brightness", mgr.fadeLevel-1)
		}
		mgr.mu.Unlock()
	}
	mgr.clearIfCurrent(p)
}

// Caller holds mu; live changes and both fades share the same effective gain.
func (m *manager) effectiveGain() float64 {
	if m.mute || m.volume <= -60 {
		return 0
	}
	return dbToGain(dbClamp(m.volume+m.loudnessGain)) * m.fadeLevel
}

func (m *manager) applyGain() {
	if m.volumeEl != nil {
		m.volumeEl.Set("volume", m.effectiveGain())
	}
}

// fadeShape maps ramp progress t (0..1) through the selected envelope curve.
// All callers share one envelope (audio gain + video brightness), so a curve
// changes both identically. linear is the historical flat ramp.
func fadeShape(curve string, t float64) float64 {
	if t <= 0 {
		return 0
	}
	if t >= 1 {
		return 1
	}
	switch curve {
	case "smooth": // S-curve (smoothstep): slow ends, fast middle
		return t * t * (3 - 2*t)
	case "log": // perceptual: fast initial change, long tail
		return math.Log10(1+9*t) / 1
	case "exp": // slow start, sharp finish
		const k = 3.0
		return (math.Exp(k*t) - 1) / (math.Exp(k) - 1)
	default: // linear
		return t
	}
}

func fadeIn(p *gst.Pipeline, gen uint64) {
	mgr.mu.Lock()
	serial, duration := mgr.fadeSerial, time.Duration(mgr.fadeIn)*time.Millisecond
	mgr.mu.Unlock()
	if duration <= 0 {
		return
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var elapsed time.Duration
	last := time.Now()
	for now := range ticker.C {
		_, state := p.GetState(gst.StateNull, 0)
		delta := now.Sub(last)
		last = now
		mgr.mu.Lock()
		if mgr.pipeline != p || mgr.gen != gen || mgr.fadeSerial != serial {
			mgr.mu.Unlock()
			return
		}
		if state == gst.StatePlaying {
			elapsed += delta
		}
		mgr.fadeLevel = fadeShape(mgr.fadeCurve, math.Min(1, float64(elapsed)/float64(duration)))
		mgr.applyGain()
		if mgr.brightEl != nil {
			mgr.brightEl.Set("brightness", mgr.fadeLevel-1)
		}
		done := mgr.fadeLevel == 1
		mgr.mu.Unlock()
		if done {
			return
		}
	}
}

// dbToGain converts a dB value (as stored per-cue) to the linear gain the
// GStreamer "volume" element expects: 0dB -> 1.0, -6dB -> 0.5, +6dB -> 2.0.
// Values at or below -inf-ish dB clamp to silence.
func dbToGain(db float64) float64 {
	if db <= -60 {
		return 0
	}
	return math.Pow(10, db/20.0)
}

// SetVolume applies a live per-cue gain (in dB) to the active pipeline's
// audio branch (0dB = no change). No global master exists; callers persist the
// value onto the cue. Returns the clamped dB value actually applied.
func SetVolume(v float64) float64 {
	if math.IsNaN(v) {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.volume
	}
	v = dbClamp(v)
	mgr.mu.Lock()
	mgr.volume = v
	mgr.applyGain()
	mgr.mu.Unlock()
	mgr.bump()
	return v
}

// dbClamp clamps a dB value to the Cue Inspector slider's range.
func dbClamp(v float64) float64 {
	return math.Max(-60, math.Min(12, v))
}

func SetMute(mute bool) {
	mgr.mu.Lock()
	mgr.mute = mute
	mgr.applyGain()
	mgr.mu.Unlock()
}

func SetBalance(balance float64) {
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return
	}
	mgr.mu.Lock()
	mgr.balance = math.Max(-1, math.Min(1, balance))
	if mgr.panEl != nil {
		mgr.panEl.Set("panorama", float32(mgr.balance))
	}
	mgr.mu.Unlock()
}

func clampRate(rate float64) float64 {
	if rate == 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 1
	}
	return math.Max(0.25, math.Min(4, rate))
}

func SetRate(rate float64) {
	mgr.mu.Lock()
	rate = clampRate(rate)
	if mgr.rate == rate {
		mgr.mu.Unlock()
		return
	}
	mgr.rate = rate
	p, starting := mgr.pipeline, mgr.starting
	mgr.mu.Unlock()
	if p != nil && !starting {
		// A preceding flush seek may still be prerolling; its position query
		// is temporarily unavailable. Wait before issuing the new rate seek.
		p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
		if ok, pos := p.QueryPosition(gst.FormatTime); ok {
			seekAtRate(p, float64(pos)/1e9)
		}
	}
}

// Every seek carries the segment rate, including loop and trim restarts.
func seekAtRate(p *gst.Pipeline, seconds float64) bool {
	mgr.mu.Lock()
	current, rate := mgr.pipeline == p, mgr.rate
	mgr.mu.Unlock()
	if !current || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return false
	}
	return p.SendEvent(gst.NewSeekEvent(clampRate(rate), gst.FormatTime, gst.SeekFlagFlush,
		gst.SeekTypeSet, int64(math.Max(0, seconds)*1e9), gst.SeekTypeEnd, -1))
}

// Seek moves playback to the given absolute position (seconds). It clamps to
// the clip's duration and, when a trim out-point is set, to that out-point.
func Seek(seconds float64) {
	mgr.mu.Lock()
	p := mgr.pipeline
	inPoint := mgr.inPoint
	outPoint := mgr.outPoint
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	ok, dur := p.QueryDuration(gst.FormatTime)
	if ok {
		max := float64(dur) / 1e9
		if outPoint > 0 && outPoint < max {
			max = outPoint
		}
		if seconds > max {
			seconds = max
		}
	}
	if seconds < inPoint && inPoint > 0 {
		seconds = inPoint
	}
	seekAtRate(p, seconds)
	mgr.bump()
}

// handleEnd deals with a clip that reached its end (natural end-of-stream or
// a trim out-point). With hold enabled the final frame stays on screen:
// GStreamer sinks already render the last frame at EOS, so the pipeline is
// simply parked in PAUSED and the manager keeps its reference (Stop/Panic
// still tear it down). With loop enabled the clip restarts from its in-point.
// Without either, the pipeline is torn down as before.
func (m *manager) handleEnd(p *gst.Pipeline) bool {
	m.mu.Lock()
	if m.pipeline != p {
		m.mu.Unlock()
		return false
	}
	hold := m.hold
	remaining := m.loopRemain
	inPoint := m.inPoint
	// Capture the cue association before any teardown: the non-hold path
	// below clears it (clearIfCurrent -> clearPlayback zeroes cuePos), and
	// the end hook needs the position that just finished. Capturing after
	// teardown meant the hook only ever fired for held cues — non-hold cues
	// ended silently, with no cue_end audit entry and no auto-continue.
	pos := m.cuePos
	cb := m.onCueEnd
	if m.loop {
		// A finite loop count is exhausted pass by pass; 0 means infinite
		// and always restarts.
		restart, next := loopSteps(remaining)
		m.loopRemain = next
		if restart {
			m.mu.Unlock()
			seekAtRate(p, inPoint)
			p.SetState(gst.StatePlaying)
			m.bump()
			return true
		}
	}
	m.mu.Unlock()

	if hold {
		p.SetState(gst.StatePaused)
		m.bump()
	} else {
		m.clearIfCurrent(p)
	}

	// AutoFollow hook: if a cue just finished (not looping), let the app
	// layer advance the selection to the next cue.
	if pos > 0 && cb != nil {
		go cb(pos)
	}
	return false
}

// loopSteps decides what a looping clip does at end-of-stream: whether it
// restarts and how many passes remain. remaining is the count still to play
// (0 = infinite, always restart). 0 is decremented to 0 forever; a positive
// count drops by one each pass and the clip stops once it reaches 0, so a
// finite count of N plays the clip N times before ending like a non-loop.
func loopSteps(remaining int) (restart bool, next int) {
	if remaining == 0 {
		return true, 0
	}
	if remaining > 1 {
		return true, remaining - 1
	}
	return false, 0
}

// watchAndPlay starts p playing and installs a bus watch that handles
// end-of-stream (freezing the last frame or tearing down, per the hold
// policy) and errors (always torn down). When an out-point is set, a
// background goroutine polls the position and ends the clip there.
func watchAndPlay(p *gst.Pipeline) {
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		// Identity precheck: if this pipeline was retired (replaced/stopped),
		// unregister the watch by returning false. Without this the closure
		// - and through it the pipeline and its bus - is pinned forever,
		// because a retired pipeline never reaches EOS or error (the only
		// other ways the watch unregisters). retirePipeline posts a wake-up
		// message so this check actually runs after a retirement.
		mgr.mu.Lock()
		current := mgr.pipeline == p
		mgr.mu.Unlock()
		if !current {
			return false
		}
		switch msg.Type() {
		case gst.MessageEOS:
			mgr.handleEnd(p)
			mgr.mu.Lock()
			current := mgr.pipeline == p
			mgr.mu.Unlock()
			return current
		case gst.MessageError:
			gerr := msg.ParseError()
			logs.Printf(logs.GSPPipeDebug, "gsp: pipeline error debug: %s", gerr.DebugString())
			logs.Printf(logs.GSPPipeStopped, "gsp: pipeline stopped: %s", gerr.Error())
			mgr.clearIfCurrent(p)
			return false
		}
		return true
	})
	startPlayback(p)
}

func startPlayback(p *gst.Pipeline) {
	mgr.mu.Lock()
	if mgr.pipeline != p {
		mgr.mu.Unlock()
		return
	}
	gen := mgr.gen
	mgr.starting = true
	mgr.fadeLevel = 1
	if mgr.fadeIn > 0 {
		mgr.fadeLevel = 0
	}
	mgr.fadeSerial++
	mgr.applyGain()
	if mgr.brightEl != nil {
		mgr.brightEl.Set("brightness", mgr.fadeLevel-1)
	}
	mgr.mu.Unlock()
	// Preroll before seeking: no initial audio/frame leaks before the trim or
	// rate is applied, and slow decoders need no guessed sleep duration.
	p.SetState(gst.StatePaused)
	result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
	mgr.mu.Lock()
	current := mgr.pipeline == p && mgr.gen == gen
	inPoint, rate, outPoint := mgr.inPoint, mgr.rate, mgr.outPoint
	mgr.mu.Unlock()
	if !current {
		return
	}
	if result == gst.StateChangeFailure || result == gst.StateChangeAsync {
		mgr.clearIfCurrent(p)
		return
	}
	if inPoint > 0 || rate != 1 {
		seekAtRate(p, inPoint)
	}
	mgr.mu.Lock()
	if mgr.pipeline != p || mgr.gen != gen {
		mgr.mu.Unlock()
		return
	}
	mgr.starting = false
	mgr.mu.Unlock()
	p.SetState(gst.StatePlaying)
	go fadeIn(p, gen)
	if outPoint > 0 {
		go watchTrim(p)
	}
}

// retirePipeline tears down a pipeline that is no longer (or about to stop
// being) the active one and posts a wake-up message to its bus so the watch
// closure's identity precheck runs and unregisters. SetState(Null) alone
// never produces a bus message, so the watch - and everything it captures -
// would otherwise leak on every cue swap for the lifetime of the process.
func retirePipeline(p *gst.Pipeline) {
	p.SetState(gst.StateNull)
	p.GetPipelineBus().Post(gst.NewApplicationMessage(p, gst.NewStructure("retire")))
}

// watchTrim polls the pipeline position until the out-point is reached and
// then terminates the clip (freezing or tearing down per the hold policy).
// It stops early if the pipeline is replaced or stopped.
func watchTrim(p *gst.Pipeline) {
	for {
		mgr.mu.Lock()
		if mgr.pipeline != p {
			mgr.mu.Unlock()
			return
		}
		outPoint := mgr.outPoint
		mgr.mu.Unlock()
		if outPoint <= 0 {
			return
		}
		ok, pos := p.QueryPosition(gst.FormatTime)
		if ok && float64(pos) >= outPoint*1_000_000_000 {
			if !mgr.handleEnd(p) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type pipelineSpec struct {
	isTest      bool
	testPattern string
	filename    string
}

// rotationMethod maps cue rotation degrees to a videoflip method.
func rotationMethod(deg int) string {
	switch deg {
	case 90:
		return "clockwise"
	case 180:
		return "rotate-180"
	case 270:
		return "counterclockwise"
	default:
		return "none"
	}
}

// flipMethod maps cue mirror mode to a videoflip method.
func flipMethod(f string) string {
	switch f {
	case "h":
		return "horizontal-flip"
	case "v":
		return "vertical-flip"
	default:
		return "none"
	}
}

// buildPipeline constructs either a video-test-pattern pipeline or a
// file-playback pipeline (filesrc -> decodebin -> auto{audio,video}sink),
// depending on spec. This replaces the previous buildFilePipeline/
// buildTestPipeline, which were ~90% duplicated.
func buildPipeline(spec pipelineSpec) (*gst.Pipeline, error) {
	pipeline, err := gst.NewPipeline("")
	if err != nil {
		return nil, err
	}

	var src *gst.Element
	if spec.isTest {
		src, err = gst.NewElement("videotestsrc")
		if err != nil {
			return nil, err
		}
		src.Set("pattern", spec.testPattern)
	} else {
		src, err = gst.NewElement("filesrc")
		if err != nil {
			return nil, err
		}
		srcFile := config.MediaLocation() + "/" + spec.filename
		src.Set("location", srcFile)
	}

	decodebin, err := gst.NewElement("decodebin")
	if err != nil {
		return nil, err
	}

	pipeline.AddMany(src, decodebin)
	src.Link(decodebin)

	// Connect to decodebin's pad-added signal, emitted whenever it finds a
	// stream from the input and a way to decode it to raw format. decodebin
	// adds a src-pad for the raw stream, which we link into a playback tail.
	decodebin.Connect("pad-added", func(self *gst.Element, srcPad *gst.Pad) {
		var isAudio, isVideo bool
		caps := srcPad.GetCurrentCaps()
		for i := 0; i < caps.GetSize(); i++ {
			st := caps.GetStructureAt(i)
			if strings.HasPrefix(st.Name(), "audio/") {
				isAudio = true
			}
			if strings.HasPrefix(st.Name(), "video/") {
				isVideo = true
			}
		}

		logs.Printf(logs.GSPPadAdded, "gsp: new pad added, is_audio=%v, is_video=%v\n", isAudio, isVideo)

		if !isAudio && !isVideo {
			err := errors.New("could not detect media stream type")
			msg := gst.NewErrorMessage(self, gst.NewGError(1, err), "", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}

		var elementNames []string
		queueName := "video-queue"
		if isAudio {
			queueName = "audio-queue"
			elementNames = []string{"queue", "audioconvert", "audioresample", "volume", "audiopanorama", "scaletempo", "autoaudiosink"}
		} else {
			// Two videoflip stages (rotate, then mirror) so a cue can
			// combine e.g. 90° with a horizontal mirror; method=none
			// passes frames through untouched.
			elementNames = []string{"queue", "videoconvert", "videobalance", "videoscale", "videoflip", "videoflip", "autovideosink"}
		}
		queueName += "-" + srcPad.GetStreamID()
		// decodebin recreates its pads after Stop -> Play. Reuse the tail;
		// an abandoned, unlinked sink would otherwise prevent preroll forever.
		if queue, err := pipeline.GetElementByName(queueName); err == nil && queue != nil {
			srcPad.Link(queue.GetStaticPad("sink"))
			return
		}

		elements, err := gst.NewElementMany(elementNames...)
		if err != nil {
			msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "could not create playback elements", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}
		elements[0].Set("name", queueName)
		pipeline.AddMany(elements...)
		gst.ElementLinkMany(elements...)

		// Elements must be synced to the pipeline's state, otherwise they
		// stay in Null state and can't process data.
		for _, e := range elements {
			e.SyncStateWithParent()
		}

		// Retain the audio "volume" element so SetVolume can drive it, and
		// apply the per-cue master gain once an audio branch exists. The
		// manager fields are only written if this pipeline is STILL the
		// active one: decodebin's pad-added fires asynchronously, so a rapid
		// load A->B can emit A's pads after B was swapped in - writing A's
		// elements here would make SetVolume/FadeAndStop ramp the dead
		// pipeline while B played unattended.
		if isAudio {
			mgr.mu.Lock()
			if mgr.pipeline == pipeline {
				mgr.volumeEl = elements[3]
				mgr.panEl = elements[4]
				mgr.applyGain()
				elements[4].Set("panorama", float32(mgr.balance))
			}
			mgr.mu.Unlock()
		}
		// Retain the video "videobalance" element so the fade-to-black ramp can
		// drive its brightness; a fresh clip always starts at full brightness.
		if isVideo {
			mgr.mu.Lock()
			if mgr.pipeline == pipeline {
				mgr.brightEl = elements[2]
				elements[2].Set("brightness", mgr.fadeLevel-1)
				// Per-cue frame geometry (§5.5): letterbox vs stretch on
				// videoscale, rotation then mirror on the two videoflips.
				fit := mgr.fitMode
				if fit != "stretch" {
					fit = "fit"
				}
				elements[3].Set("add-borders", fit == "fit")
				elements[4].Set("method", rotationMethod(mgr.rotation))
				elements[5].Set("method", flipMethod(mgr.flip))
			}
			mgr.mu.Unlock()
		}

		queue := elements[0]
		sinkPad := queue.GetStaticPad("sink")
		srcPad.Link(sinkPad)
	})

	return pipeline, nil
}
