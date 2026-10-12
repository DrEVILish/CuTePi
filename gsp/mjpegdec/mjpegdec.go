// Package mjpegdec registers cutepimjpegdec (mjpegdec.c): Motion JPEG decoded
// by several jpegdec instances in parallel, frames in order. jpegdec alone
// decodes one frame at a time (12.5 ms a 1080p frame in the service).
package mjpegdec

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0 gstreamer-app-1.0
#include <gst/gst.h>
int cutepi_mjpegdec_register(GstPlugin *plugin, int rank);
*/
import "C"

// Rank puts the element above jpegdec (primary) for moving pictures; stills
// (framerate 0/1) are outside its caps and keep jpegdec.
const Rank = 257

// Register makes cutepimjpegdec available in this process; false when jpegdec
// is missing. Call after gst.Init.
func Register() bool { return C.cutepi_mjpegdec_register(nil, Rank) != 0 }
