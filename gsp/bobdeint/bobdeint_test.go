package bobdeint

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

func pull(t *testing.T, desc string) (frames [][]byte, pts []time.Duration) {
	t.Helper()
	pipe, err := gst.NewPipelineFromString(desc)
	if err != nil {
		t.Fatal(err)
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	for {
		s := sink.TryPullSample(gst.ClockTime(5 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		frames = append(frames, append([]byte(nil), m.Bytes()...))
		pts = append(pts, time.Duration(b.PresentationTimestamp()))
		b.Unmap()
	}
	return frames, pts
}

// cutepibob gives what GStreamer's linear deinterlacer gives (one picture
// per field, the rows between the mean of their neighbours), in both field
// orders, for planar and semi-planar 4:2:0, 4:4:4, packed RGB and packed
// YUV with and without alpha, with field-rate timestamps.
func TestMatchesLinearDeinterlace(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Fatal("not registered")
	}
	for _, fmtName := range []string{"I420", "NV12", "RGBA", "BGRx", "RGB", "AYUV", "YUY2", "Y444"} {
		for _, tff := range []bool{true, false} {
			order, deintTff := "top-field-first", "tff"
			if !tff {
				order, deintTff = "bottom-field-first", "bff"
			}
			src := fmt.Sprintf("videotestsrc pattern=ball num-buffers=6 ! video/x-raw,format=%s,width=64,height=48,framerate=25/1,interlace-mode=interleaved,field-order=%s", fmtName, order)
			want, _ := pull(t, src+" ! deinterlace method=linear fields=all mode=interlaced tff="+deintTff+" ! appsink name=k sync=false")
			got, pts := pull(t, fmt.Sprintf("%s ! cutepibob tff=%v ! appsink name=k sync=false", src, tff))
			name := fmt.Sprintf("%s tff=%v", fmtName, tff)
			if len(got) != 12 || len(want) != 12 {
				t.Fatalf("%s: %d pictures, deinterlace %d, want 12", name, len(got), len(want))
			}
			for i := range want {
				if want := time.Duration(i) * 20 * time.Millisecond; pts[i] != want {
					t.Errorf("%s picture %d at %v, want %v", name, i, pts[i], want)
				}
				diff := 0
				for j := range want[i] {
					if d := int(got[i][j]) - int(want[i][j]); d > 1 || d < -1 {
						diff++
					}
				}
				if diff > 0 {
					t.Errorf("%s picture %d: %d of %d bytes differ from deinterlace by more than 1", name, i, diff, len(want[i]))
				}
			}
		}
	}
}

// With dmabuf=true the pictures leave as DMA_DRM DMABufs (strides on 64
// bytes for luma, 32 for chroma) holding the same pixels as system-memory
// output.
func TestDMABufOutput(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Fatal("not registered")
	}
	for _, c := range []struct {
		format, drm string
		w           int
		stride      int // padded luma stride
	}{{"I420", "YU12", 1920, 1920}, {"I420", "YU12", 720, 768}} {
		src := fmt.Sprintf("videotestsrc pattern=ball num-buffers=3 ! video/x-raw,format=%s,width=%d,height=48,framerate=25/1,interlace-mode=interleaved,field-order=top-field-first", c.format, c.w)
		want, _ := pull(t, src+" ! cutepibob ! appsink name=k sync=false")
		pipe, err := gst.NewPipelineFromString(src + " ! cutepibob dmabuf=true ! appsink name=k sync=false")
		if err != nil {
			t.Fatal(err)
		}
		ke, _ := pipe.GetElementByName("k")
		sink := app.SinkFromElement(ke)
		pipe.SetState(gst.StatePlaying)
		name := fmt.Sprintf("%s %d", c.format, c.w)
		var got [][]byte
		for {
			s := sink.TryPullSample(gst.ClockTime(5 * time.Second))
			if s == nil {
				break
			}
			if len(got) == 0 {
				caps := s.GetCaps()
				st := caps.GetStructureAt(0)
				if f, _ := st.GetValue("drm-format"); f != c.drm || !caps.GetFeaturesAt(0).Contains("memory:DMABuf") {
					t.Errorf("%s: caps %s", name, caps)
				}
			}
			b := s.GetBuffer()
			m := b.Map(gst.MapRead)
			got = append(got, append([]byte(nil), m.Bytes()...))
			b.Unmap()
		}
		pipe.SetState(gst.StateNull)
		if len(got) != len(want) || len(got) != 6 {
			t.Fatalf("%s: %d pictures, system memory %d, want 6", name, len(got), len(want))
		}
		// Compare row by row: luma rows, then the chroma plane(s).
		h, cw := 48, c.w/2
		for i := range want {
			type plane struct{ srcOff, srcStride, dstOff, dstStride, n, rows int }
			var planes []plane
			ys := c.stride * h
			if c.format == "I420" {
				cs := c.stride / 2
				planes = []plane{{0, c.w, 0, c.stride, c.w, h},
					{c.w * h, cw, ys, cs, cw, h / 2},
					{c.w*h + cw*h/2, cw, ys + cs*h/2, cs, cw, h / 2}}
			} else {
				planes = []plane{{0, c.w, 0, c.stride, c.w, h}, {c.w * h, c.w, ys, c.stride, c.w, h / 2}}
			}
			for pi, p := range planes {
				for y := 0; y < p.rows; y++ {
					a := want[i][p.srcOff+y*p.srcStride:][:p.n]
					d := p.dstOff + y*p.dstStride
					if d+p.n > len(got[i]) {
						t.Fatalf("%s picture %d: %d bytes, too small for the layout", name, i, len(got[i]))
					}
					if string(a) != string(got[i][d:d+p.n]) {
						t.Fatalf("%s picture %d plane %d row %d differs", name, i, pi, y)
					}
				}
			}
		}
	}
}

// A420 (4:2:0 with an alpha plane, which deinterlace does not take): the
// colour planes come out as for I420 and the alpha plane is deinterlaced the
// same way (opaque here: it stays 255).
func TestA420(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Fatal("not registered")
	}
	src := func(f string) string {
		return fmt.Sprintf("videotestsrc pattern=ball num-buffers=4 ! video/x-raw,format=%s,width=64,height=48,framerate=25/1,interlace-mode=interleaved,field-order=top-field-first", f)
	}
	i420, _ := pull(t, src("I420")+" ! cutepibob ! appsink name=k sync=false")
	a420, _ := pull(t, src("A420")+" ! cutepibob ! appsink name=k sync=false")
	// The alpha plane through bob as a GRAY8 picture of the same rows.
	if len(i420) != 8 || len(a420) != 8 {
		t.Fatalf("%d I420 and %d A420 pictures, want 8", len(i420), len(a420))
	}
	n := 64*48 + 2*32*24
	for i := range i420 {
		if len(a420[i]) != n+64*48 {
			t.Fatalf("A420 picture %d: %d bytes, want %d", i, len(a420[i]), n+64*48)
		}
		if string(a420[i][:n]) != string(i420[i][:n]) {
			t.Errorf("A420 picture %d: colour planes differ from I420", i)
		}
		for _, a := range a420[i][n:] {
			if a != 255 {
				t.Fatalf("A420 picture %d: alpha %d, want 255 (opaque source)", i, a)
			}
		}
	}
}
