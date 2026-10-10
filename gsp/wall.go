package gsp

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/gsp/planewall"
	"CuTePi/gsp/yuvpack"
	"CuTePi/logs"
)

// Layers on the KMS wall. Each pipeline that shows video owns one overlay
// plane for its lifetime. The current cue and any cues still fading out
// (crossfades, "fade and stop others") are all on screen at once: the display
// controller stacks them by zpos and blends each by its alpha over the black
// primary plane. A newly started cue always goes UNDER the layers that are
// fading out, so the outgoing picture fades away to reveal it.

type wallLayer struct {
	p       *gst.Pipeline
	plane   *kmsPlane
	sink    atomic.Pointer[gst.Element] // the layer's kmssink (set once the tail is built)
	opacity float64                     // cue opacity 0..1 (multiplies every fade level)
	visible bool                        // part of the on-screen stack (not a warm/prerolling slot)
	parked  bool                        // invisible at the top zpos, ready to be raised (panic image)
	still   atomic.Bool                 // shows a single-frame image: no video to share commits with
	framed  atomic.Bool                 // the sink has received its first frame

	// Alpha writes are commits that wait for the next vblank (~16 ms), so
	// they run on the layer's own writer: callers (fade loops holding the
	// playback lock) only post the latest value.
	want atomic.Uint64 // float64 bits of the alpha to show
	kick chan struct{}
	quit chan struct{}
}

func (l *wallLayer) post(alpha float64) {
	if planewall.IsOpen() {
		// The presenter puts it in the next commit (every refresh): no
		// pacing, nothing to share.
		if w := kmsWall(); w != nil {
			_ = w.setAlpha(l.plane, alpha)
		}
		return
	}
	l.want.Store(math.Float64bits(alpha))
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *wallLayer) writer(w *KMSWall) {
	last := -1.0
	var lastFrames uint64
	lastMove := time.Now() // a clip counts as moving from its start
	for {
		select {
		case <-l.quit:
			return
		case <-l.kick:
		}
		a := math.Float64frombits(l.want.Load())
		// Steps below ~0.25% are invisible; the end values always land.
		if a == last || (math.Abs(a-last) < 1.0/400 && a != 0 && a != l.opacity) {
			continue
		}
		start := time.Now()
		_ = w.setAlpha(l.plane, a)
		last = a
		// Each alpha write is a display commit that waits for a vblank, and
		// so does every video frame kmssink shows: the CRTC takes 60 commits
		// a second in all. While the layer's video is moving, alpha steps on
		// a grid of two refreshes (30 a second), leaving a 30 fps clip every
		// frame it needs. A layer showing no new frames (a still, a paused or
		// held clip) has nothing to share with, so it steps every refresh:
		// a 60-step fade. The grid counts from the start of the write: the
		// commit's own vblank wait is part of the step, not added to it
		// (sleeping two refreshes after the blocking write gave 20 steps a
		// second, codec support test).
		// A clip counts as moving until it has shown no new frame for half a
		// second (paused, held). Judging by a few refreshes instead starved
		// slow decoders: a 10 fps clip looked still, its fade took every
		// commit, and it never caught up with its clock (DNxHR 10.7 -> 0.3 fps).
		step := w.framePeriod()
		if n, ok := l.rendered(); ok && n != lastFrames {
			lastFrames, lastMove = n, time.Now()
		}
		if !l.still.Load() && time.Since(lastMove) < 500*time.Millisecond {
			step *= 2
		}
		if d := step - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}
}

// rendered is the number of frames the layer's sink has shown (basesink
// stats), read once per alpha step: no per-frame callback.
func (l *wallLayer) rendered() (uint64, bool) {
	sink := l.sink.Load()
	if sink == nil {
		return 0, false
	}
	v, err := sink.GetProperty("stats")
	if err != nil {
		return 0, false
	}
	st, ok := v.(*gst.Structure)
	if !ok || st == nil {
		return 0, false
	}
	r, err := st.GetValue("rendered")
	if err != nil {
		return 0, false
	}
	n, ok := r.(uint64)
	return n, ok
}

