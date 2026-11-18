package gsp

import (
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/logs"
)

// warmSlot owns an armed pipeline and the identity it must still match.
// Access and transfers are guarded by mgr.mu.
type warmSlot struct {
	p      *gst.Pipeline
	file   string
	opts   LoadOpts
	source warmSource
}

func (s warmSlot) matches(file string, opts LoadOpts) bool {
	if s.p == nil || s.file != file || s.opts != opts {
		return false
	}
	now := snapshotWarmSource(file)
	was := s.source
	return was.info != nil && now.info != nil && was.path == now.path && os.SameFile(was.info, now.info) && was.info.Size() == now.info.Size() && was.info.ModTime().Equal(now.info.ModTime())
}

// take clears the complete arm; retire the returned pipeline after unlocking.
func (s *warmSlot) take() *gst.Pipeline {
	p := s.p
	*s = warmSlot{}
	return p
}

// spare is the cue after the selected cue, so GO, GO finds both armed.
// KMS wall only; guarded by mgr.mu.
var spare warmSlot

// armedLocked reports whether either slot holds file+opts (mgr.mu held).
func armedLocked(file string, opts LoadOpts) bool {
	return mgr.warm.matches(file, opts) ||
		spare.matches(file, opts)
}

// retireAll retires every non-nil pipeline. Never call it holding mgr.mu.
func retireAll(ps ...*gst.Pipeline) {
	for _, p := range ps {
		if p != nil {
			retirePipeline(p)
		}
	}
}

var warmMu sync.Mutex // serialises Warm

// warmBuild is the arm Warm is building (done closes when it ends; nil
// when none). Guarded by mgr.mu.
var warmBuild struct {
	file string
	opts LoadOpts
	done chan struct{}
}

// awaitWarmBuild waits (at most max) for an arm of file+opts that is being
// built: a GO that lands mid-arm takes the arm when it is ready rather than
// building the same cue again beside it. False means the caller must
// report a timeout instead of starting a competing decoder.
func awaitWarmBuild(file string, opts LoadOpts, max time.Duration) bool {
	mgr.mu.Lock()
	done := warmBuild.done
	same := done != nil && warmBuild.file == file && warmBuild.opts == opts
	mgr.mu.Unlock()
	if !same {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(max):
		return false
	}
}

