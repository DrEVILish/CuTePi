package gsp

import (
	"CuTePi/gsp/av1dec"
	"CuTePi/gsp/glwall"
	"CuTePi/gsp/scanout"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
	"CuTePi/logs"
	"CuTePi/media"
	"CuTePi/ws"
)

// manager holds the single active playback pipeline. All access is guarded
// by mu so that concurrent HTTP requests (Play/Load/ShowTest/Stop/Panic)
// can't race on the pipeline handle - the historical cause of "multiple
// pipelines" / "losing reference, can't stop playback" bugs.
// clip is the playback state of one running cue's pipeline. The manager embeds
// the focus cue's clip (its fields read as mgr.pipeline, mgr.cuePos, ...);
// other running cues keep theirs in voices (§6.1.2).
type clip struct {
	pipeline     *gst.Pipeline
	currentFile  string // filename currently loaded, "" if none/test pattern
	liveEndpoint bool
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
	// fadeRamp is the picture fade in progress (fadeIn, FadeAndStop), so
	// applyBrightness can hand the whole fade to the GPU wall; nil when the
	// level is simply set.
	fadeRamp    *levelRamp
	fadeCurve   string
	fitMode     string  // fit|stretch frame fitting ("", fit = letterbox)
	rotation    int     // 0|90|180|270 clockwise degrees
	flip        string  // none|h|v mirror ("", none = off)
	fadeLevel   float64 // shared audio/video envelope, 0..1
	opacity     float64 // cue opacity 0..1 on the KMS wall
	fadeSerial  uint64  // cancels an earlier ramp on the same pipeline
	starting    bool
	brightEl    *gst.Element  // per-video-branch "videobalance" element (last wins)
	brightBusy  bool          // a brightWorker goroutine is applying fadeLevel to brightEl
	still       bool          // the loaded file is a still image: one frame, so brightness needs a re-render to show
	stillDirty  bool          // a brightness change is waiting for the re-render goroutine
	stillBusy   bool          // the re-render goroutine is running (its EOS echoes are not cue ends)
	stillEnded  bool          // the still's genuine end-of-stream has been handled
	cuePos      int           // cue position associated with the active clip (0 = not a cue)
	paused      bool          // operator pause intent: Pause sets it, Play/swap clear it.
	heldEnd     bool          // parked on the last frame by hold (paused is set too)
	testShowing bool          // a test pattern (not a file) is on the wall
	fired       uint64        // fire order (the newest running cue becomes the focus)
	layerAt     string        // where its layer goes when shown: LayerTop|LayerBottom|LayerUnder
	layerRef    *gst.Pipeline // LayerUnder: the cue's layer to go beneath
}

type manager struct {
	clip              // the focus cue
	mu                sync.Mutex
	onEndpointFailure func(pos int, title string, generation uint64)
	version           uint64        // bumped on every client-visible state change/position tick
	onCueEnd          func(pos int) // invoked (in a goroutine) when an active cue reaches its end
	gen               uint64        // bumped on every playback-decision change (load/stop/panic/teardown)
	halts             uint64        // bumped by every operator Stop/Panic call, whether or not a pipeline was running
	loads             uint64        // bumped whenever a new pipeline is installed (swap)
	warm              *gst.Pipeline // prebuilt+prerolled next cue; see Warm. Audio-only callers
	warmFile          string        // only, since a prerolled video pipeline paints its
	warmOpts          LoadOpts      // first frame onto the live wall (realtime-first rule)
	warmBuildGen      uint64        // generation the arm was made under (stale-arm check)
	voices            []*voice      // running cues other than the focus (§6.1.2)
	fireSeq           uint64        // fire order counter (clip.fired)
}

var (
	mgr      manager
	initOnce sync.Once
)

// decoderRankOverrides demote decoders that autoplugging would otherwise
// pick but that fail on this platform, so decodebin falls through to a
// working one. Harmless where an element doesn't exist.
//   - v4l2jpegdec: the Pi's hardware JPEG decoder. Its firmware path is
//     unreliable (buffer-pool activation fails or stalls silently), so JPEG
//     stills and MJPEG video would intermittently never preroll; software
//     jpegdec takes over.
//   - openjpegdec: fails to negotiate JPEG 2000 output on the Pi ("Failed to
//     negociate OpenJPEG data", codec corpus); avdec_jpeg2000 decodes it.
//
// and promote one decoder that is faster than the default:
//   - avdec_vp8 (FFmpeg's VP8) over vp8dec (libvpx, primary): two 1080p60
//     VP8 layers at once on a Pi 4 run at 6 + 6 fps with libvpx (its threads
//     spin against each other) and 56 + 56 with FFmpeg's (h264-pi4
//     OPEN-CODECS.md). WebM with alpha still goes through vp8alphadecodebin,
//     which ranks above both.
var decoderRankOverrides = []string{"v4l2jpegdec:0", "openjpegdec:0", "avdec_vp8:257"}

// applyDecoderRanks sets GST_PLUGIN_FEATURE_RANK before gst.Init reads it,
// appending each override unless the operator's own value already ranks
// that element (theirs wins).
func applyDecoderRanks() {
	cur := os.Getenv("GST_PLUGIN_FEATURE_RANK")
	val := cur
	for _, o := range decoderRankOverrides {
		name, _, _ := strings.Cut(o, ":")
		if strings.Contains(cur, name) {
			continue
		}
		if val != "" {
			val += ","
		}
		val += o
	}
	if val == cur {
		return
	}
	if err := os.Setenv("GST_PLUGIN_FEATURE_RANK", val); err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: setting GST_PLUGIN_FEATURE_RANK: %v", err)
	}
}

