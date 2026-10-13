package pngvdec

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

// decode returns path's frames as RGBA through the given chain, and the
// decoder decodebin3 used ("" when the chain names one).
func decode(t *testing.T, chain string) (frames [][]byte, used string) {
	t.Helper()
	pipe, err := gst.NewPipelineFromString(chain + " ! videoconvert ! video/x-raw,format=RGBA ! appsink name=k sync=false max-buffers=4")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := pipe.GetElementByName("d"); d != nil {
		d.Connect("deep-element-added", func(_ *gst.Element, _ *gst.Bin, el *gst.Element) {
			if f := el.GetFactory(); f != nil && (f.GetName() == "pngdec" || f.GetName() == "cutepipngvdec") {
				used = f.GetName()
			}
		})
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	for {
		s := sink.TryPullSample(gst.ClockTime(10 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		frames = append(frames, append([]byte(nil), m.Bytes()...))
		b.Unmap()
	}
	return frames, used
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg: %v %s", err, out)
	}
}

// PNG video (RGBA with a translucent half, and RGB) decodes to pngdec's
// pictures in order through decodebin3, which picks this element; a PNG still
// stays with pngdec.
func TestMatchesPngdec(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Skip("libpng16 missing")
	}
	dir := t.TempDir()
	for _, c := range []struct{ name, pixfmt string }{{"rgba.mov", "rgba"}, {"rgb.mkv", "rgb24"}} {
		path := filepath.Join(dir, c.name)
		ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(X,160),255,90)'",
			"-c:v", "png", "-pix_fmt", c.pixfmt, path)
		demux := "qtdemux"
		if filepath.Ext(path) == ".mkv" {
			demux = "matroskademux"
		}
		want, _ := decode(t, "filesrc location="+path+" ! "+demux+" ! pngdec")
		got, used := decode(t, "filesrc location="+path+" ! decodebin3 name=d")
		if used != "cutepipngvdec" {
			t.Errorf("%s: decodebin3 used %q", c.name, used)
		}
		if len(got) != len(want) || len(want) != 30 {
			t.Fatalf("%s: %d frames, pngdec %d, want 30", c.name, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Errorf("%s frame %d differs from pngdec", c.name, i)
				break
			}
		}
	}
	still := filepath.Join(dir, "still.png")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=320x240", "-frames:v", "1", still)
	if f, used := decode(t, "filesrc location="+still+" ! decodebin3 name=d"); used != "pngdec" || len(f) != 1 {
		t.Errorf("still: %d frames through %q, want 1 through pngdec", len(f), used)
	}
}

// Decode speed of real files through decodebin3, flat out (not an
// assertion): CUTEPI_PNGV_BENCH, colon-separated.
func TestThroughput(t *testing.T) {
	list := os.Getenv("CUTEPI_PNGV_BENCH")
	if list == "" {
		t.Skip("CUTEPI_PNGV_BENCH not set")
	}
	gst.Init(nil)
	Register()
	for _, f := range strings.Split(list, ":") {
		for _, chain := range []string{"filesrc location=" + f + " ! qtdemux ! pngdec", "filesrc location=" + f + " ! decodebin3"} {
			pipe, _ := gst.NewPipelineFromString(chain + " ! fakevideosink sync=false")
			start := time.Now()
			pipe.SetState(gst.StatePlaying)
			pipe.GetBus().TimedPopFiltered(gst.ClockTime(120*time.Second), gst.MessageEOS|gst.MessageError)
			t.Logf("%s: %s: %.2f s", filepath.Base(f), chain[strings.LastIndex(chain, "!")+2:], time.Since(start).Seconds())
			pipe.SetState(gst.StateNull)
		}
	}
}
