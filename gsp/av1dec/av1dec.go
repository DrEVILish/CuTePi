// Package av1dec registers cutepidav1ddec (av1dec.c), an AV1 decoder element
// on libdav1d, loaded at run time. GStreamer here has only libaom for AV1
// (av1dec: 34 fps at 1080p on a Pi 4); with dav1d decodebin decodes the same
// stream at 137 fps, two at once 69 + 69, bit-identical to FFmpeg's dav1d
// (h264-pi4 results/35).
package av1dec

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0
#cgo CFLAGS: -I${SRCDIR}/../../third_party/dav1d/include
#cgo LDFLAGS: -ldl
#include <gst/gst.h>
int cutepi_av1dec_register(GstPlugin *plugin, int rank);
const char *cutepi_av1dec_version(void);
*/
import "C"

// Rank puts the element above every other AV1 decoder (libaom's av1dec is
// secondary), so decodebin picks it.
const Rank = 257 // GST_RANK_PRIMARY + 1

// Register makes cutepidav1ddec available to pipelines in this process. It
// reports false when libdav1d.so.7 is not installed (the element then does
// not exist and AV1 falls back to av1dec). Call after gst.Init.
func Register() bool { return C.cutepi_av1dec_register(nil, Rank) != 0 }

// Version is the loaded libdav1d's version ("" before Register).
func Version() string { return C.GoString(C.cutepi_av1dec_version()) }