func gstInit() {
	initOnce.Do(func() {
		applyDecoderRanks()
		glEnv()
		gst.Init(nil)
		// AV1 through dav1d (gsp/av1dec) when libdav1d is installed.
		if av1dec.Register() {
			logs.Printf(logs.GSPPipeDebug, "gsp: AV1 decoder: dav1d %s (cutepidav1ddec)", av1dec.Version())
		}
		// Bus watches (EOS/error handling in watchAndPlay/watchWarm) only
		// dispatch on a running GLib main loop — without it a finished cue
		// never fires its end hook and pipeline errors never surface.
		go glib.NewMainLoop(glib.MainContextDefault(), false).Run()
		// Position ticker: while a clip is playing, push one sync per
		// displayed second over the WebSocket so connected clients advance
		// their progress clock without HTTP polling. CurrentPosition bumps
		// the version when the displayed second changes and stays silent
		// while paused/stopped/idle, so the broadcast only fires on a real
		// change.
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

// broadcastSoon sends at most one "sync" broadcast per 100ms window;
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

// bump increments the change counter the Now Playing refresher compares
// against. Called only when something the widget displays (filename, play
// state, position) actually changes.
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

// Halts counts operator Stop/Panic calls. Unlike Generation it is not moved
// by a clip ending or a fade finishing, so a queued action (fade-then-play)
// can tell "the operator halted playback while I waited" from "my own fade
// completed".
func Halts() uint64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.halts
}

// Loads counts pipeline installs: it moves when a new clip takes over, and
// only then.
func Loads() uint64 {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.loads
}

// swap atomically installs newPipeline as the active one under the manager
// lock, then stops/releases the previous pipeline (and any warm slot) after
// the lock is dropped: SetState(NULL) blocks on GStreamer teardown, and
// holding mu through it stalled every API call and the position readout.
// The old pipeline is fully retired before swap returns, so it is never
// still outputting when the caller starts the new one.
func (m *manager) swap(newPipeline *gst.Pipeline, currentFile string, opts LoadOpts) {
	if !opts.KeepBackground {
		stopBackground() // a new playback decision always kills the soundtrack
	}
	m.mu.Lock()
	retire := []*gst.Pipeline{m.takeWarm()} // a warm slot from a previous decision is somebody else's memory
	var outs []*outgoing
	// Keep the running cues (§6.1.2): only on a layered wall, where each
	// has its own layer.
	keep := opts.KeepOthers && stackable()
	layerAt, layerRef := LayerBottom, (*gst.Pipeline)(nil)
	if keep {
		layerAt = opts.Layer
		if layerAt != LayerBottom && layerAt != LayerUnder {
			layerAt = LayerTop
		}
		if layerAt == LayerUnder {
			for _, c := range append([]*clip{&m.clip}, voiceClips(m.voices)...) {
				if c.pipeline != nil && c.pipeline != newPipeline && opts.UnderCuePos > 0 && c.cuePos == opts.UnderCuePos {
					layerRef = c.pipeline
				}
			}
			if layerRef == nil {
				logs.Printf(logs.GSPPipeDebug, "gsp: cue %d: cue %d is not running, layered on top instead", opts.CuePos, opts.UnderCuePos)
				layerAt = LayerTop
			}
		}
		if m.pipeline != nil && m.pipeline != newPipeline {
			m.demoteLocked()
		}
	} else {
		// Every running cue stops: the focus and every voice. A cue with a
		// layer fades away over the new one (which starts underneath) when
		// the new cue crossfades; a live page loads hidden (showLive), so
		// the old cues stay up until the page is ready even on a cut
		// (crossfade 0: they are then removed in one step).
		stopping := voiceClips(m.takeVoicesLocked())
		if m.pipeline != nil && m.pipeline != newPipeline {
			stopping = append(stopping, &m.clip)
		}
		for _, c := range stopping {
			if (opts.Crossfade > 0 || opts.LiveEndpoint) && layerOf(c.pipeline) != nil {
				o := &outgoing{p: c.pipeline, volumeEl: c.volumeEl, gain: c.effectiveGain(),
					level: c.fadeLevel, curve: opts.FadeCurve, done: make(chan struct{}), wait: 5 * time.Second}
				if opts.LiveEndpoint {
					o.wait = liveFrameWait + liveLoadWait // showLive always ends within this
				}
				outs = append(outs, o)
			} else {
				retire = append(retire, c.pipeline)
			}
		}
	}
	m.clearPlayback()
	m.fireSeq++
	m.fired = m.fireSeq
	m.layerAt, m.layerRef = layerAt, layerRef
	m.pipeline = newPipeline
	m.currentFile = currentFile
	m.liveEndpoint = opts.LiveEndpoint
	m.cuePos = opts.CuePos // with the pipeline, so no reader sees a cue-less load
	m.still = isStillFile(currentFile)
	m.stillEnded = false
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
	m.opacity = opacityOf(opts)
	m.starting = true
	m.paused = false
	m.gen++
	m.loads++
	m.mu.Unlock()
	for _, o := range outs {
		awaitIncoming(newPipeline, o)
		go fadeOutgoing(o, opts.Crossfade)
	}
	retireAll(retire...)
	broadcastSoon()
}

// voiceClips is the voices' clips.
func voiceClips(vs []*voice) []*clip {
	out := make([]*clip, 0, len(vs))
	for _, v := range vs {
		out = append(out, &v.clip)
	}
	return out
}

// clearPlayback resets every mutable playback field except the pipeline
// handle itself (the caller decides the pipeline's fate) and bumps the
// version so clients re-render. Callers must hold m.mu.
func (m *manager) clearPlayback() {
	m.currentFile = ""
	m.liveEndpoint = false
	m.still = false
	m.stillEnded = false
	m.inPoint = 0
	m.outPoint = 0
	m.hold = false
	m.loop = false
	m.loopRemain = 0
	m.volumeEl = nil
	m.brightEl = nil
	m.panEl = nil
	m.starting = false
	m.paused = false
	m.heldEnd = false
	m.testShowing = false
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
		m.pipeline = nil
		m.clearPlayback()
		m.gen++
		// The newest remaining cue takes over the focus (§6.1.2).
		promoted := m.promoteLocked()
		m.mu.Unlock()
		retirePipeline(p) // outside mu: teardown blocks
		if !promoted {
			blankWall(eosBlankDelay)
		}
		broadcastSoon()
		return
	}
	m.mu.Unlock()
	m.voiceFailed(p) // a voice: out of the stack
}

func (m *manager) endpointFailed(p *gst.Pipeline) {
	m.mu.Lock()
	if m.voiceIndexLocked(p) >= 0 {
		// A live page on a lower layer: out of the stack, no recovery.
		m.mu.Unlock()
		m.voiceFailed(p)
		return
	}
	if m.pipeline != p || !m.liveEndpoint {
		m.mu.Unlock()
		return
	}
	pos, title, cb := m.cuePos, strings.TrimPrefix(m.currentFile, "Live: "), m.onEndpointFailure
	m.mu.Unlock()
	m.clearIfCurrent(p)
	if cb != nil {
		go cb(pos, title, Generation())
	}
}

// LoadOpts describes how a clip should play: an optional in/out trim window
// (seconds), whether the last frame is held on-screen when it ends, whether
// the clip loops back to its in-point at end-of-stream, and the per-cue
// master audio gain in dB (0 = 0dB). Playback volume is per-cue, never global.
type LoadOpts struct {
	CuePos         int     // cue sheet position this load plays (0 = direct playback)
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
	// WarmPreroll marks the load as prewarmable: a Warm() slot for it may
	// preroll with the video branch on fakesink (nothing displayed, decoders
	// primed) and swap the real wall sink in at activation. Set by the opts
	// builder so Warm-arms and their later fire compare equal by construction.
	WarmPreroll  bool
	LiveEndpoint bool
	// Wall layer (KMS): opacity 0..1 (0 = unset = fully opaque) and the
	// picture's box on the display — each "" (fill), pixels, or "N%".
	Opacity                    float64
	GeomX, GeomY, GeomW, GeomH string
	// Crop per edge of the source picture: "", pixels, or "N%".
	CropL, CropR, CropT, CropB string
	// Crossfade: when > 0 (ms), whatever is on screen fades out OVER this
	// cue instead of being cut: the new cue starts at once on a lower layer.
	Crossfade int
	// KeepOthers: the running cues carry on and this cue joins the stack at
	// Layer (LayerTop, LayerBottom, or LayerUnder the running cue at
	// UnderCuePos; Top if that cue is not running). Without it every running
	// cue is stopped (cut, or faded over Crossfade). DESIGN §6.1.2.
	KeepOthers  bool
	Layer       string
	UnderCuePos int
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
	mgr.mu.Lock()
	mgr.paused = false
	mgr.heldEnd = false
	mgr.mu.Unlock()
	_, state := p.GetState(gst.StateNull, 0)
	if state == gst.StateNull {
		if err := startPlayback(p); err != nil {
			logs.Printf(logs.GSPPipeStopped, "gsp: resume failed: %v", err)
		}
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

	setStateIfCurrent(p, gst.StatePlaying)
	resumeVoices()
	mgr.bump()
}

// resumeVoices resumes the running cues other than the focus that a Pause
// paused (Pause and Play act on every running cue, §6.1.2); a cue parked on
// its last frame by hold stays parked.
func resumeVoices() {
	mgr.mu.Lock()
	var ps []*gst.Pipeline
	for _, v := range mgr.voices {
		if v.paused && !v.heldEnd {
			v.paused = false
			ps = append(ps, v.pipeline)
		}
	}
	mgr.mu.Unlock()
	for _, vp := range ps {
		vp.SetState(gst.StatePlaying)
	}
}

// pauseVoices pauses every running cue other than the focus.
func pauseVoices() {
	mgr.mu.Lock()
	var ps []*gst.Pipeline
	for _, v := range mgr.voices {
		if !v.paused {
			v.paused = true
			ps = append(ps, v.pipeline)
		}
	}
	mgr.mu.Unlock()
	for _, vp := range ps {
		vp.SetState(gst.StatePaused)
	}
}

// stopVoices takes every running cue other than the focus out of the stack:
// at once, or fading out over fadeMs (picture and sound).
func stopVoices(fadeMs int) {
	mgr.mu.Lock()
	vs := mgr.takeVoicesLocked()
	if len(vs) > 0 {
		mgr.version++
	}
	type fade struct {
		c           clip
		gain, level float64
	}
	var fades []fade
	for _, v := range vs {
		fades = append(fades, fade{v.clip, v.effectiveGain(), v.fadeLevel})
	}
	mgr.mu.Unlock()
	for _, f := range fades {
		if fadeMs > 0 && Layered() {
			go fadeOutgoing(&outgoing{p: f.c.pipeline, volumeEl: f.c.volumeEl, gain: f.gain, level: f.level,
				curve: f.c.fadeCurve, done: make(chan struct{})}, fadeMs)
		} else {
			retirePipeline(f.c.pipeline)
		}
	}
}

// setStateIfCurrent changes p's state and then re-checks that p is still
// the active pipeline. Transport calls read the handle under mu but act on
// it after unlocking (state changes can block, and bus callbacks take mu);
// if a GO from another client swapped the pipeline in that window, the
// retired pipeline would otherwise be resurrected — two pipelines on the
// output. The loser is sent straight back to NULL.
func setStateIfCurrent(p *gst.Pipeline, state gst.State) error {
	err := p.SetState(state)
	mgr.mu.Lock()
	current := mgr.runningLocked(p) // the focus or a voice (§6.1.2)
	mgr.mu.Unlock()
	if !current {
		_ = p.SetState(gst.StateNull)
		return errPipelineReplaced
	}
	return err
}

// errPipelineReplaced: the pipeline lost the active slot mid-transition.
var errPipelineReplaced = errors.New("gsp: pipeline replaced during state change")

// stateQueryTimeout bounds state reads on the request path: an unbounded
// GetState blocks for as long as an async change (e.g. a slow preroll)
// takes — which can be forever — pinning the calling HTTP handler.
const stateQueryTimeout = 2 * time.Second

func Pause() {
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		return
	}
	if err := setStateIfCurrent(p, gst.StatePaused); err != nil {
		logs.Printf(logs.GSPPauseErr, "gsp: error pausing: %v", err)
	}
	pauseVoices()
	mgr.mu.Lock()
	mgr.heldEnd = false
	mgr.paused = true
	mgr.mu.Unlock()
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
	stateChangeReturn, currentState := p.GetState(gst.StateNull, gst.ClockTime(stateQueryTimeout))
	if stateChangeReturn != gst.StateChangeSuccess {
		logs.Printf(logs.GSPToggleErr, "gsp: toggle pause ignored: pipeline state not settled (%v)", stateChangeReturn)
		return
	}
	var err error
	pausing := false
	switch currentState {
	case gst.StatePlaying:
		err = setStateIfCurrent(p, gst.StatePaused)
		pausing = true
	case gst.StatePaused:
		err = setStateIfCurrent(p, gst.StatePlaying)
	}
	if err != nil {
		logs.Printf(logs.GSPToggleErr, "gsp: error toggling pause: %v", err)
	} else {
		mgr.mu.Lock()
		mgr.paused = pausing
		mgr.mu.Unlock()
		if pausing {
			pauseVoices()
		} else {
			resumeVoices()
		}
	}
	mgr.bump()
}

func Panic() {
	stopBackground()
	retireOutgoing()
	stopVoices(0)
	mgr.mu.Lock()
	mgr.halts++
	retire := []*gst.Pipeline{mgr.pipeline}
	if mgr.pipeline != nil {
		mgr.pipeline = nil
		mgr.clearPlayback()
		mgr.gen++
	} else {
		// No pipeline, but a stale cue association may survive a load
		// that tore down inside itself: clear it so Panic always means
		// "nothing is armed", without bumping the generation (no new
		// decision was taken).
		mgr.currentFile = ""
		mgr.liveEndpoint = false
		mgr.cuePos = 0
		mgr.lastPos = 0
		mgr.starting = false
		mgr.testShowing = false
		mgr.version++
	}
	retire = append(retire, mgr.takeWarm()) // PANIC tears everything down, including the warm slot
	mgr.mu.Unlock()
	retireAll(retire...)
	blankWall(0)
	broadcastSoon()
}

func Stop() {
	stopBackground()
	retireOutgoing()
	stopVoices(0)
	mgr.mu.Lock()
	mgr.halts++
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p == nil {
		// Same stale-association clear as Panic: a load that failed
		// mid-flight can leave a cuePos with no pipeline behind it.
		mgr.mu.Lock()
		mgr.currentFile = ""
		mgr.liveEndpoint = false
		mgr.cuePos = 0
		mgr.lastPos = 0
		mgr.starting = false
		mgr.testShowing = false
		mgr.version++
		mgr.mu.Unlock()
		blankWall(0)
		broadcastSoon()
		return
	}
	if err := p.SetState(gst.StateNull); err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: error stopping: %v", err)
	}
	audioBusRelease(p) // the device is free while stopped; a resume reattaches
	if glOpen {
		glHide(p) // off the wall now; a resume re-attaches after its preroll
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
	mgr.liveEndpoint = false
	mgr.cuePos = 0
	mgr.lastPos = 0
	mgr.starting = false
	mgr.testShowing = false // a stopped pattern is off the wall (Tests button state)
	warm := mgr.takeWarm()
	mgr.gen++
	mgr.version++
	mgr.mu.Unlock()
	retireAll(warm)
	blankWall(0)
	broadcastSoon()
}

// ShowTest loads and plays a GStreamer video-test-pattern.
func ShowTest(pattern string) error {
	gstInit()
	newPipeline, err := buildPipeline(pipelineSpec{isTest: true, testPattern: pattern, overlay: TestOverlay()})
	if err != nil {
		return err
	}
	mgr.swap(newPipeline, "", LoadOpts{})
	mgr.mu.Lock()
	mgr.testShowing = true
	mgr.mu.Unlock()
	return watchAndPlay(newPipeline)
}

// TestShowing reports whether a test pattern (rather than a file) is
// currently on the wall. Drives the Tests toggle button state.
func TestShowing() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.testShowing && mgr.pipeline != nil
}

