// Package glwall is the GPU compositor wall (DESIGN §6.1.1): a cgo wrapper
// around glwall.c, which runs the always-on GL mixer pipeline, the ring of
// scan-out buffers it renders into and the presenter, and bridges cue
// pipelines onto it as layers. The service's own DRM fd and one of its
// overlay planes are handed in; the service stays DRM master.
package glwall

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0 gstreamer-gl-1.0 gstreamer-allocators-1.0 libdrm egl glesv2
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "glwall.h"
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

// Open builds the wall on the display and starts presenting black.
func Open(drmFD int, crtcID, planeID uint32, width, height, refreshHz int) error {
	var cerr *C.char
	if C.glwall_open(C.int(drmFD), C.uint32_t(crtcID), C.uint32_t(planeID), C.int(width), C.int(height), C.int(refreshHz), &cerr) != 0 {
		msg := C.GoString(cerr)
		C.g_free(C.gpointer(cerr))
		return errors.New("glwall: " + msg)
	}
	return nil
}

// Close stops the wall and frees its buffers; the plane is switched off.
func Close() { C.glwall_close() }

// IsOpen reports whether the wall is running.
func IsOpen() bool { return C.glwall_is_open() != 0 }

// PrepareSink readies a cue pipeline's appsink before the cue negotiates
// (allocation answer for the decoder: video meta and a pool-size hint).
func PrepareSink(appsink *gst.Element, poolBuffers int) {
	C.glwall_prepare_sink((*C.GstElement)(appsink.Unsafe()), C.int(poolBuffers))
}

// Layer is a cue on the wall.
type Layer struct{ l *C.glwall_layer }

// Attach joins a prerolled cue (its appsink holds a preroll frame) to the
// wall. colorimetry is set on the layer when the cue's caps carry none.
func Attach(appsink *gst.Element, colorimetry string) (*Layer, error) {
	cs := C.CString(colorimetry)
	defer C.free(unsafe.Pointer(cs))
	var cerr *C.char
	l := C.glwall_layer_attach((*C.GstElement)(appsink.Unsafe()), cs, &cerr)
	if l == nil {
		msg := C.GoString(cerr)
		C.g_free(C.gpointer(cerr))
		return nil, errors.New("glwall: " + msg)
	}
	return &Layer{l: l}, nil
}

// SetAlpha sets the layer's opacity (0..1), applied on the next output frame.
func (l *Layer) SetAlpha(a float64) { C.glwall_layer_set_alpha(l.l, C.double(a)) }

// Curve is a fade envelope (as gsp's fadeShape).
type Curve int

const (
	CurveLinear Curve = iota
	CurveSmooth
	CurveLog
	CurveExp
)

// Ramp fades the layer from one opacity to another, evaluated by the wall
// for every output frame from that frame's own clock time, so the opacity
// changes on every refresh. start is a time on the wall's clock (Now). A
// SetAlpha ends it; once done it holds `to`.
func (l *Layer) Ramp(from, to float64, start uint64, dur time.Duration, curve Curve) {
	C.glwall_layer_ramp(l.l, C.double(from), C.double(to), C.uint64_t(start), C.uint64_t(dur), C.int(curve))
}

// rampShape is the C ramp's envelope (tests compare it with gsp's fadeShape).
func rampShape(c Curve, t float64) float64 {
	return float64(C.glwall_ramp_shape(C.int(c), C.double(t)))
}

// Now is the wall's clock (the system clock, ns).
func Now() uint64 { return uint64(C.glwall_now()) }

// SetCrop crops the layer's picture as it reaches the mixer, in pixels per
// edge (the mixer pad's crop: no extra GPU pass).
func (l *Layer) SetCrop(left, right, top, bottom int) {
	C.glwall_layer_set_crop(l.l, C.int(left), C.int(right), C.int(top), C.int(bottom))
}

// SetZOrder sets the stacking order (higher is on top).
func (l *Layer) SetZOrder(z int) { C.glwall_layer_set_zorder(l.l, C.int(z)) }

// SetRect places the layer on the wall; keepAspect letterboxes inside the
// rectangle (fit) instead of stretching.
func (l *Layer) SetRect(x, y, w, h int, keepAspect bool) {
	k := 0
	if keepAspect {
		k = 1
	}
	C.glwall_layer_set_rect(l.l, C.int(x), C.int(y), C.int(w), C.int(h), C.int(k))
}