var (
	wallOnce sync.Once
	kmsDisp  *KMSWall

	layersMu sync.Mutex
	layers   = map[*gst.Pipeline]*wallLayer{}
	stack    []*wallLayer // visible layers, bottom -> top
)

// OpenWall claims the display at startup (KMS wall only), before anything
// else can become its DRM master.
func OpenWall() {
	kmsWall()
	if glWallEnabled() {
		gstInit()
		glWall()
	}
}

// Layered reports whether cues get their own display layers (KMS wall):
// crossfades, opacity and geometry need it.
func Layered() bool { return kmsWall() != nil }

// kmsWall returns the open KMS display when the wall sink is kmssink.
func kmsWall() *KMSWall {
	if !planeSinkWanted() && wallVideoSink() != "kmssink" && !glWallEnabled() {
		return nil
	}
	wallOnce.Do(func() {
		w, err := openKMSWall()
		if err != nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: KMS wall unavailable: %v", err)
			return
		}
		kmsDisp = w
		logs.Printf(logs.GSPPipeDebug, "gsp: KMS wall %dx%d@%dHz, %d layers", w.Width, w.Height, w.Refresh, len(w.planes))
		if planeSinkWanted() && !glWallEnabled() {
			gstInit()
			ids := make([]uint32, len(w.planes))
			for i, p := range w.planes {
				ids[i] = p.id
			}
			if err := planewall.Open(w.fd, w.crtcID, ids, w.Width, w.Height, w.Refresh); err != nil {
				logs.PrintfWarn(logs.GSPPipeDebug, "gsp: plane wall presenter unavailable, using kmssink: %v", err)
			} else {
				logs.Printf(logs.GSPPipeDebug, "gsp: plane wall presenter on %d planes (one atomic commit per refresh)", len(ids))
			}
		}
	})
	return kmsDisp
}

// planeSinkWanted: CUTEPI_WALL_SINK=planesink asks for the plane wall with
// CuTePi's presenter (DESIGN §6.1.4) instead of a kmssink per layer.
func planeSinkWanted() bool { return wallVideoSink() == "planesink" }

// layerSink is the plane wall's sink element: the presenter's when it runs.
func layerSink() string {
	if planewall.IsOpen() {
		return "cutepiplanesink"
	}
	return "kmssink"
}

// DisplayMode reports the wall's resolution and refresh rate (0s when not
// known), e.g. for the test-pattern overlay.
func DisplayMode() (w, h, hz int) {
	if k := kmsWall(); k != nil {
		return k.Width, k.Height, k.Refresh
	}
	return fbSize()
}

// fbSize reads the framebuffer size (fbdev wall); refresh is unknown there.
func fbSize() (w, h, hz int) {
	sys := "/sys/class/graphics/" + strings.TrimPrefix(wallFramebuffer(), "/dev/")
	if b, err := readFileTrim(sys + "/virtual_size"); err == nil {
		parts := strings.Split(b, ",")
		if len(parts) == 2 {
			w, _ = strconv.Atoi(parts[0])
			h, _ = strconv.Atoi(parts[1])
		}
	}
	return w, h, 0
}

func layerOf(p *gst.Pipeline) *wallLayer {
	if p == nil {
		return nil
	}
	layersMu.Lock()
	defer layersMu.Unlock()
	return layers[p]
}

