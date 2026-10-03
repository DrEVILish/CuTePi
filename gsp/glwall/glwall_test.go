package glwall

import (
	"math"
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
