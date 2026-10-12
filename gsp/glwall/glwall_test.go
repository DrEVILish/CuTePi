package glwall

import (
	"math"
	"strings"
	"testing"
)

// The wall's per-frame ramp (C) must give the same envelope as gsp's
// fadeShape for every curve, clamped outside 0..1: the plane wall and the
// GPU wall fade alike, and the sound's ramp (Go) stays in step with the
// picture's (C).
func TestRampShapeMatchesFadeShape(t *testing.T) {
	shape := map[Curve]func(float64) float64{
		CurveLinear: func(t float64) float64 { return t },
		CurveSmooth: func(t float64) float64 { return t * t * (3 - 2*t) },
		CurveLog:    func(t float64) float64 { return math.Log10(1 + 9*t) },
		CurveExp:    func(t float64) float64 { return (math.Exp(3*t) - 1) / (math.Exp(3) - 1) },
	}
	for c, f := range shape {
		for _, x := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.999, 1} {
			if got, want := rampShape(c, x), f(x); math.Abs(got-want) > 1e-9 {
				t.Errorf("curve %d at %v: C %v, Go %v", c, x, got, want)
			}
		}
		if got := rampShape(c, -0.5); got != 0 {
			t.Errorf("curve %d before the start: %v, want 0", c, got)
		}
		if got := rampShape(c, 1.5); got != 1 {
			t.Errorf("curve %d after the end: %v, want 1", c, got)
		}
	}
	if got := rampShape(Curve(99), 0.4); math.Abs(got-0.4) > 1e-9 {
		t.Errorf("unknown curve: %v, want linear 0.4", got)
	}
}

// The copy route's row conversion (NEON blocks of 16 pixels plus a scalar
// tail) must give the bytes the slot's DRM format is read as: R, G, B, A in
// memory for ABGR/XBGR8888, B, G, R, A for ARGB/XRGB8888; x formats are
// sampled as opaque by the GPU, so only their colour bytes count there.
func TestLinRowLayouts(t *testing.T) {
	const (
		abgr = 0x34324241 // DRM_FORMAT_ABGR8888
		xbgr = 0x34324258
		argb = 0x34325241
		xrgb = 0x34325258
	)
	// order: the source's bytes per pixel, by channel letter
	cases := []struct {
		format, order string
		fourcc        uint32
	}{
		{"RGBA", "RGBA", abgr}, {"BGRA", "BGRA", argb}, {"RGBx", "RGBx", xbgr}, {"BGRx", "BGRx", xrgb},
		{"ARGB", "ARGB", abgr}, {"ABGR", "ABGR", abgr}, {"xRGB", "xRGB", xbgr}, {"xBGR", "xBGR", xbgr},
		{"RGB", "RGB", xbgr}, {"BGR", "BGR", xbgr},
	}
	for _, c := range cases {
		for _, w := range []int{1, 15, 16, 17, 33, 1920} {
			bpp := len(c.order)
			src := make([]byte, w*bpp)
			px := func(x int) map[byte]byte { // distinct values per pixel and channel
				return map[byte]byte{'R': byte(x*7 + 1), 'G': byte(x*7 + 2), 'B': byte(x*7 + 3), 'A': byte(x*7 + 4), 'x': byte(x*7 + 5)}
			}
			for x := 0; x < w; x++ {
				for i := 0; i < bpp; i++ {
					src[x*bpp+i] = px(x)[c.order[i]]
				}
			}
			dst, fourcc := linRow(c.format, src, w)
			if fourcc != c.fourcc {
				t.Fatalf("%s: fourcc %#x, want %#x", c.format, fourcc, c.fourcc)
			}
			out := "RGBA" // memory order of ABGR/XBGR8888
			if fourcc == argb || fourcc == xrgb {
				out = "BGRA"
			}
			opaque := strings.ContainsAny(c.order, "x") || bpp == 3
			for x := 0; x < w; x++ {
				p := px(x)
				for i, ch := range []byte(out) {
					want := p[ch]
					if ch == 'A' && opaque {
						if c.format == "RGBx" || c.format == "BGRx" {
							continue // copied as is; the GPU ignores the byte
						}
						want = 255
					}
					if got := dst[x*4+i]; got != want {
						t.Fatalf("%s w=%d pixel %d byte %d (%c): %d, want %d", c.format, w, x, i, ch, got, want)
					}
				}
			}
		}
	}
	if _, fourcc := linRow("I420", make([]byte, 64), 16); fourcc != 0 {
		t.Errorf("I420 taken by the copy route")
	}
}