// newWallLayer claims a plane for p's video. It starts fully transparent:
// nothing appears until the pipeline is activated (showLayer).
func newWallLayer(p *gst.Pipeline, opts LoadOpts) (*wallLayer, error) {
	w := kmsWall()
	if w == nil {
		return nil, fmt.Errorf("no KMS wall")
	}
	plane := w.acquire()
	if plane == nil {
		return nil, fmt.Errorf("all %d display layers are in use", len(w.planes))
	}
	l := &wallLayer{p: p, plane: plane, opacity: opacityOf(opts), kick: make(chan struct{}, 1), quit: make(chan struct{})}
	_ = w.setAlpha(plane, 0)
	go l.writer(w)
	bits, ok := rotationBits(opts.Rotation, opts.Flip)
	if !ok {
		bits = rotate0 // 90/270 are rotated in software before the sink
		if opts.Flip == "h" {
			bits |= reflectX
		} else if opts.Flip == "v" {
			bits |= reflectY
		}
	}
	if err := w.set(plane, "rotation", bits); err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: plane rotation %d: %v", bits, err)
	}
	// Per-pixel alpha (ProRes 4444, PNG, GIF, ...) is straight alpha; opaque
	// formats have none, so Coverage changes nothing for them (§6.1.3).
	if err := w.set(plane, "pixel blend mode", blendCoverage); err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: plane blend mode: %v", err)
	}
	layersMu.Lock()
	layers[p] = l
	layersMu.Unlock()
	return l, nil
}

func opacityOf(opts LoadOpts) float64 {
	if opts.Opacity <= 0 || opts.Opacity > 1 {
		return 1
	}
	return opts.Opacity
}

// showLayer puts p's layer on screen at alpha opacity*level, at its place in
// the visible stack (§6.1.2): LayerBottom under everything (a cue that stops
// the others starts under the cues fading out), LayerTop above everything,
// LayerUnder directly beneath ref's layer (on top if ref has none).
func showLayer(p *gst.Pipeline, level float64, at string, ref *gst.Pipeline) {
	if glOpen {
		glShow(p, level, at, ref)
		return
	}
	w := kmsWall()
	l := layerOf(p)
	if w == nil || l == nil {
		return
	}
	layersMu.Lock()
	if !l.visible {
		l.visible = true
		stack = insertLayer(stack, l, stackIndex(stack, layers[ref], at))
	}
	restackLocked(w)
	layersMu.Unlock()
	setLayerLevel(p, level)
}

// stackIndex is where a layer placed at `at` goes in a bottom-to-top stack;
// refLayer is LayerUnder's reference (nil or absent: on top).
func stackIndex[T comparable](st []T, refLayer T, at string) int {
	switch at {
	case LayerBottom:
		return 0
	case LayerUnder:
		var zero T
		if refLayer != zero {
			for i, x := range st {
				if x == refLayer {
					return i
				}
			}
		}
	}
	return len(st)
}

// insertLayer inserts l at index i of st.
func insertLayer[T any](st []T, l T, i int) []T {
	st = append(st, l)
	copy(st[i+1:], st[i:])
	st[i] = l
	return st
}

// stackPosition is p's layer's place in the visible stack, 1 = bottom; 0
// when it has no visible layer.
func stackPosition(p *gst.Pipeline) int {
	if glOpen {
		glMu.Lock()
		defer glMu.Unlock()
		for i, l := range glStack {
			if glLayers[p] == l {
				return i + 1
			}
		}
		return 0
	}
	layersMu.Lock()
	defer layersMu.Unlock()
	for i, l := range stack {
		if layers[p] == l {
			return i + 1
		}
	}
	return 0
}

// zposTop is the highest plane zpos (the vc4 range is 1..17). Visible
// layers use 1..n with n at most one less than the planes, so a layer parked
// here is above all of them.
const zposTop = 17

// parkLayer puts p's (invisible, alpha 0) layer at the top zpos, so raising
// it later is a single alpha commit.
func parkLayer(p *gst.Pipeline) bool {
	if glOpen {
		return glPark(p)
	}
	w := kmsWall()
	l := layerOf(p)
	if w == nil || l == nil {
		return false
	}
	if err := w.set(l.plane, "zpos", zposTop); err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: park plane at zpos %d: %v", zposTop, err)
		return false
	}
	layersMu.Lock()
	l.parked = true
	layersMu.Unlock()
	return true
}

