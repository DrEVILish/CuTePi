// Package yuvpack registers cutepiyuvpack (yuvpack.c): 10-bit 4:2:0/4:2:2
// and 8-bit 4:2:2 frames to 8-bit I420 in one NEON pass, ahead of
// videoconvert on the KMS wall (kmssink cannot allocate 4:2:2 buffers, so
// videoconvert went to RGB: ProRes/DNxHR at 0.8-14 fps).
package yuvpack

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-base-1.0 gstreamer-video-1.0
#include <gst/gst.h>
int cutepi_yuvpack_register(GstPlugin *plugin);
*/
import "C"

import "sync"

var (
	once sync.Once
	ok   bool
)

// Register makes cutepiyuvpack available in this process; safe to repeat.
func Register() bool {
	once.Do(func() { ok = C.cutepi_yuvpack_register(nil) != 0 })
	return ok
}
