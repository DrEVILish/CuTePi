package mjpegdec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

// frames decodes path with decoder (the factory to force) and returns each
// frame's bytes (as I420/Y42B, packed by the sink's caps) and the decoder used.
func frames(t *testing.T, path, decoder string) ([][]byte, []time.Duration) {
	t.Helper()
	pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! qtdemux ! jpegparse ! " + decoder +
		" ! videoconvert ! video/x-raw,format=I420 ! appsink name=k sync=false max-buffers=4")
	if err != nil {
		t.Fatal(err)
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	var out [][]byte
	var pts []time.Duration
	for {
		s := sink.TryPullSample(gst.ClockTime(10 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		out = append(out, append([]byte(nil), m.Bytes()...))
		pts = append(pts, time.Duration(b.PresentationTimestamp()))
		b.Unmap()
	}
	return out, pts
}

// The parallel decoder gives jpegdec's pictures, in order, with the same
// timestamps, for 4:2:0 and 4:2:2 Motion JPEG.
func TestMatchesJpegdec(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Skip("jpegdec missing")
	}
	dir := t.TempDir()
	for _, pf := range []string{"yuvj420p", "yuvj422p"} {
		path := filepath.Join(dir, pf+".mov")
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
			"testsrc2=size=320x240:rate=30:duration=1", "-c:v", "mjpeg", "-q:v", "3", "-pix_fmt", pf, path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("ffmpeg: %v %s", err, out)
		}
		want, wpts := frames(t, path, "jpegdec")
		got, gpts := frames(t, path, "cutepimjpegdec n-workers=3")
		if len(got) != len(want) || len(want) != 30 {
			t.Fatalf("%s: %d frames, jpegdec %d, want 30", pf, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Errorf("%s frame %d differs from jpegdec", pf, i)
			}
			if gpts[i] != wpts[i] {
				t.Errorf("%s frame %d at %v, jpegdec %v", pf, i, gpts[i], wpts[i])
			}
		}
	}
}

// Decode speed of real files, flat out (not an assertion), jpegdec against
// this element, and which decoder decodebin3 picks: CUTEPI_MJPEG_BENCH,
// colon-separated.
func TestThroughput(t *testing.T) {
	list := os.Getenv("CUTEPI_MJPEG_BENCH")
	if list == "" {
		t.Skip("CUTEPI_MJPEG_BENCH not set")
	}
	gst.Init(nil)
	Register()
	run := func(desc string) (time.Duration, string) {
		pipe, err := gst.NewPipelineFromString(desc)
		if err != nil {
			t.Fatal(err)
		}
		used := ""
		if d, _ := pipe.GetElementByName("d"); d != nil {
			d.Connect("deep-element-added", func(_ *gst.Element, _ *gst.Bin, el *gst.Element) {
				if fa := el.GetFactory(); fa != nil && strings.Contains(fa.GetName(), "jpegdec") && used == "" {
					used = fa.GetName()
				}
			})
		}
		start := time.Now()
		pipe.SetState(gst.StatePlaying)
		pipe.GetBus().TimedPopFiltered(gst.ClockTime(120*time.Second), gst.MessageEOS|gst.MessageError)
		el := time.Since(start)
		pipe.SetState(gst.StateNull)
		return el, used
	}
	for _, f := range strings.Split(list, ":") {
		for _, dec := range []string{"jpegdec", "cutepimjpegdec"} {
			el, _ := run("filesrc location=" + f + " ! qtdemux ! jpegparse ! " + dec + " ! fakevideosink sync=false")
			t.Logf("%s: %s %.2f s", filepath.Base(f), dec, el.Seconds())
		}
		el, used := run("filesrc location=" + f + " ! decodebin3 name=d ! fakevideosink sync=false")
		t.Logf("%s: decodebin3 picks %s, %.2f s", filepath.Base(f), used, el.Seconds())
	}
}