// raiseLayer puts p's layer on screen ABOVE every other layer at full
// opacity, written directly (not through the paced writer) so it lands on
// the next refresh: the panic cut. A parked layer is already on top, so it
// takes one commit (alpha); otherwise zpos goes first.
func raiseLayer(p *gst.Pipeline) {
	if glOpen {
		glRaise(p)
		return
	}
	w := kmsWall()
	l := layerOf(p)
	if w == nil || l == nil {
		return
	}
	layersMu.Lock()
	if l.visible {
		for i, s := range stack {
			if s == l {
				stack = append(stack[:i], stack[i+1:]...)
				break
			}
		}
	}
	l.visible = true
	stack = append(stack, l)
	z := len(stack)
	parked := l.parked
	l.parked = false // restacks now own its zpos
	layersMu.Unlock()
	if !parked {
		if err := w.set(l.plane, "zpos", uint64(z)); err != nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: plane zpos: %v", err)
		}
	}
	_ = w.setAlpha(l.plane, l.opacity)
	l.post(l.opacity) // keep the writer's view in step
}

// restackLocked writes zpos bottom->top for the visible stack. Overlay zpos
// starts at 1 (0 is the primary console plane). Caller holds layersMu.
func restackLocked(w *KMSWall) {
	for i, l := range stack {
		if err := w.set(l.plane, "zpos", uint64(i+1)); err != nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: plane zpos: %v", err)
		}
	}
}

// setLayerRamp applies a fade in progress: level from -> to over dur along
// curve, started at start. The GPU wall evaluates it for every output frame;
// the plane wall takes the level for now.
func setLayerRamp(p *gst.Pipeline, from, to float64, start time.Time, dur time.Duration, curve string) {
	if glOpen {
		glRamp(p, from, to, start, dur, curve)
		return
	}
	setLayerLevel(p, rampLevel(from, to, time.Since(start), dur, curve))
}

// rampLevel is a fade's level `elapsed` into it: from before it starts, to
// once done (also for a zero-length fade: 0/0 gave NaN).
func rampLevel(from, to float64, elapsed, dur time.Duration, curve string) float64 {
	t := 1.0
	if dur > 0 {
		t = math.Max(0, math.Min(1, float64(elapsed)/float64(dur)))
	}
	return from + (to-from)*fadeShape(curve, t)
}

// setLayerLevel applies a fade level (0..1) to p's layer: alpha = opacity x level.
func setLayerLevel(p *gst.Pipeline, level float64) {
	if glOpen {
		glSetLevel(p, level)
		return
	}
	w := kmsWall()
	l := layerOf(p)
	if w == nil || l == nil || !l.visible {
		return
	}
	l.post(l.opacity * level)
}

// dropLayer frees p's plane. Call after p is in NULL (kmssink has released it).
func dropLayer(p *gst.Pipeline) {
	if glOpen {
		glDrop(p)
		return
	}
	w := kmsWall()
	layersMu.Lock()
	l := layers[p]
	delete(layers, p)
	if l != nil && l.visible {
		for i, s := range stack {
			if s == l {
				stack = append(stack[:i], stack[i+1:]...)
				break
			}
		}
	}
	layersMu.Unlock()
	if w != nil && l != nil {
		close(l.quit)
		_ = w.setAlpha(l.plane, 0)
		if planewall.IsOpen() {
			planewall.Detach(l.plane.id) // normally done when its sink stopped
		}
		w.release(l.plane)
	}
}

// outgoing is a cue fading out over the new one (crossfade).
type outgoing struct {
	p        *gst.Pipeline
	volumeEl *gst.Element
	gain     float64 // audio gain when the fade began
	level    float64 // video level when the fade began
	curve    string
	done     chan struct{}
	start    chan struct{} // closed once the incoming cue is on screen
	wait     time.Duration // longest wait for start before fading anyway
}

var (
	pendingMu  sync.Mutex
	pendingOut = map[*gst.Pipeline][]*outgoing{} // incoming pipeline -> cues waiting to fade over it
)

// awaitIncoming parks o until incoming is on screen, so the old picture
// never fades over black while the new cue prerolls.
func awaitIncoming(incoming *gst.Pipeline, o *outgoing) {
	o.start = make(chan struct{})
	pendingMu.Lock()
	pendingOut[incoming] = append(pendingOut[incoming], o)
	pendingMu.Unlock()
}

