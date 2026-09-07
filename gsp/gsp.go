package gsp

import (
	"errors"
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
	mu          sync.Mutex
	pipeline    *gst.Pipeline
	currentFile string // filename currently loaded, "" if none/test pattern
	version     uint64 // bumped on every client-visible state change/position tick
	lastPos     float64
	inPoint     float64       // seconds; playback starts here (0 = start of file)
	outPoint    float64       // seconds; playback auto-stops here (0 = end of file)
	hold        bool          // freeze the last frame at end-of-stream / trim-out
	loop        bool          // restart from the in-point when the clip reaches its end
	loopRemain  int           // passes left in a finite loop (loopCount); 0 = infinite
	volumeEl    *gst.Element  // per-audio-branch "volume" element (last wins)
	volume      float64       // requested volume 0..1 for the active pipeline
	brightEl    *gst.Element  // per-video-branch "videobalance" element (last wins)
	cuePos      int           // cue position associated with the active clip (0 = not a cue)
	onCueEnd    func(pos int) // invoked (in a goroutine) when an active cue reaches its end
}

var (
	mgr      manager
	initOnce sync.Once
)

func gstInit() {
	initOnce.Do(func() {
		gst.Init(nil)
	})
}

// bump increments the change counter the Now Playing poller relies on. It is
// called only when something the widget displays (filename, play state,
// position) actually changes.
func (m *manager) bump() {
	m.mu.Lock()
	m.version++
	m.mu.Unlock()
	go ws.Broadcast()
}

// StateVersion returns the change counter: clients re-render the Now Playing
// widget only when the value they last saw differs from this.
func StateVersion() uint64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.version
}