// DirectOpts are the load options for playing filename straight from the
// media pool (no cue): the configured loop default; a still holds until
// Stop/Panic; an animated image repeats as the file says (GIF/APNG/WebP
// loop count) and then holds its last frame, as a browser shows it.
func DirectOpts(filename string, gain float64) LoadOpts {
	o := LoadOpts{Loop: config.Loop(), LoudnessGain: gain, Hold: isStillFile(filename)}
	if a := animationOf(filename); a.Animated {
		switch {
		case a.LoopForever:
			o.Loop, o.LoopCount = true, 0
		case !o.Loop && a.Loops > 1:
			o.Loop, o.LoopCount = true, a.Loops
		}
		o.Hold = true
	}
	return o
}

// LoadWithOpts loads filename with an optional trim window and hold policy.
// A warm slot matching file+opts (see Warm) activates instead of a fresh
// build+preroll — the ~0-latency cue path; anything else falls back to the
// normal build (and drops the stale slot). Both paths time their build+swap
// so the log shows what a cue actually cost (GSP-E161), warm or cold.
func LoadWithOpts(filename string, opts LoadOpts) error {
	gstInit()
	t0 := time.Now()
	if InstallWarm(filename, opts) {
		logs.Printf(logs.GSPFireTiming, "cue load %q: warm swap %.1fms", filename, time.Since(t0).Seconds()*1000)
		return nil
	}
	mgr.mu.Lock()
	warm := mgr.takeWarm()
	mgr.mu.Unlock()
	retireAll(warm)
	newPipeline, err := buildPipeline(pipelineSpec{isTest: false, filename: filename, opts: opts})
	if err != nil {
		return err
	}
	buildMS := time.Since(t0).Seconds() * 1000
	mgr.swap(newPipeline, filename, opts)
	start := time.Now()
	if err := watchAndPlay(newPipeline); err != nil {
		return fmt.Errorf("loading %q: %w", filename, err)
	}
	logs.Printf(logs.GSPFireTiming, "cue load %q: build %.1fms + preroll %.1fms",
		filename, buildMS, time.Since(start).Seconds()*1000)
	return nil
}

// LoadEndpointWithOpts renders a web page through the WPE GStreamer source
// into the appliance's existing HDMI wall sink.
func LoadEndpointWithOpts(title, endpointURL string, opts LoadOpts) error {
	gstInit()
	if !strings.HasPrefix(endpointURL, "http://") && !strings.HasPrefix(endpointURL, "https://") {
		return fmt.Errorf("endpoint URL must use http or https")
	}
	// A live page is picture only, never ends by itself and has no media
	// timeline: none of the file-only playback options apply.
	opts.LiveEndpoint = true
	opts.InPoint, opts.OutPoint, opts.Rate = 0, 0, 1
	opts.Hold, opts.Loop, opts.LoopCount = false, false, 0
	opts.WarmPreroll = false
	p, err := buildPipeline(pipelineSpec{endpointURL: endpointURL, liveTitle: title, opts: opts})
	if err != nil {
		return err
	}
	mgr.swap(p, "Live: "+title, opts)
	if err := watchAndPlay(p); err != nil {
		logs.Printf(logs.GSPPipeStopped, "gsp: live page %q: %v", endpointURL, err)
		return fmt.Errorf("loading live page %q: %w", endpointURL, err)
	}
	return nil
}

func SetEndpointFailureHook(cb func(pos int, title string, generation uint64)) {
	mgr.mu.Lock()
	mgr.onEndpointFailure = cb
	mgr.mu.Unlock()
}

func FailEndpoint(generation uint64) {
	mgr.mu.Lock()
	p := mgr.pipeline
	current := mgr.liveEndpoint && mgr.gen == generation && p != nil
	mgr.mu.Unlock()
	if current {
		mgr.endpointFailed(p)
	}
}

// takeWarm empties the warm slot and returns its pipeline (nil if none) for
// the caller to retire AFTER releasing mu. Caller holds m.mu.
func (m *manager) takeWarm() *gst.Pipeline {
	p := m.warm
	m.warm = nil
	m.warmFile = ""
	m.warmOpts = LoadOpts{}
	return p
}

// retireAll retires every non-nil pipeline. Never call it holding mgr.mu.
func retireAll(ps ...*gst.Pipeline) {
	for _, p := range ps {
		if p != nil {
			retirePipeline(p)
		}
	}
}

// Warm prebuilds and prerolls the NEXT cue into a slot (deck-style double
// buffer), so a later LoadWithOpts with the same file+opts activates it
// instead of rebuilding. Audio cues preroll silently on the real audio
// sink; video/image cues preroll on fakesink (spec.warmSink) and get the
// wall sink relinked at activation — nothing is ever displayed by a warm
// slot. A stale arm (a newer playback decision landed meanwhile) is
// retired, never installed.
func Warm(file string, opts LoadOpts) error {
	gstInit()
	if glOpen {
		return nil // GL wall: prewarm not wired yet; cold builds are quick
	}
	mgr.mu.Lock()
	buildGen := mgr.gen
	stale := mgr.takeWarm()
	mgr.mu.Unlock()
	retireAll(stale)
	p, err := buildPipeline(pipelineSpec{isTest: false, filename: file, warmSink: opts.WarmPreroll, opts: opts})
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
	var displaced *gst.Pipeline
	if current {
		displaced = mgr.takeWarm() // a concurrent Warm may have filled it
		mgr.warm = p
		mgr.warmFile = file
		mgr.warmOpts = opts
	}
	mgr.mu.Unlock()
	retireAll(displaced)
	if !current {
		// A newer playback decision landed while we built: this arm is
		// stale, so it is retired rather than installed.
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
			var dead *gst.Pipeline
			if mgr.warm == p {
				dead = mgr.takeWarm()
			}
			mgr.mu.Unlock()
			retireAll(dead)
			return false
		}
		return true
	})
}

// InstallWarm activates the prewarmed pipeline for file+opts if the slot is
// still the one Warm built: retire the current transport, install the warm
// pipeline, play. False = stale/mismatched slot, caller falls back.
func InstallWarm(file string, opts LoadOpts) bool {
	if glOpen {
		return false
	}
	mgr.mu.Lock()
	p, ok := mgr.warm, mgr.warmFile == file && mgr.warmOpts == opts
	if ok {
		mgr.warm, mgr.warmFile, mgr.warmOpts = nil, "", LoadOpts{} // claim before retiring anything else
	}
	mgr.mu.Unlock()
	if !ok {
		return false
	}
	// Warm builds preroll on fakesinks; relink the real audio device and
	// wall sink in before anything plays. A failed relink is a
	// caller-handled rebuild.
	audioWired, aok := warmWireAudio(p)
	if !aok || (opts.WarmPreroll && !warmWireVideo(p)) {
		logs.Printf(logs.GSPWarm, "warm pipeline output relink failed: cold load")
		retirePipeline(p)
		return false
	}
	// The relinked sink must re-preroll before the swap: the flush seek
	// re-drives the decoders, and on big files that can exceed the fire
	// budget while a cold load lands in ~0.5s. Bound it — a slow slot
	// falls back to cold instead of stalling the GO past a second.
	if (opts.WarmPreroll || audioWired) && !warmReady(p, 400*time.Millisecond) {
		logs.Printf(logs.GSPWarm, "warm pipeline re-preroll slow: cold load")
		retirePipeline(p)
		return false
	}
	logs.Printf(logs.GSPWarm, "warm pipeline activated: %s", file)
	// swap() owns the retire+install; give it the prerolled handle.
	mgr.swap(p, file, opts)
	if err := watchAndPlay(p); err != nil {
		// startPlayback already tore the slot down; report a miss so the
		// caller retries with a cold build instead of claiming success.
		logs.Printf(logs.GSPPipeStopped, "gsp: warm pipeline failed to start %q: %v (cold load)", file, err)
		return false
	}
	return true
}

// warmReady waits (bounded) for the pipeline to sit prerolled in PAUSED.
func warmReady(p *gst.Pipeline, timeout time.Duration) bool {
	result, _ := p.GetState(gst.StateNull, gst.ClockTime(timeout))
	return result == gst.StateChangeSuccess
}