// AlignAudio delays a cue's audio sink (ts-offset) by the wall's display
// delay, so its sound plays when its picture is seen.
func AlignAudio(sink *gst.Element) { C.glwall_align_audio((*C.GstElement)(sink.Unsafe())) }

// AudioOffset reports the largest ts-offset on a pipeline's sinks other than
// the video appsink (-1 when there is none).
func AudioOffset(p *gst.Pipeline) time.Duration {
	return time.Duration(C.glwall_audio_offset((*C.GstElement)(p.Unsafe())))
}

// DisplayDelay is how long after its time a frame is seen (mid-screen).
func DisplayDelay() time.Duration { return time.Duration(C.glwall_display_delay()) }

// UseSystemClock pins a cue pipeline to the system clock the wall runs on.
func UseSystemClock(p *gst.Pipeline) { C.glwall_use_system_clock((*C.GstElement)(p.Unsafe())) }

// Free detaches the layer. The cue pipeline must already be in NULL.
func (l *Layer) Free() {
	if l.l != nil {
		C.glwall_layer_free(l.l)
		l.l = nil
	}
}

// Stats reports frames pulled from the cue, pushed to the mixer, and the
// number of opacity changes applied (each lands on the next output frame).
func (l *Layer) Stats() (pulled, pushed, steps uint64) {
	var a, b, c C.uint64_t
	if l.l != nil {
		C.glwall_layer_stats(l.l, &a, &b, &c)
	}
	return uint64(a), uint64(b), uint64(c)
}

// Shown reports, over output frames actually presented, how many showed a
// new frame of the cue and how many changed its opacity (fade steps), and
// how many cue frames arrived too late for their output frame (shown on
// arrival; the decoder is told to catch up), and how late the last one was.
func (l *Layer) Shown() (frames, steps, late uint64, lag time.Duration) {
	var a, b, c C.uint64_t
	var d C.int64_t
	if l.l != nil {
		C.glwall_layer_shown(l.l, &a, &b, &c, &d)
	}
	return uint64(a), uint64(b), uint64(c), time.Duration(d)
}

// PoolStats are the ring pool's diagnostics.
type PoolStats struct {
	Allocs, Frees, Exhausted, SetConfigs, Activations, AllocQueries, Skipped uint64
	// Presenter timing (µs): fence wait and SetPlane commit, totals and
	// maxima since the last read; commits longer than a refresh.
	FenceUs, FenceMaxUs, FlipUs, FlipMaxUs, FlipsLong uint64
	// GPU time when the presenter waited, and lateness of mixed frames (µs).
	GpuUs, GpuMaxUs, LateUs, LateMaxUs uint64
	Unsnapped                          uint64 // frames presented without a mixer snapshot
	Allocated, OnScreen, Queued        int
}

// Pool reports the ring pool's diagnostics.
func Pool() PoolStats {
	var st C.glwall_pool_stats_t
	C.glwall_pool_stats(&st)
	return PoolStats{
		Allocs: uint64(st.allocs), Frees: uint64(st.frees), Exhausted: uint64(st.exhausted),
		SetConfigs: uint64(st.set_configs), Activations: uint64(st.activations), AllocQueries: uint64(st.alloc_queries),
		Skipped: uint64(st.skipped),
		FenceUs: uint64(st.fence_us), FenceMaxUs: uint64(st.fence_max_us), FlipUs: uint64(st.flip_us),
		FlipMaxUs: uint64(st.flip_max_us), FlipsLong: uint64(st.flips_long),
		Unsnapped: uint64(st.unsnapped),
		GpuUs:     uint64(st.gpu_us), GpuMaxUs: uint64(st.gpu_max_us), LateUs: uint64(st.late_us), LateMaxUs: uint64(st.late_max_us),
		Allocated: int(st.allocated), OnScreen: int(st.onscreen), Queued: int(st.queued),
	}
}

// Stats reports frames mixed and frames presented since Open.
func Stats() (mixed, presented uint64) {
	var m, p C.uint64_t
	C.glwall_stats(&m, &p)
	return uint64(m), uint64(p)
}
