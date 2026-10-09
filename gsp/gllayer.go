package gsp

// Cue layers on the GPU wall (DESIGN §6.1.1). With CUTEPI_WALL=gl a cue's
// video tail ends in an appsink instead of a display plane, and once the cue
// has prerolled the wall attaches it as a mixer layer: opacity, fades,
// stacking and geometry are mixer pad properties applied on every output
// frame. The manager's layer calls (showLayer, setLayerLevel, parkLayer,
// raiseLayer, dropLayer) branch here when the GL wall is open.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// glLayer is a cue's place on the GPU wall.
type glLayer struct {
	appsink *gst.Element
	layer   *glwall.Layer // nil until attached (after preroll)
	opts    LoadOpts
	opacity float64
	visible bool
	parked  bool // armed panic image: attached at the top, alpha 0
	level   float64
	seq     uint64 // attach order (newest highest)
	route   string // "dmabuf" (hardware), "alpha" (RGBA upload) or "isp"
	caps    string // the frames' caps at the bridge, once attached
	srcW    int    // the cue's picture size (before any flip), once attached
	srcH    int
}

// glRoute names a GL tail (glVideoTail) by what reaches its appsink: the
// decoder's DMABufs ("dmabuf"), the ISP's YU12 DMABufs ("isp"), or RGBA in
// RAM that the wall copies ("alpha"). A turned hardware cue's tail starts
// with an ISP scale but ends in RGBA, so the last converter decides.
func glRoute(names []string) string {
	for i := len(names) - 1; i >= 0; i-- {
		switch names[i] {
		case "v4l2convert":
			return "isp"
		case "videoconvert", "videoflip", "videoscale":
			return "alpha"
		}
	}
	return "dmabuf"
}

var glSeq uint64

var (
	glMu     sync.Mutex
	glLayers = map[*gst.Pipeline]*glLayer{}
	glStack  []*glLayer // visible layers, bottom -> top
)

// glDecoderPool is the pool-size hint the bridge gives a cue's decoder: a
// V4L2 decoder sizes its output pool from it and stalls when it is too
// small; the ISP copies every frame to system memory above 31.
const glDecoderPool = 16

// glParkedZ is the mixer zorder of an armed panic image: above anything the
// visible stack can reach.
const glParkedZ = 1000

// glDmaBufCapable reports whether the decoder behind srcPad can hand over
// DMABufs. decodebin exposes a stateless V4L2 decoder's pad with its
// system-memory tiled caps (NV12_128C8) before anything downstream exists
// and only renegotiates to DMABuf once a DMABuf-accepting tail is linked, so
// the current caps say "software"; what the pad can produce is asked for
// instead. Hardware decoders list memory:DMABuf there, software ones do not.
func glDmaBufCapable(srcPad *gst.Pad) bool {
	if srcPad == nil {
		return true
	}
	caps := srcPad.QueryCaps(nil)
	if caps == nil {
		return dmaBufUpstream(srcPad)
	}
	for i := 0; i < caps.GetSize(); i++ {
		if f := caps.GetFeaturesAt(i); f != nil && f.Contains("memory:DMABuf") {
			return true
		}
	}
	return false
}

// glISPCanTake: a hardware decoder whose frames the ISP (v4l2convert) can
// scale. The HEVC decoder's tiled NV12 (NV12_128C8, Broadcom SAND) has no
// GStreamer mapping in v4l2convert, so it takes the CPU path instead.
func glISPCanTake(srcPad *gst.Pad) bool {
	if !glDmaBufCapable(srcPad) {
		return false
	}
	if caps := srcPad.QueryCaps(nil); caps != nil {
		c := caps.String()
		if strings.Contains(c, "128C8") || strings.Contains(c, ":0x07") {
			return false
		}
	}
	return true
}

// hasAlphaFormat reports a raw video format with an alpha channel.
func hasAlphaFormat(f string) bool {
	switch f {
	case "RGBA", "BGRA", "ARGB", "ABGR", "AYUV", "VUYA", "GBRA", "A420", "A422", "A444", "RGBA64_LE", "ARGB64":
		return true
	}
	return strings.HasPrefix(f, "A4") || strings.HasPrefix(f, "GBRA") || strings.HasPrefix(f, "RGBA")
}