// wallVideoSink names the live video-output element: CUTEPI_WALL_SINK when
// set, autovideosink otherwise. Single source so the cold build and the
// warm-slot relink can never disagree. fbdevsink is the working pick on a
// headless KMS console (autovideosink's GL/GBM choice prerolls but never
// presents there); fakesink keeps headless tests display-free.
func wallVideoSink() string {
	if kind := os.Getenv("CUTEPI_WALL_SINK"); kind != "" {
		return kind
	}
	return "autovideosink"
}

// videoStem is the shared video-branch plumbing (fresh slice every call —
// callers append their own tail): queue, an optional DMABuf download,
// then convert/balance/scale, two videoflip stages (rotate, then mirror;
// method=none passes frames through untouched), and a second videoconvert
// after the flips (videoflip handles only a subset of raw formats, so
// without a converter on the sink side a picky wall sink such as
// fbdevsink's RGB16 framebuffer cannot negotiate at all — decodebin then
// fails delayed linking and the cue silently never plays; passthrough
// cost when formats already match).
//
// download is for DMABuf-producing file decodes only: hardware decoders
// are the sole DMABuf source, while the test-pattern source (videotestsrc,
// system memory) negotiates worse with v4l2convert in the chain (its
// device caps probe can fail the query and leave the source unlinked —
// silent no-pattern). Callers pass dmaBufUpstream(srcPad) for files.
func videoStem(download bool) []string {
	stem := []string{"queue"}
	if download {
		stem = append(stem, videoDownload()...)
	}
	return append(stem, "videoconvert", "videobalance", "videoscale", "videoflip", "videoflip", "videoconvert")
}

// videoDownload names an optional DMABuf download stage for hardware
// decoders: Pi v4l2h264dec emits DMA_DRM buffers that videoconvert cannot
// map, stalling the chain with no error and no EOS. Probed once, guarded
// by availability so non-V4L2 systems build the same chain as before.
var (
	videoDownloadOnce sync.Once
	videoDownloadEls  []string
)

func videoDownload() []string {
	videoDownloadOnce.Do(func() {
		if el, err := gst.NewElement("v4l2convert"); err == nil && el != nil {
			videoDownloadEls = []string{"v4l2convert"}
		}
	})
	return videoDownloadEls
}

func indexOfName(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

func firstByFactory(m map[string][]*gst.Element, factory string) *gst.Element {
	if els := m[factory]; len(els) > 0 {
		return els[0]
	}
	return nil
}

// dmaBufUpstream reports whether the just-linked decodebin pad already
// carries DMABuf memory, i.e. a hardware decoder produced it: only then is
// the v4l2convert download stage useful. Probing the stage for
// system-memory streams breaks negotiation (its device caps query can fail
// and sink the whole link — silent no-play for software-decoded files),
// so the stage is conditional on the actual upstream caps, not structural.
// Unreadable caps default to including it (hardware-first).
func dmaBufUpstream(srcPad *gst.Pad) bool {
	if srcPad == nil || !srcPad.HasCurrentCaps() {
		return true
	}
	caps := srcPad.GetCurrentCaps()
	if caps == nil {
		return true
	}
	// No Unref: go-gst owns the reference via a finalizer; unrefing here
	// double-frees (gst_mini_object_unref assertion failures in the log).
	for i := 0; i < caps.GetSize(); i++ {
		if f := caps.GetFeaturesAt(i); f != nil && f.Contains("memory:DMABuf") {
			return true
		}
	}
	return false
}

// warmWireVideo swaps the prerolled video branch's output edge from
// fakesink to the live wall: unlink the fake sink, install the wall sink,
// sync it, done — the pipeline is parked PAUSED, and startPlayback's
// SetState(Playing) prerolls the new sink (~a frame) with decoder context
// already warm. False = un-recoverable wiring state; caller cold-loads.
func warmWireVideo(p *gst.Pipeline) bool {
	tail, _ := p.GetElementByName("warm-vf-tail")
	fake, _ := p.GetElementByName("warm-video-sink")
	if tail == nil || fake == nil {
		return true // not a video preroll (audio slot): nothing to relink
	}
	// Wall sink shared with the cold build (see wallVideoSink): the env
	// override is a stage-side knob (fbdevsink on headless KMS consoles,
	// fakesink/xv for video-out debugging) and lets tests run
	// naming/relink without a display.
	kind := wallVideoSink()
	sink, err := gst.NewElement(kind)
	if err != nil {
		return false
	}
	p.AddMany(sink)
	if src := tail.GetStaticPad("src"); src != nil {
		src.Unlink(fake.GetStaticPad("sink"))
		if src.Link(sink.GetStaticPad("sink")) != gst.PadLinkOK {
			sink.SetState(gst.StateNull)
			p.Remove(sink)
			return false
		}
	} else {
		sink.SetState(gst.StateNull)
		p.Remove(sink)
		return false
	}
	sink.Set("sync", false)
	sink.SyncStateWithParent()
	p.Remove(fake)
	fake.SetState(gst.StateNull)
	// The old fake sink already consumed the preroll buffer, which blocks the
	// re-preroll of the new sink forever (5s stall observed). A flush seek
	// re-prerolls the swapped-in sink from the top; decoder context and all
	// elements are warm, so it lands quick.
	p.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush)
	return true
}

// warmWireAudio swaps the warm slot's audio fakesink for the real output
// sink, tuned exactly like a cold build's (device, rate, channels). wired is
// false when the slot has no audio branch. When there is no video branch to
// relink, it also flush-seeks so the new sink prerolls (a fakesink that
// already consumed the preroll buffer would otherwise starve it).
func warmWireAudio(p *gst.Pipeline) (wired, ok bool) {
	tail, _ := p.GetElementByName("warm-af-tail")
	fake, _ := p.GetElementByName("warm-audio-sink")
	if tail == nil || fake == nil {
		return false, true
	}
	sink, err := audioBusInput(p) // the audio bus, as a cold build
	if err != nil {
		logs.PrintfWarn(logs.GSPPipeDebug, "gsp: warm slot audio: %v", err)
		return false, false
	}
	p.AddMany(sink)
	fail := func() (bool, bool) {
		sink.SetState(gst.StateNull)
		p.Remove(sink)
		audioBusRelease(p)
		return false, false
	}
	src := tail.GetStaticPad("src")
	if src == nil {
		return fail()
	}
	src.Unlink(fake.GetStaticPad("sink"))
	if src.Link(sink.GetStaticPad("sink")) != gst.PadLinkOK {
		return fail()
	}
	sink.SyncStateWithParent()
	p.Remove(fake)
	fake.SetState(gst.StateNull)
	if v, _ := p.GetElementByName("warm-vf-tail"); v == nil {
		p.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush)
	}
	return true, true
}

// linkExtraStream drains a secondary audio/video stream into a fakesink.
// decodebin recreates pads after Stop -> Play, so an existing drain (named by
// stream id) is relinked instead of rebuilt.
func linkExtraStream(pipeline *gst.Pipeline, srcPad *gst.Pad, kind, streamID string) {
	name := "extra-" + kind + "-" + streamID
	if el, err := pipeline.GetElementByName(name); err == nil && el != nil {
		if ret := srcPad.Link(el.GetStaticPad("sink")); ret != gst.PadLinkOK {
			logs.PrintfWarn(logs.GSPPipeDebug, "gsp: relinking extra %s stream drain: %v", kind, ret)
		}
		return
	}
	sink, err := gst.NewElement("fakesink")
	if err != nil {
		logs.PrintfWarn(logs.GSPPipeDebug, "gsp: extra %s stream drain: %v", kind, err)
		return
	}
	sink.Set("name", name)
	// sync=true: the drained track keeps real time. Unsynced it ran as fast
	// as the demuxer fed it, and the pipeline's position (the furthest sink)
	// leapt ~20 s ahead on files with a second audio track — a jumping
	// clock, a lying scrubber and early trim-out.
	sink.Set("sync", true)
	sink.Set("async", false)
	if err := pipeline.AddMany(sink); err != nil {
		logs.PrintfWarn(logs.GSPPipeDebug, "gsp: extra %s stream drain: %v", kind, err)
		return
	}
	sink.SyncStateWithParent()
	if ret := srcPad.Link(sink.GetStaticPad("sink")); ret != gst.PadLinkOK {
		// An undrained stream stalls the whole pipeline (not-linked): say why.
		logs.PrintfWarn(logs.GSPPipeDebug, "gsp: linking extra %s stream drain: %v", kind, ret)
	}
}

