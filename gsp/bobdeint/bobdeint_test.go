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
// orders and for planar and semi-planar 4:2:0, with field-rate timestamps.
func TestMatchesLinearDeinterlace(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Fatal("not registered")
	}
	for _, fmtName := range []string{"I420", "NV12"} {
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