// glVideoTail is the cue's video chain to the wall: hardware frames
// (DMABuf) go straight to the appsink; frames with alpha are uploaded from
// system memory (the ISP would drop the alpha); everything else goes
// through the ISP, which writes YU12 DMABufs the GPU samples directly (a
// CPU repack first only when the decoder's format is not one the ISP takes).
func glVideoTail(dmabuf bool, format string, flip, hw bool) []string {
	switch {
	case dmabuf:
		return []string{"queue", "capsfilter", "appsink"}
	case flip:
		// Rotation/mirror on the CPU, as the plane wall's 90/270: scale to
		// the size shown first (the ISP for decoder frames, then identity so
		// it fills its own cached buffers), crop, turn only those pixels.
		var names []string
		hwDown := hw && !hasAlphaFormat(format) && len(videoDownload()) > 0
		if hwDown {
			names = append([]string{"queue"}, videoDownload()...)
			names = append(names, "capsfilter", "identity")
		} else {
			names = append(append([]string{"queue"}, packStage()...), "videoconvert", "videoscale", "capsfilter")
		}
		names = append(names, "videocrop", "videoflip")
		switch {
		case hasAlphaFormat(format):
			return append(names, "appsink") // RGBA in RAM: the wall copies it (linear slots)
		case hwDown:
			// A second ISP pass beside a hardware decoder starved both
			// (H.264 turned: 9.7 fps); the turned frame is small, so RGBA on
			// the CPU and the wall's copy route.
			return append(names, "videoconvert", "capsfilter", "appsink")
		}
		return append(names, "v4l2convert", "capsfilter", "appsink") // to YU12 DMABufs, as the ISP route
	case hasAlphaFormat(format):
		return []string{"queue", "videoconvert", "capsfilter", "appsink"}
	default:
		// 10-bit and 4:2:2 frames are packed to I420 by NEON first (packStage):
		// videoconvert then passes them to the ISP untouched.
		return append(append([]string{"queue"}, packStage()...), "videoconvert", "capsfilter", "v4l2convert", "capsfilter", "appsink")
	}
}

// glDirNick is videoflip's video-direction nick for d.
var glDirNick = map[videoDir]string{dirIdentity: "identity", dir90R: "90r", dir180: "180", dir90L: "90l",
	dirHoriz: "horiz", dirVert: "vert", dirULLR: "ul-lr", dirURLL: "ur-ll"}

// glConfigureFlip sets up a turning tail (glVideoTail with flip). The
// frame's size is often unknown when decodebin3 exposes its pad, so the
// pre-scale and crop (computeLayout, as the plane wall) are set when the
// first caps leave the tail's queue, before anything after it negotiates.
func glConfigureFlip(byFactory map[string][]*gst.Element, opts LoadOpts, dir videoDir, alpha bool) {
	caps := byFactory["capsfilter"]
	hwDown := len(byFactory["identity"]) > 0 // glVideoTail's ISP pre-scale: RGBA at the end
	if vf := firstByFactory(byFactory, "videoflip"); vf != nil {
		vf.SetArg("video-direction", glDirNick[dir])
	}
	if id := firstByFactory(byFactory, "identity"); id != nil {
		id.Set("drop-allocation", true)
	}
	switch {
	case hwDown && len(caps) > 1:
		caps[1].Set("caps", gst.NewCapsFromString("video/x-raw,format=RGBA"))
	case !alpha && len(caps) > 1:
		caps[1].Set("caps", gst.NewCapsFromString("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=YU12"))
		if cv := byFactory["v4l2convert"]; len(cv) > 0 {
			cv[len(cv)-1].SetArg("capture-io-mode", "dmabuf") // the one feeding the wall
		}
	}
	format := "I420"
	if alpha {
		format = "RGBA"
	}
	pre, crop := caps[0], firstByFactory(byFactory, "videocrop")
	q := firstByFactory(byFactory, "queue")
	if q == nil || pre == nil || crop == nil {
		return
	}
	src := q.GetStaticPad("src")
	var once sync.Once
	src.AddProbe(gst.PadProbeTypeEventDownstream, func(_ *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		ev := info.GetEvent()
		if ev == nil || ev.Type() != gst.EventTypeCaps {
			return gst.PadProbeOK
		}
		sw, sh := capsSize(ev.ParseCaps())
		w := kmsWall()
		if sw <= 0 || sh <= 0 || w == nil {
			return gst.PadProbeOK
		}
		once.Do(func() {
			_, _, bw, bh := wallRect(opts, w.Width, w.Height)
			lay := computeLayout(opts, sw, sh, bw, bh, dir.transposes())
			pw, ph := lay.preW, lay.preH
			if pw <= 0 || ph <= 0 { // not turned a quarter: crop at full size
				pw, ph = sw&^1, sh&^1
			}
			pre.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=%s,width=%d,height=%d,pixel-aspect-ratio=1/1", format, pw, ph)))
			crop.Set("left", lay.cropL)
			crop.Set("right", lay.cropR)
			crop.Set("top", lay.cropT)
			crop.Set("bottom", lay.cropB)
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall flip %s: %dx%d -> %dx%d, crop %d/%d/%d/%d", glDirNick[dir], sw, sh, pw, ph, lay.cropL, lay.cropR, lay.cropT, lay.cropB)
		})
		return gst.PadProbeOK
	})
}