// Warm prebuilds and prerolls the NEXT cue into a slot (deck-style double
// buffer), so a later LoadWithOpts with the same file+opts activates it
// instead of rebuilding. On the KMS wall the slot is built exactly as the
// cue plays: its first frame (at the trim in-point) sits on the cue's own
// plane at alpha 0, outside the visible stack, as the panic image does, and
// its sound on the audio bus, silent until it plays; GO then only raises
// the layer and starts the clock (DESIGN §6.1.4). Elsewhere video/image
// cues preroll on fakesink (spec.warmSink) and get the wall sink relinked
// at activation (a re-preroll); stills do not warm there (a single frame
// never re-prerolls after the relink's flush seek). An arm already in the
// slot is kept. A stale arm (a newer playback decision landed meanwhile)
// is retired, never installed.
func Warm(file string, opts LoadOpts) error {
	gstInit()
	halt := Halts()
	source := snapshotWarmSource(file)
	if glOpen {
		return nil // GL wall: prewarm not wired yet; cold builds are quick
	}
	onWall := Layered()
	if !onWall && isStillFile(file) {
		return nil
	}
	warmMu.Lock() // one build at a time: a second arm of the same cue finds it armed
	defer warmMu.Unlock()
	mgr.mu.Lock()
	if mgr.halts != halt || mgr.warm.matches(file, opts) {
		mgr.mu.Unlock()
		return nil
	}
	if spare.matches(file, opts) {
		// Armed as the next cue: it becomes the slot (the selection moved
		// onto it), and what the slot held goes.
		old := mgr.warm.take()
		mgr.warm, spare = spare, warmSlot{}
		mgr.mu.Unlock()
		if old != nil {
			go retirePipeline(old)
		}
		return nil
	}
	buildGen := mgr.gen
	stale := mgr.warm.take()
	done := make(chan struct{})
	warmBuild.file, warmBuild.opts, warmBuild.done = file, opts, done
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		warmBuild.done = nil
		mgr.mu.Unlock()
		close(done)
	}()
	retireAll(stale)
	p, err := buildPipeline(pipelineSpec{isTest: false, filename: file, warmSink: opts.WarmPreroll && !onWall, opts: opts})
	if err != nil {
		return err
	}
	// Preroll parked (paused): first buffers decoded, nothing displayed, no
	// audible output until a later activation starts the pipeline.
	p.SetState(gst.StatePaused)
	result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
	if result == gst.StateChangeFailure || (onWall && result != gst.StateChangeSuccess) {
		// An arm still prerolling after 5 s is not kept either: it would
		// hold its decoder (the Pi's one H.264 decoder) for every other cue.
		retirePipeline(p)
		return fmt.Errorf("prewarm preroll failed: %v", result)
	}
	if onWall && !warmPreSeek(p, file, opts) {
		retirePipeline(p)
		return fmt.Errorf("prewarm seek to the in-point failed")
	}
	mgr.mu.Lock()
	current := mgr.gen == buildGen && mgr.halts == halt
	var displaced *gst.Pipeline
	if current {
		displaced = mgr.warm.take() // a concurrent Warm may have filled it
		mgr.warm = warmSlot{p: p, file: file, opts: opts, source: source}
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

// WarmNext arms file+opts in the spare slot: the cue after the selected one
// (KMS wall only, built exactly as Warm builds it). Arming a cue already in
// either slot keeps it; what the spare held goes. Not dropped when a GO
// lands meanwhile: the next cue is still wanted after it (Warm keeps its
// stale-arm rule for its other callers).
func WarmNext(file string, opts LoadOpts) error {
	gstInit()
	halt := Halts()
	source := snapshotWarmSource(file)
	if glOpen || !Layered() {
		return nil
	}
	warmMu.Lock()
	defer warmMu.Unlock()
	mgr.mu.Lock()
	if mgr.halts != halt || armedLocked(file, opts) {
		mgr.mu.Unlock()
		return nil
	}
	stale := spare.take()
	done := make(chan struct{})
	warmBuild.file, warmBuild.opts, warmBuild.done = file, opts, done
	mgr.mu.Unlock()
	defer func() {
		mgr.mu.Lock()
		warmBuild.done = nil
		mgr.mu.Unlock()
		close(done)
	}()
	retireAll(stale)
	p, err := buildPipeline(pipelineSpec{filename: file, opts: opts})
	if err != nil {
		return err
	}
	p.SetState(gst.StatePaused)
	if result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second)); result != gst.StateChangeSuccess {
		retirePipeline(p) // as Warm: an arm that did not preroll is not kept
		return fmt.Errorf("prewarm preroll failed: %v", result)
	}
	if !warmPreSeek(p, file, opts) {
		retirePipeline(p)
		return fmt.Errorf("prewarm seek to the in-point failed")
	}
	mgr.mu.Lock()
	if mgr.halts != halt {
		mgr.mu.Unlock()
		retirePipeline(p)
		return nil
	}
	displaced := spare.take()
	spare = warmSlot{p: p, file: file, opts: opts, source: source}
	mgr.mu.Unlock()
	retireAll(displaced)
	watchWarm(p)
	return nil
}

// watchWarm parks the slot's error watch while the pipeline is idle: any
// bus error or retirement unregisters. EOS cannot occur here — a PAUSED
// pipeline never reaches end-of-stream on its own.
func watchWarm(p *gst.Pipeline) {
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		mgr.mu.Lock()
		current := mgr.warm.p == p || spare.p == p
		mgr.mu.Unlock()
		if !current {
			return false
		}
		if msg.Type() == gst.MessageError {
			logs.Printf(logs.GSPPipeDebug, "gsp: warm pipeline error debug: %s", msg.ParseError().DebugString())
			mgr.mu.Lock()
			var dead *gst.Pipeline
			if mgr.warm.p == p {
				dead = mgr.warm.take()
			} else if spare.p == p {
				dead = spare.take()
			}
			mgr.mu.Unlock()
			retireAll(dead)
			return false
		}
		return true
	})
}

