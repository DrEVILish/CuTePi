package gsp

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>

extern void cutepiDeepElementAdded(GstElement *el, int kind);
extern void cutepiPadAdded(GstElement *el, GstPad *pad, gpointer key);

static void cutepi_deep_added(GstBin *bin, GstBin *sub, GstElement *el, gpointer kind) {
	cutepiDeepElementAdded(el, GPOINTER_TO_INT(kind));
}
static void cutepi_connect_deep(GstElement *bin, int kind) {
	g_signal_connect(bin, "deep-element-added", G_CALLBACK(cutepi_deep_added), GINT_TO_POINTER(kind));
}
static void cutepi_pad_added(GstElement *el, GstPad *pad, gpointer key) { cutepiPadAdded(el, pad, key); }
static void cutepi_connect_pad_added(GstElement *el, gpointer key) {
	g_signal_connect(el, "pad-added", G_CALLBACK(cutepi_pad_added), key);
}
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

// Per-pipeline signals go through these C handlers instead of go-glib's
// Connect: go-glib v1.4.0 keeps one reference of every Connect closure for
// itself, so the closure (and all it captures, a cue's whole pipeline) is
// never freed, disconnected or not.

const (
	deepNoQoS       = iota // intraNoQoS: cue pipelines
	deepOneDecoderT        // oneDecodeThread: the import's decode check
)

//export cutepiDeepElementAdded
func cutepiDeepElementAdded(el *C.GstElement, kind C.int) {
	e := gst.FromGstElementUnsafeNone(unsafe.Pointer(el))
	if kind == deepOneDecoderT {
		oneDecodeThread(e)
	} else {
		intraNoQoS(e)
	}
}

// onDeepElementAdded runs the kind's setup on every element joining bin or
// any bin inside it.
func onDeepElementAdded(bin *gst.Element, kind int) {
	C.cutepi_connect_deep((*C.GstElement)(unsafe.Pointer(bin.Instance())), C.int(kind))
}

// padHandlers: pipeline (its C pointer) -> its decodebin's pad-added handler.
// retirePipeline drops the entry, which frees what the handler holds.
var padHandlers sync.Map

//export cutepiPadAdded
func cutepiPadAdded(el *C.GstElement, pad *C.GstPad, key C.gpointer) {
	if f, ok := padHandlers.Load(uintptr(key)); ok {
		f.(func(*gst.Element, *gst.Pad))(gst.FromGstElementUnsafeNone(unsafe.Pointer(el)), gst.FromGstPadUnsafeNone(unsafe.Pointer(pad)))
	}
}

// onPadAdded connects f to el's pad-added for as long as p is not retired.
func onPadAdded(p *gst.Pipeline, el *gst.Element, f func(self *gst.Element, srcPad *gst.Pad)) {
	padHandlers.Store(pipeKey(p), f)
	C.cutepi_connect_pad_added((*C.GstElement)(unsafe.Pointer(el.Instance())), C.gpointer(unsafe.Pointer(p.Instance())))
}

func pipeKey(p *gst.Pipeline) uintptr { return uintptr(unsafe.Pointer(p.Instance())) }
