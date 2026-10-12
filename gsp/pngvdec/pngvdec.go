// Package pngvdec registers cutepipngvdec (pngvdec.c): PNG video frames
// decoded in parallel by libpng, in order. pngdec alone decodes one frame
// at a time (21-22 ms a 1080p RGBA frame), so PNG video played at 33-39 fps.
package pngvdec

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0
#cgo LDFLAGS: -ldl
#include <gst/gst.h>
int cutepi_pngvdec_register(GstPlugin *plugin, int rank);
*/
import "C"

// Rank puts the element above pngdec (primary) for moving pictures; stills
// (framerate 0/1) are outside its caps and keep pngdec.
const Rank = 257

// Register makes cutepipngvdec available in this process; false when
// libpng16 cannot be loaded. Call after gst.Init.
func Register() bool { return C.cutepi_pngvdec_register(nil, Rank) != 0 }