// lastFire is when the latest cue load began (unix ns).
var lastFire atomic.Int64

// noteFire marks a cue load beginning.
func noteFire() { lastFire.Store(time.Now().UnixNano()) }

// LastFire is when the latest cue load began (zero time if none yet).
func LastFire() time.Time {
	if n := lastFire.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// Starting reports whether the focus cue is still being started (built
// and prerolled, not yet shown).
func Starting() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.starting
}

// IsWarm reports whether the warm slot or the spare holds file with opts.
func IsWarm(file string, opts LoadOpts) bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return armedLocked(file, opts)
}

// IsWarmSlot reports whether the warm slot itself (not the spare) holds
// file with opts.
func IsWarmSlot(file string, opts LoadOpts) bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.warm.matches(file, opts)
}

// InstallWarm activates the prewarmed pipeline for file+opts if the slot is
// still the one Warm built: retire the current transport, install the warm
// pipeline, play. False = stale/mismatched slot, caller falls back.
func InstallWarm(file string, opts LoadOpts) bool {
	if glOpen {
		return false
	}
	mgr.mu.Lock()
	var p *gst.Pipeline
	if mgr.warm.matches(file, opts) {
		p = mgr.warm.take() // claim before retiring anything else
	} else if spare.matches(file, opts) {
		p = spare.take()
	}
	mgr.mu.Unlock()
	if p == nil {
		return false
	}
	// The slot's own error watch: a bus takes one watch, and watchAndPlay's
	// would not be added beside it (no end-of-cue or error handling).
	p.GetPipelineBus().RemoveWatch()
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
	mgr.swap(p, file, opts, true)
	if err := watchAndPlay(p); err != nil {
		// startPlayback already tore the slot down; report a miss so the
		// caller retries with a cold build instead of claiming success.
		logs.Printf(logs.GSPPipeStopped, "gsp: warm pipeline failed to start %q: %v (cold load)", file, err)
		return false
	}
	return true
}

// warmSeek is the trim/rate/loop seek a slot made while it prerolled.
type warmSeek struct {
	in, rate float64
	segment  bool
}

// warmSeeked holds, per slot pipeline, the seek warmPreSeek made, which
// startPlayback then does not repeat (a flush seek re-decodes from the
// keyframe: 100s of ms at GO).
var warmSeeked sync.Map // *gst.Pipeline -> warmSeek

// warmPreSeek makes the start seek startPlayback would make at GO (trim
// in-point, rate, a loop's segment), while the slot waits, and waits for
// the re-preroll. True when none is needed or it landed.
func warmPreSeek(p *gst.Pipeline, file string, opts LoadOpts) bool {
	ws := warmSeek{in: opts.InPoint, rate: clampRate(opts.Rate)}
	ws.segment = opts.Loop && !isStillFile(file) && (opts.LoopCount == 0 || opts.LoopCount > 1) // clip.loopsAgain on its first pass
	if ws.in <= 0 && ws.rate == 1 && !ws.segment {
		return true
	}
	flags := gst.SeekFlagFlush
	if ws.segment {
		flags |= gst.SeekFlagSegment
	}
	if !p.SendEvent(gst.NewSeekEvent(ws.rate, gst.FormatTime, flags, gst.SeekTypeSet, int64(math.Max(0, ws.in)*1e9), gst.SeekTypeEnd, -1)) {
		return false
	}
	if result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second)); result != gst.StateChangeSuccess {
		return false
	}
	warmSeeked.Store(p, ws)
	return true
}

// preSeeked reports (once) that p's slot already made this start seek.
func preSeeked(p *gst.Pipeline, in, rate float64, segment bool) bool {
	v, ok := warmSeeked.LoadAndDelete(p)
	return ok && v.(warmSeek) == warmSeek{in, rate, segment}
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

type warmSource struct {
	path string
	info os.FileInfo
}

func snapshotWarmSource(file string) warmSource {
	path := sourcePath(file)
	info, _ := os.Stat(path)
	return warmSource{path, info}
}