// swap atomically stops/releases the current pipeline (if any) and installs
// newPipeline as the active one, under the manager lock.
func (m *manager) swap(newPipeline *gst.Pipeline, currentFile string, opts LoadOpts) {
	m.mu.Lock()
	if m.pipeline != nil {
		m.pipeline.SetState(gst.StateNull)
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
	m.mu.Unlock()
	go ws.Broadcast()
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
	m.cuePos = 0
	m.version++
}

// clearIfCurrent nils out the pipeline only if it is still p, so a stale
// EOS/error callback from an already-replaced pipeline can't clobber a
// newer one.
func (m *manager) clearIfCurrent(p *gst.Pipeline) {
	m.mu.Lock()
	if m.pipeline == p {
		m.pipeline.SetState(gst.StateNull)
		m.pipeline = nil
		m.clearPlayback()
		m.mu.Unlock()
		go ws.Broadcast()
		return
	}
	m.mu.Unlock()
}

// LoadOpts describes how a clip should play: an optional in/out trim window
// (seconds), whether the last frame is held on-screen when it ends, whether
// the clip loops back to its in-point at end-of-stream, and the per-cue
// master audio gain in dB (0 = 0dB). Playback volume is per-cue, never global.
type LoadOpts struct {
	InPoint   float64 // 0 = start of file
	OutPoint  float64 // 0 = end of file
	Hold      bool    // freeze last frame at end / trim-out
	Loop      bool    // restart from in-point at end / trim-out
	LoopCount int     // finite loop count; 0 = infinite (ignored when Loop is false)
	Volume    float64 // per-cue master gain in dB, 0 = 0dB (range -60..12)
}

func Play() {
	mgr.mu.Lock()
	p := mgr.pipeline
	inPoint := mgr.inPoint
	hold := mgr.hold
	mgr.mu.Unlock()
	if p == nil {
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
			p.SeekTime(time.Duration(inPoint*float64(time.Second)), gst.SeekFlagFlush)
		} else {
			p.SeekTime(0, gst.SeekFlagFlush)
		}
	} else if inPoint > 0 && okPos && float64(pos) < inPoint*1_000_000_000 {
		// Freshly loaded (or stop->play): never play the part before the
		// in-point, jump straight to the trim start.
		p.SeekTime(time.Duration(inPoint*float64(time.Second)), gst.SeekFlagFlush)
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
	mgr.mu.Lock()
	if mgr.pipeline != nil {
		mgr.pipeline.SetState(gst.StateNull)
		mgr.pipeline = nil
		mgr.clearPlayback()
	}
	mgr.mu.Unlock()
	go ws.Broadcast()
}

func Stop() {
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
	mgr.mu.Lock()
	mgr.currentFile = ""
	mgr.version++
	mgr.mu.Unlock()
	go ws.Broadcast()
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
func LoadWithOpts(filename string, opts LoadOpts) error {
	gstInit()
	newPipeline, err := buildPipeline(pipelineSpec{isTest: false, filename: filename})
	if err != nil {
		return err
	}
	mgr.swap(newPipeline, filename, opts)
	watchAndPlay(newPipeline)
	return nil
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
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.pipeline == nil {
		return 0
	}
	ok, pos := mgr.pipeline.QueryPosition(gst.FormatTime)
	if !ok {
		return 0
	}
	seconds := float64(pos) / float64(gst.ClockTime(1_000_000_000))
	// A position tick the client cares about is one that changes the
	// displayed clock (rounded to the nearest second, matching formatClock).
	// While paused/stopped the value is frozen, so no bump and no re-render.
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
// for video, brightness -> -1) and then tears the pipeline down. It spawns a
// goroutine and returns immediately; a zero or negative duration stops at
// once. The tween samples the element pointers under the lock each step and
// bails if the pipeline is swapped out mid-fade.
// ponytail: single tween goroutine at the single-active-pipeline scale.
func FadeAndStop(durMs int) {
	mgr.mu.Lock()
	p := mgr.pipeline
	startGain := dbToGain(mgr.volume)
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	if durMs <= 0 {
		Stop()
		return
	}
	steps := 20
	stepMs := time.Duration(durMs/steps) * time.Millisecond
	dVol := startGain / float64(steps)
	for i := 1; i <= steps; i++ {
		mgr.mu.Lock()
		still := mgr.pipeline == p
		ve := mgr.volumeEl
		be := mgr.brightEl
		mgr.mu.Unlock()
		if !still {
			return
		}
		v := startGain - dVol*float64(i)
		if v < 0 {
			v = 0
		}
		if ve != nil {
			ve.Set("volume", v)
		}
		if be != nil {
			be.Set("brightness", -float64(i)/float64(steps))
		}
		time.Sleep(stepMs)
	}
	Stop()
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
	vol := mgr.volumeEl
	mgr.mu.Unlock()
	if vol != nil {
		vol.Set("volume", dbToGain(v))
	}
	mgr.bump()
	return v
}

// dbClamp clamps a dB value to the Cue Inspector slider's range.
func dbClamp(v float64) float64 {
	return math.Max(-60, math.Min(12, v))
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
	p.SeekTime(time.Duration(seconds*float64(time.Second)), gst.SeekFlagFlush)
	mgr.bump()
}

// handleEnd deals with a clip that reached its end (natural end-of-stream or
// a trim out-point). With hold enabled the final frame stays on screen:
// GStreamer sinks already render the last frame at EOS, so the pipeline is
// simply parked in PAUSED and the manager keeps its reference (Stop/Panic
// still tear it down). With loop enabled the clip restarts from its in-point.
// Without either, the pipeline is torn down as before.
func (m *manager) handleEnd(p *gst.Pipeline) {
	m.mu.Lock()
	if m.pipeline != p {
		m.mu.Unlock()
		return
	}
	hold := m.hold
	remaining := m.loopRemain
	inPoint := m.inPoint
	if m.loop {
		// A finite loop count is exhausted pass by pass; 0 means infinite
		// and always restarts.
		restart, next := loopSteps(remaining)
		m.loopRemain = next
		if restart {
			m.mu.Unlock()
			p.SeekTime(time.Duration(inPoint*float64(time.Second)), gst.SeekFlagFlush)
			p.SetState(gst.StatePlaying)
			m.bump()
			return
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
	m.mu.Lock()
	pos := m.cuePos
	cb := m.onCueEnd
	m.mu.Unlock()
	if pos > 0 && cb != nil {
		go cb(pos)
	}
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
	p.SetState(gst.StatePlaying)
	if inPoint := func() float64 { mgr.mu.Lock(); defer mgr.mu.Unlock(); return mgr.inPoint }(); inPoint > 0 {
		// Seek to the in-point once dynamic pads have linked and the clock
		// is running; a seek during pre-roll is usually dropped.
		go func() {
			time.Sleep(200 * time.Millisecond)
			mgr.mu.Lock()
			if mgr.pipeline != p {
				mgr.mu.Unlock()
				return
			}
			mgr.mu.Unlock()
			p.SeekTime(time.Duration(inPoint*float64(time.Second)), gst.SeekFlagFlush)
		}()
	}
	if outPoint := func() float64 { mgr.mu.Lock(); defer mgr.mu.Unlock(); return mgr.outPoint }(); outPoint > 0 {
		go watchTrim(p)
	}
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			mgr.handleEnd(p)
			return false
		case gst.MessageError:
			gerr := msg.ParseError()
			logs.Printf(logs.GSPPipeDebug, "gsp: pipeline error debug: %s", gerr.DebugString())
			logs.Printf(logs.GSPPipeStopped, "gsp: pipeline stopped: %s", gerr.Error())
			mgr.clearIfCurrent(p)
			return false
		}
		return true
	})
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
			mgr.handleEnd(p)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type pipelineSpec struct {
	isTest      bool
	testPattern string
	filename    string
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
		if isAudio {
			elementNames = []string{"queue", "audioconvert", "audioresample", "volume", "autoaudiosink"}
		} else {
			elementNames = []string{"queue", "videoconvert", "videobalance", "videoscale", "autovideosink"}
		}

		elements, err := gst.NewElementMany(elementNames...)
		if err != nil {
			msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "could not create playback elements", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}
		pipeline.AddMany(elements...)
		gst.ElementLinkMany(elements...)

		// Elements must be synced to the pipeline's state, otherwise they
		// stay in Null state and can't process data.
		for _, e := range elements {
			e.SyncStateWithParent()
		}

		// Retain the audio "volume" element so SetVolume can drive it, and
		// apply the per-cue master gain once an audio branch exists.
		if isAudio {
			mgr.mu.Lock()
			mgr.volumeEl = elements[3]
			gain := dbToGain(mgr.volume)
			mgr.mu.Unlock()
			elements[3].Set("volume", gain)
		}
		// Retain the video "videobalance" element so the fade-to-black ramp can
		// drive its brightness; a fresh clip always starts at full brightness.
		if isVideo {
			mgr.mu.Lock()
			mgr.brightEl = elements[2]
			mgr.mu.Unlock()
			elements[2].Set("brightness", 0.0)
		}

		queue := elements[0]
		sinkPad := queue.GetStaticPad("sink")
		srcPad.Link(sinkPad)
	})

	return pipeline, nil
}