// failLink fails pipeline when a link in its decoded-stream tail did not
// take: unchecked, such a cue played on with no picture or no sound and no
// error anywhere. The error goes to the bus, where the watch logs it and
// tears the pipeline down like any other pipeline error.
func failLink(pipeline *gst.Pipeline, from *gst.Element, err error) {
	logs.PrintfWarn(logs.GSPPipeStopped, "gsp: %v", err)
	pipeline.GetPipelineBus().Post(gst.NewErrorMessage(from, gst.NewGError(4, err), err.Error(), nil))
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
	live := mgr.liveEndpoint
	mgr.mu.Unlock()
	if p == nil || live {
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
	live := mgr.liveEndpoint
	mgr.mu.Unlock()
	if p == nil || live {
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

// IsPaused reports the operator pause intent (Pause sets it, Play/swap clear
// it, a hold-parked end sets it). Intent, not a live state query: a polled
// GetState races transitions (fresh Play still reads PAUSED) and can block
// on a wedged bus. False when nothing is loaded.
func IsPaused() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.paused && mgr.pipeline != nil
}

// HeldAtEnd reports a clip parked on its last frame by hold (a still, or a
// video with hold at end): IsPaused is true then too, but no operator paused.
func HeldAtEnd() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.heldEnd && mgr.pipeline != nil
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
func SetLoop(loop bool) error {
	mgr.mu.Lock()
	mgr.loop = loop
	mgr.mu.Unlock()
	err := config.SetLoop(loop)
	mgr.bump()
	return err
}

// SetClipLoop toggles loop-at-end for the currently loaded clip only; the
// persisted default is untouched (a deck's "play: loop:" is per playback).
func SetClipLoop(loop bool) {
	mgr.mu.Lock()
	changed := mgr.pipeline != nil && mgr.loop != loop
	if changed {
		mgr.loop = loop
	}
	mgr.mu.Unlock()
	if changed {
		mgr.bump()
	}
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
	stopVoices(durMs) // every running cue fades out together (§6.1.2)
	// Level from elapsed time: the fade takes exactly durMs however long
	// each apply takes.
	total := time.Duration(durMs) * time.Millisecond
	start := time.Now()
	for {
		time.Sleep(fadeTick)
		t := math.Min(1, float64(time.Since(start))/float64(total))
		mgr.mu.Lock()
		if mgr.pipeline != p || mgr.gen != gen || mgr.fadeSerial != serial {
			mgr.mu.Unlock()
			return
		}
		mgr.fadeLevel = startLevel * (1 - fadeShape(fadeCurve, t))
		mgr.fadeRamp = &levelRamp{from: startLevel, to: 0, start: start, dur: total, curve: fadeCurve}
		mgr.applyGain()
		mgr.applyBrightness()
		mgr.fadeRamp = nil
		mgr.mu.Unlock()
		if t >= 1 {
			break
		}
	}
	if Layered() {
		time.Sleep(fadeLand()) // the last alpha lands before the plane goes
	}
	mgr.clearIfCurrent(p)
}

// levelRamp is a fade of the picture level: from -> to over dur along curve,
// started at start.
type levelRamp struct {
	from, to float64
	start    time.Time
	dur      time.Duration
	curve    string
}

// Caller holds mu; live changes and both fades share the same effective gain.
func (m *clip) effectiveGain() float64 {
	if m.mute || m.volume <= -60 {
		return 0
	}
	return dbToGain(dbClamp(m.volume+m.loudnessGain)) * m.fadeLevel
}

// applyBrightness drives the video fade level. A still is a single buffer
// the sink has already shown, so a new brightness would never reach the
// screen: re-render it. Caller holds mu.
// applyLayerLevel applies a voice's fade level to its layer (voices only run
// on layered walls). Caller holds mgr.mu.
func (c *clip) applyLayerLevel() {
	if r := c.fadeRamp; r != nil {
		setLayerRamp(c.pipeline, r.from, r.to, r.start, r.dur, r.curve)
	} else {
		setLayerLevel(c.pipeline, c.fadeLevel)
	}
}

func (m *manager) applyBrightness() {
	if kmsWall() != nil {
		// Plane alpha over the black primary: a true fade (colours scale,
		// never shift), no re-render, applied at the next vblank. A fade in
		// progress goes to the wall whole (the GPU wall steps it per frame).
		if r := m.fadeRamp; r != nil {
			setLayerRamp(m.pipeline, r.from, r.to, r.start, r.dur, r.curve)
		} else {
			setLayerLevel(m.pipeline, m.fadeLevel)
		}
		return
	}
	if m.brightEl == nil {
		return
	}
	if !m.still {
		if !m.brightBusy {
			m.brightBusy = true
			go m.brightWorker()
		}
		return
	}
	m.brightEl.Set("brightness", m.fadeLevel-1)
	if m.pipeline == nil {
		return
	}
	m.stillDirty = true
	if m.stillBusy {
		return
	}
	m.stillBusy = true
	go m.rerenderStill(m.pipeline)
}

// brightWorker applies the latest fade level to the video branch off the fade
// clock. Setting videobalance's brightness blocks on the element's stream lock
// for as long as a frame is being pushed downstream (hundreds of ms on a 1080p
// clip running ahead of the sink), and doing it inline under mu stretched a
// 500ms ESC fade to ~2s and stalled every status reader behind it. The audio
// ramp and the fade deadline stay on time; video follows as fast as frames flow.
func (m *manager) brightWorker() {
	var lastEl *gst.Element
	var last float64
	for {
		m.mu.Lock()
		el, v := m.brightEl, m.fadeLevel-1
		if el == nil || (el == lastEl && v == last) {
			m.brightBusy = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		el.Set("brightness", v)
		lastEl, last = el, v
	}
}

// rerenderStill flush-seeks a still back to its only frame whenever the fade
// level changed, paced by how fast the frame decodes. The seek makes the
// source emit its frame (and an EOS) again; stillBusy keeps that echo from
// counting as the cue ending a second time.
func (m *manager) rerenderStill(p *gst.Pipeline) {
	for {
		m.mu.Lock()
		if !m.stillDirty || m.pipeline != p {
			m.stillDirty = false
			m.mu.Unlock()
			time.Sleep(150 * time.Millisecond) // let the last echo EOS land
			m.mu.Lock()
			if !m.stillDirty || m.pipeline != p {
				m.stillBusy = false
				m.mu.Unlock()
				return
			}
		}
		m.stillDirty = false
		m.mu.Unlock()
		p.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush|gst.SeekFlagAccurate)
		// Wait for the sink to preroll the frame: a new flush before the
		// decode finishes cancels it, and a ramp faster than the decode
		// time would show nothing until the last step.
		p.GetState(gst.StateNull, gst.ClockTime(time.Second))
	}
}

// isStillFile reports whether name is a single-frame image (the same
// extension list import uses, media.KindFromExtension). An animated GIF,
// APNG or WebP is not a still: it plays as a timeline, so none of the
// single-frame shortcuts (re-render on fades, infinite hold, armed panic
// image) apply to it (§6.1.3).
func isStillFile(name string) bool {
	return media.KindFromExtension(name) == media.KindImage && !animationOf(name).Animated
}

// animationOf reads the animation header of a media-pool file (microseconds:
// header only).
func animationOf(name string) media.Animation {
	if media.KindFromExtension(name) != media.KindImage {
		return media.Animation{}
	}
	return media.ImageAnimation(filepath.Join(config.MediaLocation(), name))
}

// IsAnimated reports whether filename is an animated image (GIF, APNG, WebP).
func IsAnimated(filename string) bool { return animationOf(filename).Animated }

func (m *clip) applyGain() {
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
	c0 := mgr.clipOfLocked(p)
	if c0 == nil {
		mgr.mu.Unlock()
		return
	}
	serial, duration := c0.fadeSerial, time.Duration(c0.fadeIn)*time.Millisecond
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
		onWall := !glOpen || glOnWall(p)
		mgr.mu.Lock()
		// The focus, or the voice it became when a cue that keeps the
		// others running was fired (§6.1.2): the fade carries on there. A
		// newer fade (a restart, ESC) bumps the serial.
		c := mgr.clipOfLocked(p)
		if c == nil || c.fadeSerial != serial {
			mgr.mu.Unlock()
			return
		}
		// A still ends (and holds, paused) the moment its one frame is
		// out, so its fade clock cannot wait for PLAYING.
		// On the GPU wall the clock also waits for the layer to be shown.
		advancing := (state == gst.StatePlaying || c.still) && onWall
		if advancing {
			elapsed += delta
		}
		c.fadeLevel = fadeShape(c.fadeCurve, math.Min(1, float64(elapsed)/float64(duration)))
		if advancing {
			// Started `elapsed` ago: the wall steps the rest per frame. A
			// held fade (paused, not on the wall yet) is a plain level.
			c.fadeRamp = &levelRamp{from: 0, to: 1, start: now.Add(-elapsed), dur: duration, curve: c.fadeCurve}
		}
		c.applyGain()
		if c == &mgr.clip {
			mgr.applyBrightness()
		} else {
			c.applyLayerLevel()
		}
		c.fadeRamp = nil
		done := c.fadeLevel == 1
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

// dbClamp clamps a dB value to the Cue Inspector slider's range.
func dbClamp(v float64) float64 {
	return math.Max(-60, math.Min(12, v))
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

// SetCueVolume applies a live gain (dB) to the playing pipeline and reports
// the clamped value and the cue position that pipeline plays, read under
// the same lock. The caller persists the value on exactly that cue: reading
// CurrentCuePos separately could save it to a cue fired in between (0 when
// the clip has no cue).
func SetCueVolume(v float64) (applied float64, cuePos int) {
	mgr.mu.Lock()
	if !math.IsNaN(v) {
		mgr.volume = dbClamp(v)
		mgr.applyGain()
	}
	applied, cuePos = mgr.volume, mgr.cuePos
	mgr.mu.Unlock()
	mgr.bump()
	return applied, cuePos
}

// CueMix is a cue's live mix: what the Cue Inspector re-applies after a
// save while the cue plays.
type CueMix struct {
	Mute    bool
	Volume  float64 // dB
	Balance float64
	Rate    float64
}

// ApplyCueMix applies mix to the playing pipeline only if it still plays
// cuePos, checked under the lock that applies it, so a cue fired meanwhile
// never receives another cue's settings. Reports whether it applied.
func ApplyCueMix(cuePos int, mix CueMix) bool {
	mgr.mu.Lock()
	if cuePos <= 0 || mgr.cuePos != cuePos || mgr.pipeline == nil {
		mgr.mu.Unlock()
		return false
	}
	mgr.mute = mix.Mute
	if !math.IsNaN(mix.Volume) {
		mgr.volume = dbClamp(mix.Volume)
	}
	mgr.applyGain()
	if !math.IsNaN(mix.Balance) && !math.IsInf(mix.Balance, 0) {
		mgr.balance = math.Max(-1, math.Min(1, mix.Balance))
		if mgr.panEl != nil {
			mgr.panEl.Set("panorama", float32(mgr.balance))
		}
	}
	rate := clampRate(mix.Rate)
	rateChanged := mgr.rate != rate
	mgr.rate = rate
	p, starting := mgr.pipeline, mgr.starting
	mgr.mu.Unlock()
	mgr.bump()
	if rateChanged && !starting {
		// As SetRate; seekAtRate skips the seek if p was replaced meanwhile.
		p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
		if ok, pos := p.QueryPosition(gst.FormatTime); ok {
			seekAtRate(p, float64(pos)/1e9)
		}
	}
	return true
}

// Every seek carries the segment rate, including loop and trim restarts.
func seekAtRate(p *gst.Pipeline, seconds float64) bool {
	mgr.mu.Lock()
	c := mgr.clipOfLocked(p) // the focus or a voice (§6.1.2)
	current, rate := c != nil, 1.0
	if c != nil {
		rate = c.rate
	}
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
	live := mgr.liveEndpoint
	inPoint := mgr.inPoint
	outPoint := mgr.outPoint
	mgr.mu.Unlock()
	if p == nil || live {
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
	if m.liveEndpoint {
		m.mu.Unlock()
		m.endpointFailed(p)
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
			setStateIfCurrent(p, gst.StatePlaying)
			m.mu.Lock()
			m.paused = false
			m.mu.Unlock()
			m.bump()
			return true
		}
	}
	m.mu.Unlock()

	if hold {
		setStateIfCurrent(p, gst.StatePaused)
		m.mu.Lock()
		m.paused = true
		m.heldEnd = true
		m.mu.Unlock()
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
func watchAndPlay(p *gst.Pipeline) error {
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		// Identity precheck: if this pipeline was retired (replaced/stopped),
		// unregister the watch by returning false. Without this the closure
		// - and through it the pipeline and its bus - is pinned forever,
		// because a retired pipeline never reaches EOS or error (the only
		// other ways the watch unregisters). retirePipeline posts a wake-up
		// message so this check actually runs after a retirement.
		mgr.mu.Lock()
		current := mgr.runningLocked(p)
		focus := mgr.pipeline == p
		mgr.mu.Unlock()
		if !current {
			return false
		}
		if !focus {
			// A voice (§6.1.2): its own end, or out of the stack on error.
			switch msg.Type() {
			case gst.MessageEOS:
				return mgr.voiceEnd(p)
			case gst.MessageError:
				logs.Printf(logs.GSPPipeStopped, "gsp: layered cue stopped: %s", msg.ParseError().Error())
				mgr.voiceFailed(p)
				return false
			}
			return true
		}
		switch msg.Type() {
		case gst.MessageEOS:
			mgr.mu.Lock()
			echo := mgr.stillBusy && mgr.stillEnded && mgr.pipeline == p
			if mgr.pipeline == p {
				mgr.stillEnded = true
			}
			mgr.mu.Unlock()
			if echo {
				return true
			}
			mgr.handleEnd(p)
			mgr.mu.Lock()
			current := mgr.pipeline == p
			mgr.mu.Unlock()
			return current
		case gst.MessageElement:
			// wpevideosrc reports its page's load progress; 100 is loaded.
			if st := msg.GetStructure(); st != nil && st.Name() == "wpe-stats" {
				if v, err := st.GetValue("estimated-load-progress"); err == nil {
					if f, ok := v.(float64); ok && f >= 100 {
						liveLoaded.LoadOrStore(p, time.Now())
					}
				}
			}
		case gst.MessageError:
			gerr := msg.ParseError()
			logs.Printf(logs.GSPPipeDebug, "gsp: pipeline error debug: %s", gerr.DebugString())
			logs.Printf(logs.GSPPipeStopped, "gsp: pipeline stopped: %s", gerr.Error())
			mgr.mu.Lock()
			live := mgr.liveEndpoint
			mgr.mu.Unlock()
			if live {
				mgr.endpointFailed(p)
			} else {
				mgr.clearIfCurrent(p)
			}
			return false
		}
		return true
	})
	return startPlayback(p)
}

// prerollTimeout bounds the PAUSED preroll before a clip starts. Large 4K
// files on SD/USB storage can take several seconds to preroll on a Pi, so
// the budget is generous; exceeding it fails the load with an error the
// caller surfaces (cue result, HTTP error) instead of dying silently.
var prerollTimeout = 15 * time.Second

// startPlayback prerolls p, applies the trim/rate seek and starts it. A nil
// error with nothing started means p was superseded meanwhile (not a
// failure: a newer decision owns the output).
func startPlayback(p *gst.Pipeline) error {
	mgr.mu.Lock()
	if mgr.pipeline != p {
		mgr.mu.Unlock()
		return nil
	}
	gen := mgr.gen
	live := mgr.liveEndpoint
	mgr.starting = true
	mgr.fadeLevel = 1
	if mgr.fadeIn > 0 {
		mgr.fadeLevel = 0
	}
	mgr.fadeSerial++
	mgr.applyGain()
	mgr.applyBrightness()
	mgr.mu.Unlock()
	audioBusReattach(p) // a resume after Stop: back on the bus
	// Preroll before seeking: no initial audio/frame leaks before the trim or
	// rate is applied, and slow decoders need no guessed sleep duration.
	p.SetState(gst.StatePaused)
	result, _ := p.GetState(gst.StateNull, gst.ClockTime(prerollTimeout))
	// From here p may be a voice: a cue that keeps the others running was
	// fired while this one prerolled (§6.1.2). It carries on from its own
	// clip.
	mgr.mu.Lock()
	current := mgr.startCurrentLocked(p, gen)
	var inPoint, rate, outPoint float64
	if c := mgr.clipOfLocked(p); c != nil {
		inPoint, rate, outPoint = c.inPoint, c.rate, c.outPoint
	}
	mgr.mu.Unlock()
	if !current {
		return nil
	}
	if result == gst.StateChangeFailure || (result == gst.StateChangeAsync && !live) {
		mgr.clearIfCurrent(p)
		reason := "preroll failed"
		if result == gst.StateChangeAsync {
			reason = fmt.Sprintf("preroll did not finish within %v", prerollTimeout)
		}
		logs.Printf(logs.GSPPipeStopped, "gsp: %s", reason)
		return errors.New("gsp: " + reason)
	}
	if inPoint > 0 || rate != 1 {
		seekAtRate(p, inPoint)
	}
	mgr.mu.Lock()
	c := mgr.clipOfLocked(p)
	if c == nil || !mgr.startCurrentLocked(p, gen) {
		mgr.mu.Unlock()
		return nil
	}
	c.starting = false
	level, layerAt, layerRef := c.fadeLevel, c.layerAt, c.layerRef
	mgr.mu.Unlock()
	if live {
		// A live page has no preroll: its layer appears only after PLAYING
		// brings the first frame (showLive).
		if err := setStateIfCurrent(p, gst.StatePlaying); err != nil {
			incomingShown(p)
			if errors.Is(err, errPipelineReplaced) {
				return nil
			}
			mgr.clearIfCurrent(p)
			return fmt.Errorf("gsp: starting live page: %w", err)
		}
		go showLive(p, gen)
		return nil
	}
	// Prerolled: put the layer on screen at its place in the stack (under
	// anything fading out when it stops the others), and only now let the
	// cues above it start fading away.
	showLayer(p, level, layerAt, layerRef)
	incomingShown(p)
	if err := setStateIfCurrent(p, gst.StatePlaying); err != nil {
		if errors.Is(err, errPipelineReplaced) {
			return nil // superseded while starting; the newer decision owns output
		}
		mgr.clearIfCurrent(p)
		return fmt.Errorf("gsp: starting playback: %w", err)
	}
	go fadeIn(p, gen)
	if outPoint > 0 {
		go watchTrim(p)
	}
	return nil
}

// liveFrameWait bounds how long a live page may take to draw its first
// frame (WebKit start-up) before the cue counts as failed and the live-page
// recovery takes over (DESIGN §12.14).
var liveFrameWait = 20 * time.Second

// liveLoadWait bounds how long a drawing page stays hidden waiting for its
// load to complete: a page that never reports it (endless subresource
// loads) is shown anyway rather than held off the wall.
var liveLoadWait = 15 * time.Second

// livePaintSettle is how long a loaded page keeps rendering hidden before it
// is shown: layout and web fonts often paint a few frames after the load
// event.
var livePaintSettle = 400 * time.Millisecond

// liveLoaded holds, per live pipeline, when its page finished loading
// (wpevideosrc's wpe-stats message at 100%).
var liveLoaded sync.Map // *gst.Pipeline -> time.Time

// showLive puts a live page on screen once it has loaded and painted, then
// runs the cue's fade-in. Until then the page renders on its own layer at
// alpha 0, so the audience never sees WebKit's blank (white) page or a half
// loaded one; a cue it replaces keeps its picture meanwhile. A live source
// has no preroll (PAUSED returns before decodebin has exposed a pad), so
// this cannot happen at the point a file cue is shown.
func showLive(p *gst.Pipeline, gen uint64) {
	defer incomingShown(p) // never leave a crossfade waiting on a failed page
	start := time.Now()
	reason := "loaded"
	for {
		mgr.mu.Lock()
		current := mgr.startCurrentLocked(p, gen)
		mgr.mu.Unlock()
		if !current {
			return
		}
		waited := time.Since(start)
		if liveFrameReady(p) {
			if at, ok := liveLoaded.Load(p); ok && time.Since(at.(time.Time)) >= livePaintSettle {
				break
			}
			if waited > liveLoadWait {
				reason = fmt.Sprintf("load not complete after %v, shown anyway", liveLoadWait)
				break
			}
		} else if waited > liveFrameWait {
			logs.Printf(logs.GSPPipeStopped, "gsp: live page drew no frame within %v", liveFrameWait)
			mgr.endpointFailed(p)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr.mu.Lock()
	c := mgr.clipOfLocked(p)
	if c == nil || !mgr.startCurrentLocked(p, gen) {
		mgr.mu.Unlock()
		return
	}
	level, title, layerAt, layerRef := c.fadeLevel, c.currentFile, c.layerAt, c.layerRef
	mgr.mu.Unlock()
	showLayer(p, level, layerAt, layerRef)
	incomingShown(p)
	logs.Printf(logs.GSPFireTiming, "%s: on screen after %.0fms (%s)", title, time.Since(start).Seconds()*1000, reason)
	fadeIn(p, gen)
}

// liveFrameReady reports whether p's first frame is on its wall layer.
func liveFrameReady(p *gst.Pipeline) bool {
	if glOpen {
		glMu.Lock()
		defer glMu.Unlock()
		return glLayers[p] != nil // glShow attaches it
	}
	if kmsWall() == nil {
		return true // plain sink: no layer to raise
	}
	l := layerOf(p)
	return l != nil && l.framed.Load()
}

// retirePipeline tears down a pipeline that is no longer (or about to stop
// being) the active one and posts a wake-up message to its bus so the watch
// closure's identity precheck runs and unregisters. SetState(Null) alone
// never produces a bus message, so the watch - and everything it captures -
// would otherwise leak on every cue swap for the lifetime of the process.
func retirePipeline(p *gst.Pipeline) {
	liveLoaded.Delete(p)
	p.SetState(gst.StateNull)
	audioBusRelease(p)
	dropLayer(p)
	incomingShown(p)
	p.GetPipelineBus().Post(gst.NewApplicationMessage(p, gst.NewStructure("retire")))
}

// watchTrim polls the pipeline position until the out-point is reached and
// then terminates the clip (freezing or tearing down per the hold policy).
// It stops early if the pipeline is replaced or stopped.
func watchTrim(p *gst.Pipeline) {
	for {
		mgr.mu.Lock()
		c := mgr.clipOfLocked(p) // the focus, or a voice it was demoted to
		if c == nil {
			mgr.mu.Unlock()
			return
		}
		outPoint, focus := c.outPoint, c == &mgr.clip
		mgr.mu.Unlock()
		if outPoint <= 0 {
			return
		}
		ok, pos := p.QueryPosition(gst.FormatTime)
		if ok && float64(pos) >= outPoint*1_000_000_000 {
			end := mgr.handleEnd
			if !focus {
				end = mgr.voiceEnd
			}
			if !end(p) {
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
	endpointURL string
	liveTitle   string
	// warmSink routes the video branch to fakesink instead of the wall: a
	// warm-slot build may not display anything or its first frame would
	// colour over the live cue (see Warm). Activation relinks the wall.
	warmSink bool
	opts     LoadOpts // the cue's playback options (geometry, rotation, …)
	overlay  bool     // test pattern: print the display mode on it
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
// endpointFPS is the live-page render rate when its frames are copied into
// the display plane (CPU path, or GL with a readback): TimerPi pages are text
// and countdowns (changing once a second; the overtime pulse twice), and
// fades run on the display plane, not the page, so 15 fps loses nothing
// visible there. Measured on a Pi 4 at 1080p: each copied frame costs CuTePi
// about 54% of a core at 30 fps, and a GL readback caps an animated page at
// 33-50 fps. The zero-copy path renders at the display's refresh rate.
const endpointFPS = 15

// livePageChain is what follows wpevideosrc. With GL (the default), WPE renders
// and composites the page on the GPU. On the KMS plane wall, wpedmabuf
// (gsp/scanout) GPU-copies each frame into a linear buffer the plane scans
// out, so the page runs at the display rate with no CPU copy: measured on a
// Pi 4 at 1080p60, an animated page (TimerPi's blue-future background) at 60.0
// fps, against 33-35 fps through gldownload, which reads every frame back
// into system memory and is still the path elsewhere (other wall sinks,
// 90/270° software rotation). Without GL memory WPE falls back to painting and
// compositing the whole page on the CPU: the same page cost WebKit 340% of a
// core that way against 198% with GL, for identical frames (h264-pi4 livepage
// results). CUTEPI_LIVEPAGE_GL=0 forces the CPU path.
func livePageChain(dw, dh, hz int, zeroCopy bool) ([]*gst.Element, error) {
	raw := func() ([]*gst.Element, error) {
		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return nil, err
		}
		caps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, endpointFPS)))
		return []*gst.Element{caps}, nil
	}
	if os.Getenv("CUTEPI_LIVEPAGE_GL") == "0" {
		return raw()
	}
	glCaps, err := gst.NewElement("capsfilter")
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: live page: no GStreamer GL (gstreamer1.0-gl), rendering on the CPU")
		return raw()
	}
	if zeroCopy && scanout.Register() {
		if out, err := gst.NewElement("wpedmabuf"); err == nil {
			if hz <= 0 {
				hz = 60
			}
			glCaps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw(memory:GLMemory),format=RGBA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, hz)))
			return []*gst.Element{glCaps, out}, nil
		}
	}
	down, err2 := gst.NewElement("gldownload")
	outCaps, err3 := gst.NewElement("capsfilter")
	if err2 != nil || err3 != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: live page: no GStreamer GL (gstreamer1.0-gl), rendering on the CPU")
		return raw()
	}
	glCaps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw(memory:GLMemory),format=RGBA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, endpointFPS)))
	outCaps.Set("caps", gst.NewCapsFromString("video/x-raw,format=RGBA"))
	return []*gst.Element{glCaps, down, outCaps}, nil
}