// incomingShown releases the fades waiting on p (it is on screen, failed or
// was retired: either way the old cues must not wait any longer).
func incomingShown(p *gst.Pipeline) {
	pendingMu.Lock()
	list := pendingOut[p]
	delete(pendingOut, p)
	pendingMu.Unlock()
	for _, o := range list {
		close(o.start)
	}
}

var (
	outMu    sync.Mutex
	outgoers = map[*gst.Pipeline]*outgoing{}
)

// fadeOutgoing ramps an outgoing cue's picture and sound to nothing over
// durMs at the display's pace, then retires it.
func fadeOutgoing(o *outgoing, durMs int) {
	outMu.Lock()
	outgoers[o.p] = o
	outMu.Unlock()
	defer func() {
		outMu.Lock()
		delete(outgoers, o.p)
		outMu.Unlock()
		retirePipeline(o.p)
	}()
	if o.start != nil {
		select {
		case <-o.start:
		case <-o.done:
			return
		case <-time.After(o.wait): // the new cue never showed; fade anyway
		}
	}
	total := time.Duration(durMs) * time.Millisecond
	start := time.Now()
	for {
		select {
		case <-o.done:
			return
		case <-time.After(fadeTick):
		}
		t := math.Min(1, float64(time.Since(start))/float64(total))
		k := 1 - fadeShape(o.curve, t)
		setLayerRamp(o.p, o.level, 0, start, total, o.curve)
		if o.volumeEl != nil {
			o.volumeEl.Set("volume", o.gain*k)
		}
		if t >= 1 {
			time.Sleep(fadeLand()) // let the last alpha land before teardown
			return
		}
	}
}

// retireOutgoing cuts every fading-out cue immediately (Stop/Panic).
func retireOutgoing() {
	outMu.Lock()
	list := make([]*outgoing, 0, len(outgoers))
	for _, o := range outgoers {
		list = append(list, o)
	}
	outMu.Unlock()
	for _, o := range list {
		select {
		case <-o.done:
		default:
			close(o.done)
		}
	}
	// fadeOutgoing retires them on its way out; wait briefly so Stop/Panic
	// returns with the wall already clear.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		n := len(outgoers)
		outMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fadeTick paces fade loops at twice the display refresh. Levels are
// computed from elapsed time, so a fade always takes its stated duration;
// each layer's alpha writer keeps only the latest level, so posting faster
// than the refresh means a fresh level is waiting at every vblank (at 16 ms,
// plus the loop's own work, levels came slower than 60 Hz and a still's
// fade missed refreshes).
const fadeTick = 8 * time.Millisecond

// alphaLand is how long a fade waits after its last level before tearing
// the layer down, so the final alpha (0) is on screen first: the writer may
// be mid-step (up to two refreshes) plus the commit's own vblank wait.
const alphaLand = 64 * time.Millisecond

// glWallDelay is how much later the GPU wall shows a frame than its time:
// the mixer's latency and the wall sink's wait (measured 87 ms). A fade
// there is stepped per output frame from each frame's own time, so its last
// step reaches the screen this much after the fade ends.
const glWallDelay = 100 * time.Millisecond

// fadeLand is how long to wait after a fade's last level before teardown.
func fadeLand() time.Duration {
	if glOpen {
		return alphaLand + glWallDelay
	}
	return alphaLand
}

// wallRect resolves a cue's geometry (x, y, width, height; each "" for the
// default, "N" or "Npx" for pixels, or "N%" of the display) to a rectangle.
// Defaults fill the display.
func wallRect(opts LoadOpts, dw, dh int) (x, y, w, h int) {
	x = geomValue(opts.GeomX, dw, 0)
	y = geomValue(opts.GeomY, dh, 0)
	w = geomValue(opts.GeomW, dw, dw)
	h = geomValue(opts.GeomH, dh, dh)
	if w <= 0 {
		w = dw
	}
	if h <= 0 {
		h = dh
	}
	return
}

func geomValue(s string, full, def int) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return def
	}
	if strings.HasSuffix(s, "%") {
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil {
			return def
		}
		return int(math.Round(f / 100 * float64(full)))
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
	if err != nil {
		return def
	}
	return int(math.Round(f))
}

