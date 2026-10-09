// Package cfhd registers cutepicfhddec (cfhd.c): CineForm decoding with the
// GoPro CineForm SDK (third_party/cineform-sdk, ported to AArch64), several
// single-threaded decoders on alternate frames. FFmpeg's avdec_cfhd decodes
// 1080p CineForm at 19-23 fps on a Pi 4; this element at 70-98 (h264-pi4
// results/38). The library is loaded at run time: build and install it with
// `make -C third_party/cineform-sdk install` (deploy/build-cineform.sh).
package cfhd

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0 gstreamer-app-1.0
#cgo CFLAGS: -I${SRCDIR}/../../third_party/cineform-sdk/Common
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include <gst/gst.h>
int cutepi_cfhd_register(GstPlugin *plugin, const char *path, int rank);
*/
import "C"

import (
	"os"
	"unsafe"
)

// DefaultLib is where `make install` puts the library.
const DefaultLib = "/usr/local/lib/cutepi/libcutepi-cfhd.so"

// Rank puts the element above FFmpeg's avdec_cfhd (marginal).
const Rank = 257 // GST_RANK_PRIMARY + 1

// Register makes cutepicfhddec available when the CineForm library loads
// (CUTEPI_CFHD_LIB, else DefaultLib); false when it is not installed, and
// CineForm then plays through avdec_cfhd. Call after gst.Init.
func Register() bool {
	path := os.Getenv("CUTEPI_CFHD_LIB")
	if path == "" {
		path = DefaultLib
	}
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	return C.cutepi_cfhd_register(nil, cs, Rank) != 0
}
