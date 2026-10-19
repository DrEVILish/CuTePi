// Package sysdmabuf registers cutepisysdmabuf (sysdmabuf.c): software-decoded
// I420/NV12 frames copied by the CPU into system dma-heap buffers, which the
// GPU wall imports as dmabufs, in place of the ISP pass (v4l2convert) on the
// GPU wall's software route.
package sysdmabuf

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-base-1.0 gstreamer-video-1.0 gstreamer-allocators-1.0
#include <gst/gst.h>
int cutepi_sysdmabuf_register(void);
*/
import "C"

import "sync"

var (
	once sync.Once
	ok   bool
)

// Register makes cutepisysdmabuf available in this process; false when the
// system dma-heap (/dev/dma_heap/system) cannot be opened. Safe to repeat;
// call after gst.Init.
func Register() bool {
	once.Do(func() { ok = C.cutepi_sysdmabuf_register() != 0 })
	return ok
}