// kmsVideoTail names the video output chain for the KMS wall. Hardware
// decoders hand DMABuf frames straight to the plane (no copy, no
// conversion); system-memory frames (software decode, stills, test
// patterns) go through one converter. 90/270° rotation is the only software
// stage: the display controller rotates 0/180° and mirrors by itself.
// "stretch" fills the box by rewriting the pixel aspect ratio (capssetter);
// "fit" letterboxes inside it, which kmssink does natively.
func kmsVideoTail(dmabuf bool, opts LoadOpts) []string {
	var names []string
	names = append(names, "queue")
	_, hwRotate := rotationBits(opts.Rotation, opts.Flip)
	if !hwRotate {
		// Scale to the final on-screen size BEFORE the software rotation:
		// v4l2convert does it in hardware for decoder frames, so videoflip
		// only turns the pixels that are actually shown.
		// I420 at that size, then identity drop-allocation so v4l2convert
		// fills its own buffers instead of uncached ones videoflip would read
		// slowly (measured 1080p: 15 fps -> 46 fps).
		if dmabuf {
			names = append(names, videoDownload()...)
			names = append(names, "capsfilter", "identity")
		} else {
			names = append(names, packStage()...)
			names = append(names, "videoconvert", "videoscale", "capsfilter")
		}
		names = append(names, "videocrop", "videoflip", "videoconvert")
	} else {
		if !dmabuf {
			// Converted frames are limited to layouts the display can
			// allocate (kmsSysmemCaps). 10-bit and 4:2:2 frames are packed to
			// I420 first (packStage).
			names = append(names, packStage()...)
			names = append(names, "videoconvert", "capsfilter")
		}
		// On decoder frames videocrop only attaches crop metadata: the plane
		// scans out the sub-rectangle, no pixels are copied (measured 30 fps).
		names = append(names, "videocrop")
	}
	if opts.FitMode == "stretch" {
		names = append(names, "capssetter")
	}
	return append(names, layerSink())
}

// packStage is cutepiyuvpack (gsp/yuvpack) where it registers: 10-bit
// 4:2:0/4:2:2 and 8-bit 4:2:2 frames become I420 in one NEON pass (1080p
// 10-bit 4:2:2: 2.35 ms against 20 ms for videoconvert to I420, which
// otherwise picked RGB for them); every other format passes through.
func packStage() []string {
	if yuvpack.Register() {
		return []string{"cutepiyuvpack"}
	}
	return nil
}

// kmsSysmemCaps are the layouts a software-converted frame may take on its
// way to a display plane. The planes also list 4:2:2 and 4:4:4 YUV, but the
// kernel cannot allocate dumb buffers for them ("failed to activate
// bufferpool": DNxHR and ProRes 422 never prerolled, codec corpus).
// videoconvert picks the least lossy of these, so 4:2:0 sources pass through
// unconverted and 4:2:2/4:4:4 sources go to RGB with full chroma.
const kmsSysmemCaps = "video/x-raw,format={I420,YV12,NV12,NV21,BGRx,BGRA,RGBx,RGBA,xRGB,ARGB,xBGR,ABGR,RGB16}"

// frameLayout is how a frame is cropped and sized for its box on the wall.
type frameLayout struct {
	cropL, cropR, cropT, cropB int // pixels of the frame videocrop sees
	preW, preH                 int // 90/270 path: pre-rotation scaled size
	outW, outH                 int // shown frame size after crop and rotation (stretch aspect)
}

