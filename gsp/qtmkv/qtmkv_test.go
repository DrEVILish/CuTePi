package qtmkv

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

// decode plays uri through uridecodebin3 (autoplugging, as CuTePi) into RGBA
// frames, and reports whether cutepiqtmkvparse was plugged.
func decode(t *testing.T, uri string) (frames [][]byte, caps string, plugged bool) {
	t.Helper()
	pipe, err := gst.NewPipelineFromString("uridecodebin3 uri=" + uri + " ! videoconvert ! video/x-raw,format=RGBA ! appsink name=k sync=false max-buffers=4")
	if err != nil {
		t.Fatal(err)
	}
	pipe.Connect("deep-element-added", func(_, _, e any) {
		// Bins arrive as *gst.Bin, other elements as *gst.Element.
		if el, ok := e.(*gst.Element); ok {
			if f := el.GetFactory(); f != nil && f.GetName() == "cutepiqtmkvparse" {
				plugged = true
			}
		}
	})
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
		b.Unmap()
	}
	return frames, caps, plugged
}

// HAP (with and without alpha) and QuickTime Animation in MKV decode, through
// autoplugging, to the same frames as the same streams in MOV.
func TestQuickTimeInMatroska(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	gst.Init(nil)
	if !Register() {
		t.Fatal("not registered")
	}
	dir := t.TempDir()
	for _, c := range []struct{ name, args string }{
		{"hap", "-c:v hap"}, {"hap_alpha", "-c:v hap -format hap_alpha -pix_fmt rgba"}, {"qtrle", "-c:v qtrle"},
	} {
		for _, ext := range []string{"mov", "mkv"} {
			out := filepath.Join(dir, c.name+"."+ext)
			args := append([]string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=64x48:rate=25", "-frames:v", "10"},
				append(splitArgs(c.args), out)...)
			if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("%s: ffmpeg: %v %s", out, err, b)
			}
		}
		if c.name == "qtrle" {
			// FFmpeg's Matroska muxer zeroes the sample description after
			// the fourcc; write the depth (24) as a correct file has it.
			patchDepth(t, filepath.Join(dir, "qtrle.mkv"), 24)
		}
		want, _, _ := decode(t, "file://"+filepath.Join(dir, c.name+".mov"))
		got, _, plugged := decode(t, "file://"+filepath.Join(dir, c.name+".mkv"))
		if !plugged {
			t.Errorf("%s.mkv: cutepiqtmkvparse not plugged", c.name)
		}
		if len(want) == 0 || len(got) != len(want) {
			t.Fatalf("%s: MKV %d frames, MOV %d", c.name, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(want[i], got[i]) {
				t.Errorf("%s frame %d: MKV and MOV differ", c.name, i)
			}
		}
	}
}

// patchDepth writes depth into the QuickTime sample description in the
// file's first CodecPrivate (size field, then the description: depth is 78
// bytes after its fourcc).
func patchDepth(t *testing.T, path string, depth int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte{0x63, 0xA2})
	if i < 0 || b[i+2]&0x80 == 0 {
		t.Fatalf("%s: no one-byte-size CodecPrivate", path)
	}
	o := i + 3 + 4 + 78
	b[o], b[o+1] = byte(depth>>8), byte(depth)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An FFmpeg-written QuickTime Animation MKV (no depth) is refused with an
// error rather than decoded as the wrong layout.
func TestQuickTimeAnimationWithoutDepth(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	gst.Init(nil)
	Register()
	out := filepath.Join(t.TempDir(), "q.mkv")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x48:rate=25", "-frames:v", "3", "-c:v", "qtrle", out).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, b)
	}
	if frames, _, _ := decode(t, "file://"+out); len(frames) != 0 {
		t.Errorf("%d frames decoded from a description without a depth", len(frames))
	}
}

func splitArgs(s string) []string {
	var out []string
	for _, f := range bytes.Fields([]byte(s)) {
		out = append(out, string(f))
	}
	return out
}
