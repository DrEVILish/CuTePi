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
}

// glRoute names a GL tail by its elements (glVideoTail).
func glRoute(names []string) string {
	switch {
	case indexOfName(names, "v4l2convert") >= 0:
		return "isp"
	case indexOfName(names, "videoconvert") >= 0:
		return "alpha"
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
func glVideoTail(dmabuf bool, format string) []string {
	switch {
	case dmabuf:
		return []string{"queue", "capsfilter", "appsink"}
	case hasAlphaFormat(format):
		return []string{"queue", "videoconvert", "capsfilter", "appsink"}
	default:
		return []string{"queue", "videoconvert", "capsfilter", "v4l2convert", "capsfilter", "appsink"}
	}
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
	switch {
	case dmabuf:
		caps[0].Set("caps", gst.NewCapsFromString("video/x-raw(memory:DMABuf)"))
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
		}
	}
	if w := kmsWall(); w != nil {
		x, y, bw, bh := wallRect(l.opts, w.Width, w.Height)
		layer.SetRect(x, y, bw, bh, l.opts.FitMode != "stretch")
	}
	return true
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

func glShow(p *gst.Pipeline, level float64) {
	l := glAwait(p)
	glMu.Lock()
	defer glMu.Unlock()
	if l == nil || glLayers[p] != l || !glAttachLocked(p, l) {
		return
	}
	if !l.visible {
		l.visible = true
		glStack = append([]*glLayer{l}, glStack...)
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
	l.level = from + (to-from)*fadeShape(curve, float64(since)/float64(dur))
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