// glTailDMABuf: the cue's frames go to the wall as the decoder's own DMABufs
// (hardware decoders), unless they must be turned or mirrored first, which
// happens on the CPU.
func glTailDMABuf(srcPad *gst.Pad, opts LoadOpts, isTest bool) bool {
	return !isTest && glDirection(opts.Rotation, opts.Flip) == dirIdentity && glDmaBufCapable(srcPad)
}

// AlphaLookup reports whether a media file's video carries an alpha channel,
// from its import metadata (set by main once the database is open). The GL
// tail needs it when the decoder's pad appears before its format is fixed
// (decodebin3 exposes software decoders' pads that way): without it an alpha
// video would take the ISP route, which has no alpha.
var AlphaLookup func(filename string) bool

// glTailFormat is the format the GL tail is chosen by: the pad's own when
// fixed, else "RGBA" for a file recorded as having alpha. Unfixed caps (a
// caps query's answer) list what the decoder could produce, not what it
// will: gdkpixbufdec offers RGB first and delivers RGBA for a TIFF with alpha.
func glTailFormat(caps *gst.Caps, filename string) string {
	if caps != nil && caps.IsFixed() {
		if f := capsFormat(caps); f != "" {
			return f
		}
	}
	if AlphaLookup != nil && filename != "" && AlphaLookup(filename) {
		return "RGBA"
	}
	return ""
}

// ispInputCaps are the formats the ISP accepts; videoconvert passes a
// matching source through untouched and repacks 10-bit or planar 4:2:2.
const ispInputCaps = "video/x-raw,format={I420,YV12,NV12,NV21,YUY2,UYVY,BGRx,RGB,BGR}"

// configureGLTail sets the tail up and registers the cue's layer record
// (attached later by glShow). Called from the pad-added handler with the
// elements just created from glVideoTail's names.
func configureGLTail(p *gst.Pipeline, byFactory map[string][]*gst.Element, names []string, opts LoadOpts, dmabuf bool, format string) error {
	sink := firstByFactory(byFactory, "appsink")
	if sink == nil {
		return fmt.Errorf("gl tail without an appsink")
	}
	sink.Set("sync", false) // push ahead; the wall takes each frame when due
	sink.Set("max-buffers", uint(2))
	sink.Set("drop", false)
	sink.Set("enable-last-sample", false)
	sink.Set("emit-signals", false)
	caps := byFactory["capsfilter"]
	dir := glDirection(opts.Rotation, opts.Flip)
	switch {
	case dmabuf:
		caps[0].Set("caps", gst.NewCapsFromString("video/x-raw(memory:DMABuf)"))
	case dir != dirIdentity:
		glConfigureFlip(byFactory, opts, dir, hasAlphaFormat(format))
	case hasAlphaFormat(format):
		caps[0].Set("caps", gst.NewCapsFromString("video/x-raw,format=RGBA"))
	default:
		caps[0].Set("caps", gst.NewCapsFromString(ispInputCaps))
		caps[1].Set("caps", gst.NewCapsFromString("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=YU12"))
		if vc := firstByFactory(byFactory, "v4l2convert"); vc != nil {
			vc.SetArg("capture-io-mode", "dmabuf")
		}
	}
	if conv := firstByFactory(byFactory, "videoconvert"); conv != nil {
		conv.Set("n-threads", uint(4))
	}
	glwall.PrepareSink(sink, glDecoderPool)
	logs.Printf(logs.GSPPipeDebug, "gsp: GL wall tail %v (dmabuf=%v format=%q)", names, dmabuf, format)
	return nil
}

