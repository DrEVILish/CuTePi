// Package scanout registers wpedmabuf (wpedmabuf.c), the element that puts a
// live page's GPU frames on a display plane without a CPU copy: one GPU blit
// per frame from WPE's tiled texture into a ring of linear CMA buffers the
// vc4 plane scans out (DESIGN §12.14).
package scanout

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-base-1.0 gstreamer-video-1.0 gstreamer-gl-1.0 gstreamer-allocators-1.0 libdrm egl glesv2
int scanout_register(void);
*/
import "C"

import "sync"

var (
	once sync.Once
	ok   bool
)

// Register makes the wpedmabuf element available to pipelines in this
// process; it reports whether that worked. Safe to call more than once.
func Register() bool {
	once.Do(func() { ok = C.scanout_register() != 0 })
	return ok
}
