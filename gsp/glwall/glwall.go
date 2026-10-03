// Package glwall is the GPU compositor wall (DESIGN §6.1.1): a cgo wrapper
// around glwall.c, which runs the always-on GL mixer pipeline, the ring of
// scan-out buffers it renders into and the presenter, and bridges cue
// pipelines onto it as layers. The service's own DRM fd and one of its
// overlay planes are handed in; the service stays DRM master.
package glwall

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0 gstreamer-gl-1.0 gstreamer-allocators-1.0 libdrm egl glesv2
#include <stdlib.h>
#include "glwall.h"
*/
import "C"

import (
	"errors"
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

// SetZOrder sets the stacking order (higher is on top).
func (l *Layer) SetZOrder(z int) { C.glwall_layer_set_zorder(l.l, C.int(z)) }

// SetRect places the layer on the wall.
func (l *Layer) SetRect(x, y, w, h int) {
	C.glwall_layer_set_rect(l.l, C.int(x), C.int(y), C.int(w), C.int(h))
}

// Free detaches the layer. The cue pipeline must already be in NULL.
func (l *Layer) Free() {
	if l.l != nil {
		C.glwall_layer_free(l.l)
		l.l = nil
	}
}

// Stats reports frames mixed and frames presented since Open.
func Stats() (mixed, presented uint64) {
	var m, p C.uint64_t
	C.glwall_stats(&m, &p)
	return uint64(m), uint64(p)
}