// glRegister records the cue's layer once its tail is in the pipeline and
// synced to its state (showLayer may be waiting for it; an appsink still in
// NULL would answer the preroll pull with nothing).
func glRegister(p *gst.Pipeline, byFactory map[string][]*gst.Element, names []string, opts LoadOpts) {
	sink := firstByFactory(byFactory, "appsink")
	if sink == nil {
		return
	}
	glMu.Lock()
	if glLayers[p] != nil {
		// A second video pad for the same cue (decodebin3 can expose one per
		// stream): the first tail is the layer; this one drains nowhere.
		logs.Printf(logs.GSPPipeDebug, "gsp: GL wall: second video tail for a cue ignored")
		glMu.Unlock()
		return
	}
	glLayers[p] = &glLayer{appsink: sink, opts: opts, opacity: opacityOf(opts), level: 1, route: glRoute(names)}
	glMu.Unlock()
}

// glLayerWait is how long showLayer waits for a cue's tail to exist:
// decodebin3 adds its pads after the pipeline reports PAUSED, so the tail
// (and the appsink's preroll) can arrive after startPlayback's own wait.
const glLayerWait = 10 * time.Second

// glAwait returns the cue's layer record, waiting for the pad-added handler
// to create it. nil when the cue has no video or was retired meanwhile.
func glAwait(p *gst.Pipeline) *glLayer {
	deadline := time.Now().Add(glLayerWait)
	for {
		glMu.Lock()
		l := glLayers[p]
		glMu.Unlock()
		if l != nil || time.Now().After(deadline) {
			return l
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// glAttach joins the (prerolled) cue to the wall if it is not attached yet.
// Caller holds glMu.
func glAttachLocked(p *gst.Pipeline, l *glLayer) bool {
	if l.layer != nil {
		return true
	}
	dir := glDirection(l.opts.Rotation, l.opts.Flip)
	layer, err := glwall.Attach(l.appsink, "bt709")
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: GL wall layer: %v", err)
		return false
	}
	l.layer = layer
	glSeq++
	l.seq = glSeq
	if pad := l.appsink.GetStaticPad("sink"); pad != nil {
		if c := pad.GetCurrentCaps(); c != nil {
			l.caps = c.String()
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall layer caps %s", l.caps)
			l.srcW, l.srcH = capsSize(c)
		}
	}
	if w := kmsWall(); w != nil {
		x, y, bw, bh := wallRect(l.opts, w.Width, w.Height)
		// The cue's crop and fit mode, as on the plane wall (computeLayout:
		// crops on the source picture's edges, the fill modes' overflow
		// included), moved to the flipped picture's edges for the mixer (the
		// cue's videoflip has already turned it; srcW x srcH is the turned
		// size). A fill mode's cropped picture has the box's aspect, so every
		// mode but stretch keeps the aspect inside the box.
		if l.srcW > 0 && l.srcH > 0 && dir == dirIdentity { // a turned cue was cropped by its videocrop
			sw, sh := l.srcW, l.srcH
			if dir.transposes() {
				sw, sh = sh, sw
			}
			lay := computeLayout(l.opts, sw, sh, bw, bh, false)
			cl, cr, ct, cb := glCropEdges(dir, lay.cropL, lay.cropR, lay.cropT, lay.cropB)
			if cl|cr|ct|cb != 0 {
				layer.SetCrop(cl, cr, ct, cb)
			}
		}
		layer.SetRect(x, y, bw, bh, l.opts.FitMode != "stretch")
	}
	return true
}

// videoDir is a GstVideoOrientationMethod (videoflip's video-direction).
type videoDir int

const (
	dirIdentity videoDir = iota
	dir90R
	dir180
	dir90L
	dirHoriz
	dirVert
	dirULLR // transpose: across the upper-left/lower-right diagonal
	dirURLL // across the upper-right/lower-left diagonal
)

// transposes reports a direction that swaps width and height.
func (d videoDir) transposes() bool {
	return d == dir90R || d == dir90L || d == dirULLR || d == dirURLL
}

// glDirection is the flip for a cue's rotation (clockwise degrees) and
// mirror, applied in that order as on the plane wall: rotate, then mirror.
// On the GL wall it is done by videoflip in the cue's own pipeline (CPU,
// before the ISP), like the plane wall's 90/270: a GPU flip pass inside the
// wall pipeline rendered into GL's own textures (Mesa's fresh-buffer mode,
// the wall at 13 fps) or, with imported buffers, could block the GL thread
// on its pool and stop the whole wall.
func glDirection(rotation int, flip string) videoDir {
	r := ((rotation % 360) + 360) % 360
	switch {
	case flip == "h" && r == 90, flip == "v" && r == 270:
		return dirULLR
	case flip == "v" && r == 90, flip == "h" && r == 270:
		return dirURLL
	case flip == "h" && r == 180:
		return dirVert
	case flip == "v" && r == 180:
		return dirHoriz
	case flip == "h":
		return dirHoriz
	case flip == "v":
		return dirVert
	case r == 90:
		return dir90R
	case r == 180:
		return dir180
	case r == 270:
		return dir90L
	}
	return dirIdentity
}

// glCropEdges moves a crop given on the source picture's edges (left,
// right, top, bottom) to where those edges are after the flip: the mixer
// crops what reaches it, the flipped picture.
func glCropEdges(d videoDir, l, r, t, b int) (int, int, int, int) {
	switch d {
	case dir90R: // source left becomes the top, top becomes the right
		return b, t, l, r
	case dir180:
		return r, l, b, t
	case dir90L:
		return t, b, r, l
	case dirHoriz:
		return r, l, t, b
	case dirVert:
		return l, r, b, t
	case dirULLR:
		return t, b, l, r
	case dirURLL:
		return b, t, r, l
	}
	return l, r, t, b
}

// glRestackLocked writes zorder bottom->top for the visible stack (the
// pacing black pad is 0). Caller holds glMu.
func glRestackLocked() {
	for i, l := range glStack {
		if l.layer != nil {
			l.layer.SetZOrder(i + 1)
		}
	}
}

func glShow(p *gst.Pipeline, level float64, at string, ref *gst.Pipeline) {
	l := glAwait(p)
	glMu.Lock()
	defer glMu.Unlock()
	if l == nil || glLayers[p] != l || !glAttachLocked(p, l) {
		return
	}
	if !l.visible {
		l.visible = true
		glStack = insertLayer(glStack, l, stackIndex(glStack, glLayers[ref], at))
	}
	glRestackLocked()
	l.level = level
	l.layer.SetAlpha(l.opacity * level)
}

// glCurve maps a cue's fade curve name to the wall's.
func glCurve(curve string) glwall.Curve {
	switch curve {
	case "smooth":
		return glwall.CurveSmooth
	case "log":
		return glwall.CurveLog
	case "exp":
		return glwall.CurveExp
	}
	return glwall.CurveLinear
}

// glRamp hands a fade to the wall: level from -> to over dur along curve,
// started at start (in the past when a fade has been running). The wall
// evaluates it for each output frame, so the fade steps every refresh.
// Calling it again with the same fade is harmless (fade loops re-send it
// each tick, which also re-anchors a fade-in that was held).
func glRamp(p *gst.Pipeline, from, to float64, start time.Time, dur time.Duration, curve string) {
	glMu.Lock()
	defer glMu.Unlock()
	l := glLayers[p]
	if l == nil || l.layer == nil || !l.visible {
		return
	}
	since := time.Since(start)
	l.level = rampLevel(from, to, since, dur, curve)
	l.layer.Ramp(l.opacity*from, l.opacity*to, glwall.Now()-uint64(since), dur, glCurve(curve))
}

// glOnWall reports whether p's layer is attached and shown: a fade-in's
// clock waits for it (a cue's first frame can reach the wall well after
// Play, e.g. a still through decodebin3, and a fade that ran before then
// would show the picture at full level at once).
func glOnWall(p *gst.Pipeline) bool {
	glMu.Lock()
	defer glMu.Unlock()
	l := glLayers[p]
	return l != nil && l.layer != nil && l.visible
}

func glSetLevel(p *gst.Pipeline, level float64) {
	glMu.Lock()
	defer glMu.Unlock()
	l := glLayers[p]
	if l == nil || l.layer == nil || !l.visible {
		return
	}
	l.level = level
	l.layer.SetAlpha(l.opacity * level)
}

// glPark attaches the armed panic image above everything at alpha 0.
func glPark(p *gst.Pipeline) bool {
	l := glAwait(p)
	glMu.Lock()
	defer glMu.Unlock()
	if l == nil || glLayers[p] != l || !glAttachLocked(p, l) {
		return false
	}
	l.parked = true
	l.layer.SetZOrder(glParkedZ)
	l.layer.SetAlpha(0)
	return true
}

// glRaise cuts the layer to the top at full opacity: the next output frame.
func glRaise(p *gst.Pipeline) {
	l := glAwait(p)
	glMu.Lock()
	defer glMu.Unlock()
	if l == nil || glLayers[p] != l || !glAttachLocked(p, l) {
		return
	}
	if l.visible {
		for i, s := range glStack {
			if s == l {
				glStack = append(glStack[:i], glStack[i+1:]...)
				break
			}
		}
	}
	l.visible, l.parked = true, false
	glStack = append(glStack, l)
	l.layer.SetZOrder(len(glStack))
	l.level = 1
	l.layer.SetAlpha(l.opacity)
}

// glHide takes a stopped cue off the wall but keeps its record: the cue
// pipeline is kept in NULL for a later resume, which prerolls again and
// re-attaches through showLayer (the pump left with the nulled appsink).
func glHide(p *gst.Pipeline) {
	glMu.Lock()
	l := glLayers[p]
	if l != nil {
		if l.visible {
			for i, s := range glStack {
				if s == l {
					glStack = append(glStack[:i], glStack[i+1:]...)
					break
				}
			}
			l.visible = false
			glRestackLocked()
		}
	}
	layer := glTakeLayerLocked(l)
	glMu.Unlock()
	glFreeLayer(layer)
}

// glTakeLayerLocked detaches the wall layer from its record, so exactly one
// caller frees it: Stop (glHide) and the cue's teardown (glDrop) can run at
// once, and both freeing the same layer released its mixer pad twice and
// crashed the service. Caller holds glMu.
func glTakeLayerLocked(l *glLayer) *glwall.Layer {
	if l == nil {
		return nil
	}
	layer := l.layer
	l.layer = nil
	return layer
}

// glFreeLayer takes a detached layer off the wall (outside glMu: it waits for
// the layer's pump thread).
func glFreeLayer(layer *glwall.Layer) {
	if layer != nil {
		layer.SetAlpha(0)
		layer.Free()
	}
}

// glDrop detaches the cue's layer. Call after p is in NULL.
func glDrop(p *gst.Pipeline) {
	glMu.Lock()
	l := glLayers[p]
	delete(glLayers, p)
	if l != nil && l.visible {
		for i, s := range glStack {
			if s == l {
				glStack = append(glStack[:i], glStack[i+1:]...)
				break
			}
		}
		glRestackLocked()
	}
	layer := glTakeLayerLocked(l)
	glMu.Unlock()
	glFreeLayer(layer)
}
