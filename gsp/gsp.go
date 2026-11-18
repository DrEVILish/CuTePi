package gsp

import (
	"CuTePi/gsp/animdec"
	"CuTePi/gsp/av1dec"
	"CuTePi/gsp/bobdeint"
	"CuTePi/gsp/cfhd"
	"CuTePi/gsp/mjpegdec"
	"CuTePi/gsp/pngvdec"
	"CuTePi/gsp/qtmkv"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	callbacks         sync.WaitGroup
	onEndpointFailure func(pos int, title string, generation uint64)
	version           uint64        // bumped on every client-visible state change/position tick
	onCueEnd          func(pos int) // invoked (in a goroutine) when an active cue reaches its end
	gen               uint64        // bumped on every playback-decision change (load/stop/panic/teardown)
	halts             uint64        // bumped by every operator Stop/Panic call, whether or not a pipeline was running
	loads             uint64        // bumped whenever a new pipeline is installed (swap)
	warm              warmSlot      // prebuilt+prerolled next cue, hidden and silent (see Warm)
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
//   - a52dec and mpeg2dec (gstreamer1.0-plugins-ugly, installed for its ASF
//     and RealMedia demuxers): liba52 would outrank FFmpeg's AC-3 decoder
//     (secondary over marginal) and change the decoder every AC-3 track has
//     been tested on; libmpeg2 is demoted with it so MPEG-1/2 stays on
//     avdec_mpeg2video whatever the ranks.
//
// and promote one decoder that is faster than the default:
//   - avdec_vp8 (FFmpeg's VP8) over vp8dec (libvpx, primary): two 1080p60
//     VP8 layers at once on a Pi 4 run at 6 + 6 fps with libvpx (its threads
//     spin against each other) and 56 + 56 with FFmpeg's (h264-pi4
//     OPEN-CODECS.md). WebM with alpha still goes through vp8alphadecodebin,
//     which ranks above both.
var decoderRankOverrides = []string{"v4l2jpegdec:0", "openjpegdec:0", "a52dec:0", "mpeg2dec:0", "avdec_vp8:257"}

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
		// CineForm through the CineForm SDK (gsp/cfhd) when its library is installed.
		if cfhd.Register() {
			logs.Printf(logs.GSPPipeDebug, "gsp: CineForm decoder: CineForm SDK (cutepicfhddec)")
		}
		// Interlaced video to a picture per field (gsp/bobdeint).
		bobdeint.Register()
		// HAP and QuickTime Animation in Matroska (gsp/qtmkv).
		qtmkv.Register()
		// Motion JPEG on several cores (gsp/mjpegdec).
		if mjpegdec.Register() {
			logs.Printf(logs.GSPPipeDebug, "gsp: Motion JPEG decoder: parallel jpegdec (cutepimjpegdec)")
		}
		// PNG video on several cores (gsp/pngvdec).
		if pngvdec.Register() {
			logs.Printf(logs.GSPPipeDebug, "gsp: PNG video decoder: parallel libpng (cutepipngvdec)")
		}
		// Animated WebP, APNG, AVIF and JPEG XL (gsp/animdec).
		if f := animdec.Register(); f != "" {
			logs.Printf(logs.GSPPipeDebug, "gsp: animated image decoder (cutepianimdec): %s", f)
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
// keepUp: the new cue shows at once (an armed slot) or loads hidden (a live
// page), so what it replaces stays on screen until it does; a cold file load
// replaces at once (the old cues' decoding would slow its preroll).
func (m *manager) swap(newPipeline *gst.Pipeline, currentFile string, opts LoadOpts, keepUp bool) {
	if !opts.KeepBackground {
		stopBackground() // a new playback decision always kills the soundtrack
	}
	m.mu.Lock()
	// A warm slot from a previous decision is somebody else's memory. It
	// goes in the background: its plane comes off with a display commit,
	// which GO need not wait for.
	if w := m.warm.take(); w != nil {
		go retirePipeline(w)
	}
	var retire []*gst.Pipeline
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
		// the new cue crossfades. On a cut with keepUp the new one goes on
		// top and the old ones are removed in one step once it shows: no
		// black while a live page loads (showLive), and the old cue's
		// teardown is not in GO's path.
		stopping := voiceClips(m.takeVoicesLocked())
		if m.pipeline != nil && m.pipeline != newPipeline {
			stopping = append(stopping, &m.clip)
		}
		overNew := opts.Crossfade > 0 || keepUp
		if keepUp && opts.Crossfade <= 0 {
			layerAt = LayerTop
		}
		for _, c := range stopping {
			if overNew && layerOf(c.pipeline) != nil {
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
	if cb != nil {
		m.callbacks.Add(1)
	}
	m.mu.Unlock()
	m.clearIfCurrent(p)
	if cb != nil {
		go func() { defer m.callbacks.Done(); cb(pos, title, Generation()) }()
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
	OpacitySet                 bool // distinguishes an explicit zero from the direct-load default
	GeomX, GeomY, GeomW, GeomH string
	// Crop per edge of the source picture: "", pixels, or "N%".
	CropL, CropR, CropT, CropB string
	// Align: "[left|center|right] [top|middle|bottom]" (either part optional);
	// the picture sits against those screen edges (wallRect, computeLayout).
	Align string
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
	if state == gst.StatePlaying {
		if t := takeStartAt(p); !t.IsZero() {
			applyStartAt(p, t)
		}
	}
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
	retire = append(retire, mgr.warm.take(), spare.take()) // PANIC tears everything down, including the armed cues
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
	mgr.gen++               // the warm slot stays: it is the selected cue's, still GO's next
	mgr.version++
	mgr.mu.Unlock()
	blankWall(0)
	broadcastSoon()
}

// ShowTest loads and plays a GStreamer video-test-pattern.
func ShowTest(pattern string) error {
	gstInit()
	if pattern == AVSyncPattern {
		return showAVSync()
	}
	newPipeline, err := buildPipeline(pipelineSpec{isTest: true, testPattern: pattern, overlay: TestOverlay()})
	if err != nil {
		return err
	}
	mgr.swap(newPipeline, "", LoadOpts{}, false)
	mgr.mu.Lock()
	mgr.testShowing = true
	mgr.mu.Unlock()
	return watchAndPlay(newPipeline)
}

// AVSyncPattern is the A/V sync test signal's name in the Tests list
// (DESIGN §12.11): a dial, an offset meter and corner markers around a 1 kHz
// tone every 2 s, played as a file through the cue path so it measures the
// real picture-to-sound delay.
const AVSyncPattern = "avsync"

var avsyncMu sync.Mutex // one generation at a time

// showAVSync plays the sync clip for the current display mode in a loop,
// generating it on first use (under a minute on the Pi; cached beside the
// thumbnails, the signal's version in the name).
func showAVSync() error {
	dw, dh, hz := DisplayMode()
	dir := filepath.Join(filepath.Dir(config.ThumbnailLocation()), "testsignals")
	prefix := fmt.Sprintf("avsync-v%d-", media.AVSyncVersion)
	path := filepath.Join(dir, fmt.Sprintf("%s%dx%d-%d.mkv", prefix, dw, dh, hz))
	avsyncMu.Lock()
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			avsyncMu.Unlock()
			return err
		}
		// Clips of an older signal are never played again.
		old, _ := filepath.Glob(filepath.Join(dir, "avsync-*.mkv"))
		for _, f := range old {
			if !strings.HasPrefix(filepath.Base(f), prefix) {
				os.Remove(f)
			}
		}
		t0 := time.Now()
		if err := media.GenerateAVSync(path, dw, dh, hz); err != nil {
			avsyncMu.Unlock()
			return err
		}
		logs.Printf(logs.GSPPipeDebug, "gsp: A/V sync clip for %dx%d@%d generated in %.1fs", dw, dh, hz, time.Since(t0).Seconds())
	}
	avsyncMu.Unlock()
	if err := LoadWithOpts(path, LoadOpts{Loop: true}); err != nil {
		return err
	}
	mgr.mu.Lock()
	mgr.testShowing = true
	mgr.mu.Unlock()
	return nil
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
	noteFire()
	halt := Halts()
	if !awaitWarmBuild(filename, opts, 6*time.Second) {
		return fmt.Errorf("loading %q: prewarm is still using the decoder", filename)
	}
	if Halts() != halt {
		return fmt.Errorf("loading %q: playback halted while waiting for prewarm", filename)
	}
	if InstallWarm(filename, opts) {
		logs.Printf(logs.GSPFireTiming, "cue load %q: warm swap %.1fms", filename, time.Since(t0).Seconds()*1000)
		return nil
	}
	mgr.mu.Lock()
	warm := mgr.warm.take()
	mgr.mu.Unlock()
	retireAll(warm)
	newPipeline, err := buildPipeline(pipelineSpec{isTest: false, filename: filename, opts: opts})
	if err != nil {
		return err
	}
	buildMS := time.Since(t0).Seconds() * 1000
	mgr.swap(newPipeline, filename, opts, false)
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
	opts = liveOpts(opts)
	noteFire()
	p := takeStandbyLive(title, endpointURL, opts) // loaded ahead of this GO (livestandby.go)
	if p != nil {
		logs.Printf(logs.GSPWarm, "live page %q: playing the standby renderer", title)
	} else {
		var err error
		if p, err = buildPipeline(pipelineSpec{endpointURL: endpointURL, liveTitle: title, opts: opts}); err != nil {
			return err
		}
	}
	mgr.swap(p, "Live: "+title, opts, true)
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

// Every seek carries the segment rate, including loop and trim restarts.
func seekAtRate(p *gst.Pipeline, seconds float64) bool {
	return seekClip(p, seconds, gst.SeekFlagFlush)
}

// seekClip seeks p's clip with flags. A clip that will loop again seeks as a
// segment: its end posts SEGMENT_DONE instead of EOS, and the restart is a
// non-flushing segment seek (flags none) queued behind the pass's last
// frames, so the loop has no gap (§6.2).
func seekClip(p *gst.Pipeline, seconds float64, flags gst.SeekFlags) bool {
	mgr.mu.Lock()
	c := mgr.clipOfLocked(p) // the focus or a voice (§6.1.2)
	current, rate := c != nil, 1.0
	if c != nil {
		rate = c.rate
		if c.loopsAgain() {
			flags |= gst.SeekFlagSegment
		}
	}
	mgr.mu.Unlock()
	if !current || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return false
	}
	return p.SendEvent(gst.NewSeekEvent(clampRate(rate), gst.FormatTime, flags,
		gst.SeekTypeSet, int64(math.Max(0, seconds)*1e9), gst.SeekTypeEnd, -1))
}

// loopsAgain reports whether the clip's current pass ends in a restart (a
// file, looping, not on its last finite pass): that pass plays as a segment.
func (c *clip) loopsAgain() bool {
	restart, _ := loopSteps(c.loopRemain)
	return c.loop && restart && !c.liveEndpoint && !c.still
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
func (m *manager) handleEnd(p *gst.Pipeline) bool { return m.passEnd(p, false) }

// passEnd is handleEnd; segmentDone marks a segment pass's end (SEGMENT_DONE,
// seekClip), whose restart does not flush.
func (m *manager) passEnd(p *gst.Pipeline, segmentDone bool) bool {
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
			if segmentDone {
				seekClip(p, inPoint, gst.SeekFlagNone)
			} else {
				seekAtRate(p, inPoint)
				setStateIfCurrent(p, gst.StatePlaying)
			}
			m.mu.Lock()
			m.paused = false
			m.mu.Unlock()
			m.bump()
			return true
		}
	}
	if pos > 0 && cb != nil {
		m.callbacks.Add(1)
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
		go func() { defer m.callbacks.Done(); cb(pos) }()
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
		// other ways the watch unregisters). retirePipeline also removes the
		// watch outright (its bus is flushing in NULL: no message arrives).
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
			case gst.MessageSegmentDone:
				return mgr.voicePassEnd(p, true)
			case gst.MessageError:
				logs.Printf(logs.GSPPipeStopped, "gsp: layered cue stopped: %s", msg.ParseError().Error())
				mgr.voiceFailed(p)
				return false
			}
			return true
		}
		switch msg.Type() {
		case gst.MessageSegmentDone:
			// shortcut: Loop turned off mid-pass ends here, a few in-flight
			// frames before the file's end; push EOS downstream if that matters.
			mgr.passEnd(p, true)
			mgr.mu.Lock()
			current := mgr.pipeline == p
			mgr.mu.Unlock()
			return current
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
	goAt := time.Now()
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
	segment := false
	if c := mgr.clipOfLocked(p); c != nil {
		inPoint, rate, outPoint = c.inPoint, c.rate, c.outPoint
		segment = c.loopsAgain()
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
	if (inPoint > 0 || rate != 1 || segment) && !preSeeked(p, inPoint, rate, segment) { // a loop's first pass is a segment too
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
	// cues above it start fading away. A timed start (§6.8c) shows it at
	// its instant instead, so linked units put the first frame up together.
	at := peekStartAt(p)
	onTime := at.IsZero() && PresenterWall()
	if at.IsZero() && !onTime {
		showLayer(p, level, layerAt, layerRef)
		incomingOnScreen(p)
	}
	if err := setStateIfCurrent(p, gst.StatePlaying); err != nil {
		if errors.Is(err, errPipelineReplaced) {
			return nil // superseded while starting; the newer decision owns output
		}
		mgr.clearIfCurrent(p)
		return fmt.Errorf("gsp: starting playback: %w", err)
	}
	if onTime {
		// The presenter shows the first frame at its time, with its sound,
		// not when it prerolled (pwStartLatency earlier), but no later than
		// pwFirstFrameMax after GO.
		at = dueTime(p)
		if limit := goAt.Add(pwFirstFrameMax); at.After(limit) {
			at = limit
		}
	}
	if at.IsZero() {
		go fadeIn(p, gen)
	} else {
		go func() {
			if showAt(p, gen, at, level, layerAt, layerRef) {
				fadeIn(p, gen)
			}
		}()
	}
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

// liveStandbyShowWait bounds the wait for a page that was standing by (the
// operator highlighted it before GO): it is shown within this of GO, loaded
// or not, and finishes loading on screen. The requirement is under 1 s from
// GO to picture; a cold page keeps liveLoadWait.
var liveStandbyShowWait = 800 * time.Millisecond

// liveStandbyPainted: a page that has stood by this long has painted its
// content (a news site never reports "loaded"; waiting for it only cost the
// whole liveStandbyShowWait), so GO shows it with its first frame.
var liveStandbyPainted = 2 * time.Second

// liveFromStandby marks pipelines GO took from the standby pool, with when
// their renderer started.
var liveFromStandby sync.Map // *gst.Pipeline -> time.Time

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
			wait := liveLoadWait
			if built, ok := liveFromStandby.Load(p); ok {
				wait = liveStandbyShowWait
				if start.Sub(built.(time.Time)) >= liveStandbyPainted {
					wait = 0
				}
			}
			if waited > wait {
				reason = fmt.Sprintf("load not complete after %v, shown anyway", wait)
				break
			}
		} else if waited > liveFrameWait {
			logs.Printf(logs.GSPPipeStopped, "gsp: live page drew no frame within %v", liveFrameWait)
			mgr.endpointFailed(p)
			return
		}
		if waited < 100*time.Millisecond {
			time.Sleep(2 * time.Millisecond) // a standby page's frame comes within a frame or two of GO
		} else {
			time.Sleep(10 * time.Millisecond)
		}
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
// being) the active one. Every Go closure holding p must go with it or p is
// never finalized (its bus sockets, decoders and buffers stay): the bus watch
// is removed here (a pipeline in NULL flushes its bus, so a message posted
// to wake the watch is dropped and the watch stays), and its decodebin's
// pad-added handler (signals.go) is dropped.
func retirePipeline(p *gst.Pipeline) {
	p.SetState(gst.StateNull)
	noteRetired(p)
	p.GetPipelineBus().RemoveWatch()
	padHandlers.Delete(pipeKey(p))
	liveLoaded.Delete(p) // after the watch: it stores here
	liveFromStandby.Delete(p)
	warmSeeked.Delete(p)
	stopLiveRenderer(p)
	audioBusRelease(p)
	dropLayer(p)
	incomingShown(p)
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

// WaitCallbacks drains app hooks after the caller detached them and stopped playback.
func WaitCallbacks() { mgr.callbacks.Wait() }