func buildPipeline(spec pipelineSpec) (*gst.Pipeline, error) {
	pipeline, err := gst.NewPipeline("")
	if err != nil {
		return nil, err
	}
	if glOpen {
		glwall.UseSystemClock(pipeline) // the wall's clock: the bridge maps running times onto it
	}

	var src *gst.Element
	var srcChain []*gst.Element // test patterns: sized to the display, optional mode label
	if spec.isTest {
		src, err = gst.NewElement("videotestsrc")
		if err != nil {
			return nil, err
		}
		// Enum property: SetArg parses the nick ("red", "checkers-1"); Set with
		// a Go string is a type mismatch GObject ignores (every pattern then
		// stayed the default SMPTE bars).
		src.SetArg("pattern", spec.testPattern)
		dw, dh, hz := DisplayMode()
		if dw <= 0 || dh <= 0 {
			dw, dh = 1920, 1080
		}
		rate := testPatternRate(spec.testPattern, hz)
		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return nil, err
		}
		gw, gh := dw, dh
		if spec.testPattern == "snow" {
			// Random noise needs no native resolution; the display layer
			// scales it up for free (1080p noise at 30 fps pins a core).
			gw, gh = dw/3&^1, dh/3&^1
		}
		caps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", gw, gh, rate)))
		srcChain = append(srcChain, caps)
		if spec.overlay {
			txt, err := gst.NewElement("textoverlay")
			if err != nil {
				return nil, err
			}
			label := fmt.Sprintf("%d × %d", dw, dh)
			if hz > 0 {
				label += fmt.Sprintf(" @ %d Hz", hz)
			}
			txt.Set("text", label)
			txt.Set("font-desc", "Sans Bold 40")
			txt.SetArg("valignment", "top")
			txt.SetArg("halignment", "left")
			txt.Set("shaded-background", true)
			// Plain raw video after the overlay: textoverlay then blends the
			// text itself instead of offering an overlay-composition caps
			// feature that the decodebin -> wall chain cannot link.
			plain, err := gst.NewElement("capsfilter")
			if err != nil {
				return nil, err
			}
			plain.Set("caps", gst.NewCapsFromString("video/x-raw"))
			srcChain = append(srcChain, txt, plain)
		}
	} else if spec.endpointURL != "" {
		// Live page (DESIGN §12.14): WPE renders the page off-screen (no
		// window, no DRM master) at the display's size. Its raw frames pass
		// straight through decodebin into the same wall tail as any video.
		src, err = gst.NewElement("wpevideosrc")
		if err != nil {
			return nil, errors.New("live pages need the WPE renderer: install the gstreamer1.0-wpe package")
		}
		src.Set("location", spec.endpointURL)
		dw, dh, hz := DisplayMode()
		if dw <= 0 || dh <= 0 {
			dw, dh = 1920, 1080
		}
		// Zero-copy only where the frames go straight to a plane: the KMS
		// wall with the rotation done by the display controller.
		_, hwRotate := rotationBits(spec.opts.Rotation, spec.opts.Flip)
		chain, err := livePageChain(dw, dh, hz, !glOpen && kmsWall() != nil && hwRotate && !spec.warmSink)
		if err != nil {
			return nil, err
		}
		srcChain = append(srcChain, chain...)
	} else {
		src, err = gst.NewElement("filesrc")
		if err != nil {
			return nil, err
		}
		srcFile := config.MediaLocation() + "/" + spec.filename
		src.Set("location", srcFile)
	}

	// GPU wall: decodebin3 plugs the decoder against the real downstream, so a
	// stateless V4L2 decoder negotiates DMABuf output; decodebin exposes such a
	// pad with system-memory tiled caps first and never renegotiates
	// (TEST_REPORT, GPU wall step 2).
	// Live pages stay on decodebin: its raw passthrough is what was verified
	// for wpevideosrc's frames.
	decoderBin := "decodebin"
	if glOpen && spec.endpointURL == "" {
		decoderBin = "decodebin3"
	}
	decodebin, err := gst.NewElement(decoderBin)
	if err != nil {
		return nil, err
	}

	head := append(append([]*gst.Element{src}, srcChain...), decodebin)
	if err := pipeline.AddMany(head...); err != nil {
		return nil, fmt.Errorf("building the source chain: %w", err)
	}
	if err := gst.ElementLinkMany(head...); err != nil {
		return nil, fmt.Errorf("linking the source chain: %w", err)
	}

	// Connect to decodebin's pad-added signal, emitted whenever it finds a
	// stream from the input and a way to decode it to raw format. decodebin
	// adds a src-pad for the raw stream, which we link into a playback tail.
	var primaryMu sync.Mutex
	primary := map[string]string{}
	decodebin.Connect("pad-added", func(self *gst.Element, srcPad *gst.Pad) {
		var isAudio, isVideo bool
		caps := srcPad.GetCurrentCaps()
		if caps == nil {
			// decodebin3 exposes a pad before it carries caps: ask what it
			// can produce (the decoder's template; the format is not fixed yet).
			caps = srcPad.QueryCaps(nil)
		}
		if caps == nil {
			caps = gst.NewCapsFromString("video/x-raw")
			if strings.HasPrefix(srcPad.GetName(), "audio") {
				caps = gst.NewCapsFromString("audio/x-raw")
			}
		}
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

		// Only the first audio and the first video stream get a real output
		// chain. A file with several audio tracks (dual-language mp4, mp3 plus
		// AC3) would otherwise build a second sink per stream: two sinks on one
		// device fail to preroll and the whole cue never starts. Extra streams
		// are drained into a fakesink so decodebin still sees every pad linked.
		kind := "video"
		if isAudio {
			kind = "audio"
		}
		streamID := srcPad.GetStreamID()
		primaryMu.Lock()
		if primary[kind] == "" {
			primary[kind] = streamID
		}
		isPrimary := primary[kind] == streamID
		primaryMu.Unlock()
		if !isPrimary {
			linkExtraStream(pipeline, srcPad, kind, streamID)
			return
		}

		var elementNames []string
		queueName := "video-queue"
		if isAudio {
			queueName = "audio-queue"
			// Audio routing: every cue's sound goes into the audio bus
			// (audiobus.go: one mixer on the configured device, so running
			// cues are heard together, §6.1.2).
			sink := "autoaudiosink"
			if !spec.isTest && !spec.warmSink {
				sink = "interaudiosink"
			}
			if spec.warmSink {
				// Warm-slot audio prerolls into a fakesink: the real device
				// is only opened at activation (warmWireAudio), so the slot
				// can never hold a hw device the live cue owns, and the cue
				// lands on the configured device/rate/channels like a cold
				// load does.
				sink = "fakesink"
			}
			elementNames = []string{"queue", "audioconvert", "audioresample", "volume", "audiopanorama", "scaletempo", sink}
		} else if glOpen && !spec.warmSink {
			// GPU wall: the cue feeds the mixer through an appsink (gllayer.go).
			elementNames = glVideoTail(glTailDMABuf(srcPad, spec.opts, spec.isTest), glTailFormat(caps, spec.filename),
				glDirection(spec.opts.Rotation, spec.opts.Flip) != dirIdentity, !spec.isTest && glISPCanTake(srcPad))
		} else if kmsWall() != nil {
			// KMS wall: own display plane, hardware scaling/blending.
			elementNames = kmsVideoTail(!spec.isTest && dmaBufUpstream(srcPad), spec.opts)
		} else {
			// Two videoflip stages (rotate, then mirror) so a cue can
			// combine e.g. 90° with a horizontal mirror; method=none
			// passes frames through untouched.
			// The second videoconvert sits after the flips: videoflip handles
			// only a subset of raw formats, so without a converter on the sink
			// side a picky wall sink (fbdevsink's RGB16 framebuffer) cannot
			// negotiate at all — decodebin then fails delayed linking and the
			// cue silently never plays. Passthrough cost when formats match.
			stem := videoStem(!spec.isTest && dmaBufUpstream(srcPad))
			elementNames = append(stem, wallVideoSink())
			if d := config.Display(); d.Resolution != "" && !d.UseEDID && gstCapsWidthHeightOK(d.Resolution) {
				// Manual wall resolution: retune the decoded stream to the
				// destination size (videoscale picks it up from the caps).
				elementNames = append(videoStem(!spec.isTest && dmaBufUpstream(srcPad)), "capsfilter", wallVideoSink())
			}
			if spec.warmSink {
				// Warm-slot video preroll: decode into fakesink — silent, no
				// wall takeover. warm-vf-tail is the relink anchor at activation.
				// Warm slots are always file decodes; the download applies
				// exactly when the pad carries DMABuf (see dmaBufUpstream).
				elementNames = append(videoStem(dmaBufUpstream(srcPad)), "fakesink")
			}
		}
		queueName += "-" + srcPad.GetStreamID()
		// decodebin recreates its pads after Stop -> Play. Reuse the tail;
		// an abandoned, unlinked sink would otherwise prevent preroll forever.
		if queue, err := pipeline.GetElementByName(queueName); err == nil && queue != nil {
			if ret := srcPad.Link(queue.GetStaticPad("sink")); ret != gst.PadLinkOK {
				failLink(pipeline, self, fmt.Errorf("relinking the %s stream: %v", kind, ret))
			}
			return
		}

		elements, err := gst.NewElementMany(elementNames...)
		if err != nil {
			msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "could not create playback elements", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}
		elements[0].Set("name", queueName)
		// Role map built from the same slice the elements were created
		// from, so it stays correct whatever optional stages the chain
		// carries. (Factory introspection is not reliably exposed, and
		// positional wiring silently mis-aimed every per-cue setting on
		// hardware-decoded pipelines.)
		byFactory := make(map[string][]*gst.Element, len(elements))
		for i, e := range elements {
			f := elementNames[i]
			byFactory[f] = append(byFactory[f], e)
		}
		// Settings-tab output tuning (only real pipelines; the warm slot
		// prerolls with default plumbing so it can never grab a hw device).
		if !spec.warmSink && !spec.isTest {
			if isAudio {
				// The bus carries the device, format and the GPU wall's
				// display delay (audiobus.go).
				elements[len(elements)-1].Set("name", "cue-audio-sink") // found again on resume
				if err := audioBusAttach(pipeline, elements[len(elements)-1]); err != nil {
					failLink(pipeline, self, fmt.Errorf("the audio output: %w", err))
					return
				}
			} else if i := indexOfName(elementNames, "capsfilter"); i >= 0 {
				setResolutionCaps(elements[i])
			}
		}
		if isVideo && glOpen && !spec.warmSink {
			if err := configureGLTail(pipeline, byFactory, elementNames, spec.opts, glTailDMABuf(srcPad, spec.opts, spec.isTest), glTailFormat(caps, spec.filename)); err != nil {
				msg := gst.NewErrorMessage(self, gst.NewGError(3, err), "no wall layer", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
		} else if isVideo && kmsWall() != nil {
			fw, fh := capsSize(caps)
			if err := configureKMSTail(pipeline, byFactory, spec.opts, fw, fh, !spec.isTest && isStillFile(spec.filename)); err != nil {
				msg := gst.NewErrorMessage(self, gst.NewGError(3, err), "no display layer", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
		}
		if spec.warmSink && isAudio {
			last := len(elements) - 1
			elements[last-1].Set("name", "warm-af-tail")
			elements[last].Set("name", "warm-audio-sink")
			elements[last].Set("sync", false)
		}
		if spec.warmSink && isVideo {
			if i := indexOfName(elementNames, "fakesink"); i > 0 {
				// Relink anchors for install-time: whatever feeds the fake
				// sink is the output edge (post-flip converter, or the
				// capsfilter when a wall resolution is configured); the
				// fake sink gets a findable name (startPlayback's pool).
				elements[i-1].Set("name", "warm-vf-tail")
				elements[i].Set("name", "warm-video-sink")
				elements[i].Set("sync", false)
			}
		}
		if err := pipeline.AddMany(elements...); err != nil {
			failLink(pipeline, self, fmt.Errorf("adding the %s output: %w", kind, err))
			return
		}
		if err := gst.ElementLinkMany(elements...); err != nil {
			failLink(pipeline, self, fmt.Errorf("linking the %s output (%s): %w", kind, strings.Join(elementNames, " ! "), err))
			return
		}

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
		// Elements resolve by role from the build slice, never by position:
		// optional stages (the v4l2convert download, the wall-resolution
		// capsfilter) shift indices, and positional wiring silently aimed
		// every setting at the wrong element on hardware-decoded pipelines.
		if isVideo && kmsWall() == nil {
			mgr.mu.Lock()
			if mgr.pipeline == pipeline {
				if el := firstByFactory(byFactory, "videobalance"); el != nil {
					mgr.brightEl = el
					el.Set("brightness", mgr.fadeLevel-1)
				}
				// Per-cue frame geometry (§5.5): letterbox vs stretch on
				// videoscale, rotation then mirror on the two videoflips.
				fit := mgr.fitMode
				if fit != "stretch" {
					fit = "fit"
				}
				if el := firstByFactory(byFactory, "videoscale"); el != nil {
					el.Set("add-borders", fit == "fit")
				}
				flips := byFactory["videoflip"]
				if len(flips) > 0 {
					flips[0].SetArg("method", rotationMethod(mgr.rotation))
				}
				if len(flips) > 1 {
					flips[1].SetArg("method", flipMethod(mgr.flip))
				}
			}
			mgr.mu.Unlock()
		}

		queue := elements[0]
		sinkPad := queue.GetStaticPad("sink")
		if ret := srcPad.Link(sinkPad); ret != gst.PadLinkOK {
			failLink(pipeline, self, fmt.Errorf("linking the decoded %s stream to its output: %v", kind, ret))
			return
		}
		if isVideo && glOpen && !spec.warmSink {
			glRegister(pipeline, byFactory, elementNames, spec.opts)
		}
	})

	return pipeline, nil
}

// gstCapsWidthHeightOK validates a settings "WxH" resolution for caps use.
func gstCapsWidthHeightOK(res string) bool {
	parts := strings.SplitN(res, "x", 2)
	if len(parts) != 2 {
		return false
	}
	w, e1 := strconv.Atoi(parts[0])
	h, e2 := strconv.Atoi(parts[1])
	return e1 == nil && e2 == nil && w > 0 && h > 0
}

// setResolutionCaps retunes a capsfilter element to the configured wall
// resolution (Display tab). Silent on invalid config: a broken setting must
// never stop the show, only fall back to the media's own size.
func setResolutionCaps(capsEl *gst.Element) {
	d := config.Display()
	if !gstCapsWidthHeightOK(d.Resolution) {
		return
	}
	w, _ := strconv.Atoi(strings.SplitN(d.Resolution, "x", 2)[0])
	h, _ := strconv.Atoi(strings.SplitN(d.Resolution, "x", 2)[1])
	capsEl.Set("caps", gst.NewCapsFromString(
		fmt.Sprintf("video/x-raw,width=%d,height=%d", w, h)))
}
