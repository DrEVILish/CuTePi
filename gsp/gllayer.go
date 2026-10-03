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
}

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
func glRegister(p *gst.Pipeline, byFactory map[string][]*gst.Element, opts LoadOpts) {
	sink := firstByFactory(byFactory, "appsink")
	if sink == nil {
		return
	}
	glMu.Lock()
	glLayers[p] = &glLayer{appsink: sink, opts: opts, opacity: opacityOf(opts), level: 1}
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
	if pad := l.appsink.GetStaticPad("sink"); pad != nil {
		if c := pad.GetCurrentCaps(); c != nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall layer caps %s", c.String())
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
	glMu.Unlock()
	if l != nil && l.layer != nil {
		l.layer.SetAlpha(0)
		l.layer.Free()
		l.layer = nil
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
	glMu.Unlock()
	if l != nil && l.layer != nil {
		l.layer.SetAlpha(0)
		l.layer.Free()
	}
}
