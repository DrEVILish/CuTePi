// Package qtmkv registers cutepiqtmkvparse (qtmkv.c): caps for QuickTime
// codecs carried in Matroska (V_QUICKTIME: HAP, QuickTime Animation), read
// from the file's track header, so their decoders are plugged.
package qtmkv

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>
int cutepi_qtmkv_register(void);
*/
import "C"

// Register makes cutepiqtmkvparse available in this process. Call after gst.Init.
func Register() bool { return C.cutepi_qtmkv_register() != 0 }