// computeLayout applies the cue's crop (source pixels or % per edge), then the
// fit mode inside the bw x bh box:
//
//	fit          whole picture inside the box, aspect kept (letterbox)
//	stretch      fills the box, aspect not kept
//	fill-width   picture as wide as the box; top/bottom overflow is cropped
//	fill-height  picture as tall as the box; left/right overflow is cropped
//	fill         covers the box; whichever side overflows is cropped
//
// Overflow crops are centred. swRotate: the frame is scaled (preW x preH)
// before videocrop, so crops are expressed in that scaled frame.
func computeLayout(opts LoadOpts, srcW, srcH, bw, bh int, swRotate bool) frameLayout {
	var l frameLayout
	if srcW <= 0 || srcH <= 0 || bw <= 0 || bh <= 0 {
		return l
	}
	cl, cr := cropPx(opts.CropL, srcW), cropPx(opts.CropR, srcW)
	ct, cb := cropPx(opts.CropT, srcH), cropPx(opts.CropB, srcH)
	// Keep at least 16 px of picture whatever the crop asks.
	if cl+cr > srcW-16 {
		cl, cr = 0, 0
	}
	if ct+cb > srcH-16 {
		ct, cb = 0, 0
	}
	rot := opts.Rotation == 90 || opts.Rotation == 270
	ow, oh := float64(srcW-cl-cr), float64(srcH-ct-cb) // as shown (oriented)
	if rot {
		ow, oh = oh, ow
	}
	fbw, fbh := float64(bw), float64(bh)
	var ex, ey float64 // extra crop per side, oriented pixels
	switch opts.FitMode {
	case "fill-width":
		if s := fbw / ow; oh*s > fbh {
			ey = (oh - fbh/s) / 2
		}
	case "fill-height":
		if s := fbh / oh; ow*s > fbw {
			ex = (ow - fbw/s) / 2
		}
	case "fill":
		s := math.Max(fbw/ow, fbh/oh)
		if ow*s > fbw {
			ex = (ow - fbw/s) / 2
		}
		if oh*s > fbh {
			ey = (oh - fbh/s) / 2
		}
	}
	fcl, fcr, fct, fcb := float64(cl), float64(cr), float64(ct), float64(cb)
	if rot {
		fcl, fcr, fct, fcb = fcl+ey, fcr+ey, fct+ex, fcb+ex
	} else {
		fcl, fcr, fct, fcb = fcl+ex, fcr+ex, fct+ey, fcb+ey
	}
	fw, fh := ow-2*ex, oh-2*ey
	k := 1.0
	if swRotate {
		// Scale so the shown (cropped, turned) region lands at box size.
		k = math.Min(fbw/fw, fbh/fh)
		l.preW, l.preH = int(float64(srcW)*k)&^1, int(float64(srcH)*k)&^1
	}
	l.cropL, l.cropR = int(fcl*k), int(fcr*k)
	l.cropT, l.cropB = int(fct*k), int(fcb*k)
	l.outW, l.outH = int(fw*k), int(fh*k)
	return l
}

// cropPx reads a crop edge: "" none, "N" / "Npx" pixels, "N%" of full.
func cropPx(v string, full int) int {
	n := geomValue(v, full, 0)
	if n < 0 {
		return 0
	}
	return n
}

