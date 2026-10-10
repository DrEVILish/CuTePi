// Package planewall is the plane wall's presenter (DESIGN §6.1.4): a cgo
// wrapper around planewall.c, which commits every cue plane's newest frame
// and its opacity, z-order and placement in one atomic commit per display
// refresh, and registers cutepiplanesink, the sink that hands it frames. The
// service's own DRM fd (master, atomic capability set) is handed in.
package planewall

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-base-1.0 gstreamer-video-1.0 gstreamer-allocators-1.0 gstreamer-gl-1.0 libdrm egl glesv2
#include <stdlib.h>
#include <glib.h>
#include "planewall.h"
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

var regOnce sync.Once

// Open starts the presenter on the display's CRTC and overlay planes.
func Open(drmFD int, crtcID uint32, planes []uint32, width, height, refreshHz int) error {
	if len(planes) == 0 {
		return errors.New("planewall: no planes")
	}
	var cerr *C.char
	if C.planewall_open(C.int(drmFD), C.uint32_t(crtcID), (*C.uint32_t)(unsafe.Pointer(&planes[0])), C.int(len(planes)),
		C.int(width), C.int(height), C.int(refreshHz), &cerr) != 0 {
		msg := C.GoString(cerr)
		C.g_free(C.gpointer(cerr))
		return errors.New("planewall: " + msg)
	}
	regOnce.Do(func() { C.cutepi_planesink_register() })
	return nil
}

// SetGLDisplay hands in the process-wide GstGLDisplay (glwall's, never
// freed): HEVC frames are gathered on the GPU in a context on it.
func SetGLDisplay(display unsafe.Pointer) { C.planewall_set_gl_display(display) }

// WarmGL makes the HEVC gather's GL context now (false: none, HEVC stays
// off the planes' fast route).
func WarmGL() bool { return C.planewall_gl_warm() != 0 }

// IsOpen reports whether the presenter runs.
func IsOpen() bool { return C.planewall_is_open() != 0 }

// Set queues a plane property (alpha, zpos, rotation, "pixel blend mode")
// for the next commit.
func Set(plane uint32, name string, value uint64) error {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	if C.planewall_set(C.uint32_t(plane), cs, C.uint64_t(value)) != 0 {
		return errors.New("planewall: unknown plane or property " + name)
	}
	return nil
}

// Detach takes the plane off the screen and releases its frames (waits for
// the commit).
func Detach(plane uint32) { C.planewall_detach(C.uint32_t(plane)) }

// Stats is the presenter's commit count, failures and the intervals between
// commits.
type Stats struct {
	Commits, Fails      uint64
	P50Ms, P99Ms, MaxMs float64
}

func ReadStats() Stats {
	var s C.planewall_stats_t
	C.planewall_stats(&s)
	return Stats{uint64(s.commits), uint64(s.fails), float64(s.p50_ms), float64(s.p99_ms), float64(s.max_ms)}
}

// PlaneStat is one plane's presenter counts as of one commit.
type PlaneStat struct {
	Shown   uint64 // frames that reached the screen
	Dropped uint64 // frames replaced before reaching it
	Steps   uint64 // commits that changed its opacity on screen
	First   bool   // has shown a frame
	AtUs    int64  // monotonic time (µs) of the commit the counts are as of
}

func PlaneStats(plane uint32) PlaneStat {
	var s, d, st C.uint64_t
	var f C.int
	var t C.int64_t
	C.planewall_plane_stats(C.uint32_t(plane), &s, &d, &st, &f, &t)
	return PlaneStat{uint64(s), uint64(d), uint64(st), f != 0, int64(t)}
}
