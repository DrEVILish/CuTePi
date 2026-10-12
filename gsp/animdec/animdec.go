// Package animdec registers cutepianimdec (animdec.c): animated GIF, WebP,
// APNG, AVIF and JPEG XL images (and their stills), decoded to straight-alpha
// RGBA at the file's own frame timing. GStreamer here cannot play the last
// four, and decodes GIF on one thread. WebP,
// AVIF and JPEG XL use the system's libwebpdemux, libavif and libjxl, loaded
// at run time; APNG frames are rebuilt as PNGs and decoded by pngdec.
package animdec

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0 gstreamer-app-1.0
#cgo CFLAGS: -I${SRCDIR}/../../third_party/dav1d/include -I${SRCDIR}/../../third_party/libwebp/include -I${SRCDIR}/../../third_party/libavif/include -I${SRCDIR}/../../third_party/libjxl/include
#cgo LDFLAGS: -ldl
#include <gst/gst.h>
#include <stdlib.h>
int cutepi_animdec_register(GstPlugin *plugin, int rank);
void *cutepi_webp_ref_open(const unsigned char *data, size_t len, int *w, int *h, int *n);
int cutepi_webp_ref_next(void *d, unsigned char *out);
void cutepi_webp_ref_close(void *d);
*/
import "C"

import (
	"strings"
	"unsafe"
)

// Rank puts the element above webpdec (primary), so animated WebP, which
// webpdec cannot decode, comes here (its stills too), and above avdemux_gif
// and avdec_gif (marginal), whose single-threaded decode held a 1080p GIF to
// ~40 fps.
const Rank = 257 // GST_RANK_PRIMARY + 1

// Register makes cutepianimdec and its type finders (APNG, AVIF, JPEG XL)
// available in this process; it returns the formats it can decode ("" if
// the element could not be registered). Call after gst.Init.
func Register() string {
	have := int(C.cutepi_animdec_register(nil, Rank))
	var f []string
	for i, n := range []string{"WebP", "APNG", "AVIF", "JPEG XL", "GIF"} {
		if have&(1<<i) != 0 {
			f = append(f, n)
		}
	}
	return strings.Join(f, ", ")
}

// webpRef steps through an animated WebP with libwebp's own animation
// decoder, one composited RGBA frame at a time (tests compare the element's
// frames with it). next returns nil at the end; call close when done.
type webpRef struct {
	d    unsafe.Pointer
	data unsafe.Pointer
	w, h int
	n    int
}

func newWebpRef(data []byte) *webpRef {
	r := &webpRef{data: C.CBytes(data)}
	var cw, ch, cn C.int
	r.d = C.cutepi_webp_ref_open((*C.uchar)(r.data), C.size_t(len(data)), &cw, &ch, &cn)
	if r.d == nil {
		C.free(r.data)
		return nil
	}
	r.w, r.h, r.n = int(cw), int(ch), int(cn)
	return r
}

func (r *webpRef) next(buf []byte) bool {
	return C.cutepi_webp_ref_next(r.d, (*C.uchar)(unsafe.Pointer(&buf[0]))) != 0
}

func (r *webpRef) close() {
	C.cutepi_webp_ref_close(r.d)
	C.free(r.data)
}
