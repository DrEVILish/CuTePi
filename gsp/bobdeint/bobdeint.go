// Package bobdeint registers cutepibob (bobdeint.c): interlaced video to one
// picture per field, line-interpolated, in one NEON pass that reads each
// input row once.
package bobdeint

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0 gstreamer-allocators-1.0
#include <gst/gst.h>
int cutepi_bob_register(void);
*/
import "C"

// Register makes cutepibob available in this process. Call after gst.Init.
func Register() bool { return C.cutepi_bob_register() != 0 }