// configureKMSTail wires the chain built from kmsVideoTail: plane, shared fd,
// box, crop, rotation and stretch. srcW/srcH are the decoded frame size.
func configureKMSTail(p *gst.Pipeline, byFactory map[string][]*gst.Element, opts LoadOpts, srcW, srcH int, still bool) error {
	w := kmsWall()
	sink := firstByFactory(byFactory, "kmssink")
	planeSink := sink == nil
	if planeSink {
		sink = firstByFactory(byFactory, "cutepiplanesink")
	}
	if w == nil || sink == nil {
		return fmt.Errorf("kms tail without a KMS wall")
	}
	l, err := newWallLayer(p, opts)
	if err != nil {
		return err
	}
	l.sink.Store(sink)
	l.still.Store(still)
	sink.Set("plane-id", int(l.plane.id))
	if !planeSink {
		sink.Set("fd", w.fd)
		sink.Set("skip-vsync", true) // one vsync waiter per DRM fd: several sinks share it
	}
	// First-frame flag: a live page has no preroll, so its show waits on this.
	sink.GetStaticPad("sink").AddProbe(gst.PadProbeTypeBuffer, func(*gst.Pad, *gst.PadProbeInfo) gst.PadProbeReturn {
		l.framed.Store(true)
		return gst.PadProbeRemove
	})
	x, y, bw, bh := wallRect(opts, w.Width, w.Height)
	// kmssink fits the (cropped) frame inside this box, aspect kept.
	sink.SetArg("render-rectangle", fmt.Sprintf("<%d,%d,%d,%d>", x, y, bw, bh))
	if id := firstByFactory(byFactory, "identity"); id != nil {
		id.Set("drop-allocation", true)
	}
	flip := firstByFactory(byFactory, "videoflip")
	lay := computeLayout(opts, srcW, srcH, bw, bh, flip != nil)
	if flip != nil {
		flip.SetArg("method", rotationMethod(opts.Rotation)) // enum: SetArg parses the nick; Set(string) is silently ignored
		if cf := firstByFactory(byFactory, "capsfilter"); cf != nil && lay.preW > 0 && lay.preH > 0 {
			cf.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,pixel-aspect-ratio=1/1", lay.preW, lay.preH)))
		}
	} else if cf := firstByFactory(byFactory, "capsfilter"); cf != nil {
		cf.Set("caps", gst.NewCapsFromString(kmsSysmemCaps))
	}
	if vc := firstByFactory(byFactory, "videocrop"); vc != nil {
		vc.Set("left", lay.cropL)
		vc.Set("right", lay.cropR)
		vc.Set("top", lay.cropT)
		vc.Set("bottom", lay.cropB)
	}
	if cs := firstByFactory(byFactory, "capssetter"); cs != nil && lay.outW > 0 && lay.outH > 0 {
		// Pixel aspect that makes the shown frame's aspect equal the box's.
		fw, fh := lay.outW, lay.outH // as shown (after crop and turn)
		num, den := bw*fh, bh*fw
		g := gcd(num, den)
		cs.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,pixel-aspect-ratio=%d/%d", num/g, den/g)))
		cs.Set("join", true)
		cs.Set("replace", false)
	}
	return nil
}

func readFileTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// capsSize reads width/height from the first caps structure (0s if absent).
// capsFormat reads the raw video format name from caps ("" when unknown).
func capsFormat(caps *gst.Caps) string {
	if caps == nil || caps.GetSize() == 0 {
		return ""
	}
	if v, err := caps.GetStructureAt(0).GetValue("format"); err == nil {
		f, _ := v.(string)
		return f
	}
	return ""
}

func capsSize(caps *gst.Caps) (w, h int) {
	if caps == nil || caps.GetSize() == 0 {
		return 0, 0
	}
	st := caps.GetStructureAt(0)
	if v, err := st.GetValue("width"); err == nil {
		w, _ = v.(int)
	}
	if v, err := st.GetValue("height"); err == nil {
		h, _ = v.(int)
	}
	return w, h
}

var testOverlay atomic.Bool

// SetTestOverlay turns the display-mode label on test patterns on or off.
func SetTestOverlay(on bool) { testOverlay.Store(on) }

// TestOverlay reports whether test patterns carry the display-mode label.
func TestOverlay() bool { return testOverlay.Load() }

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// testPatternRate is how many frames a second a test pattern is generated at.
// The display scans a layer out at its own refresh whatever the source rate,
// so static patterns need only a few frames (1080p generation and copy is the
// CPU cost); moving ones run at 30; Blink alternates at the display rate
// because it is the refresh check.
func testPatternRate(pattern string, displayHz int) int {
	if displayHz <= 0 {
		displayHz = 60
	}
	switch pattern {
	case "blink":
		return displayHz
	case "snow", "ball", "chroma-zone-plate", "zone-plate":
		return 30
	}
	return 5
}
