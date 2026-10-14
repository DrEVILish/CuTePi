package gsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
)

// A real (if tiny) WAV decodes; bytes that are no media at all are refused
// with a reason.
func TestCheckDecodable(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(wav, silentWAV(8000, 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDecodable(wav); err != nil {
		t.Fatalf("wav refused: %v", err)
	}
	junk := filepath.Join(dir, "junk.mp4")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not a video ", 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDecodable(junk); err == nil {
		t.Fatal("junk bytes passed the decode check")
	}
}

// silentWAV builds a mono 16-bit PCM WAV of n samples at rate Hz.
func silentWAV(rate, n int) []byte {
	data := n * 2
	b := []byte("RIFF")
	le := func(v, size int) {
		for i := 0; i < size; i++ {
			b = append(b, byte(v>>(8*i)))
		}
	}
	le(36+data, 4)
	b = append(b, "WAVEfmt "...)
	le(16, 4)
	le(1, 2)
	le(1, 2)
	le(rate, 4)
	le(rate*2, 4)
	le(2, 2)
	le(16, 2)
	b = append(b, "data"...)
	le(data, 4)
	return append(b, make([]byte, data)...)
}

// Animated images report their duration (the sum of the frame delays) from
// the decode check: ffprobe has none for APNG and animated WebP.
func TestDecodeCheckAnimatedDuration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	for _, c := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"anim.apng", []string{"-c:v", "apng", "-plays", "0", "-f", "apng"}, 800 * time.Millisecond},
		{"anim.webp", []string{"-c:v", "libwebp_anim", "-loop", "0"}, 800 * time.Millisecond},
		{"still.png", []string{"-frames:v", "1"}, 0},
	} {
		p := filepath.Join(dir, c.name)
		args := append([]string{"-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=0.8"}, append(c.args, p)...)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", c.name, err, out)
		}
		d, err := DecodeCheck(p)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if diff := d - c.want; diff < -5*time.Millisecond || diff > 5*time.Millisecond {
			t.Errorf("%s: duration %v, want %v", c.name, d, c.want)
		}
	}
}

// The import's decode check runs its decoders on one thread (it prerolls
// one frame, inside the service, beside playback); audio decoders, which
// have no thread setting, are left alone.
func TestDecodeCheckOneThread(t *testing.T) {
	gstInit()
	for _, c := range []struct {
		factory, prop string
		want          any
	}{
		{"cutepidav1ddec", "n-threads", 1}, {"cutepidav1ddec", "max-frame-delay", 1},
		{"avdec_h264", "max-threads", 1}, {"vp9dec", "threads", uint(1)},
		{"cutepimjpegdec", "n-workers", 1}, {"cutepipngvdec", "n-workers", 1},
	} {
		e, err := gst.NewElement(c.factory)
		if err != nil {
			t.Logf("%s not installed", c.factory)
			continue
		}
		oneDecodeThread(e)
		if v, _ := e.GetProperty(c.prop); v != c.want {
			t.Errorf("%s %s = %v, want %v", c.factory, c.prop, v, c.want)
		}
	}
	if e, err := gst.NewElement("avdec_aac"); err == nil {
		oneDecodeThread(e) // must not touch a property it lacks
	}
}

// Repeated checks keep finding FFmpeg's decoders. A one-thread hook that read
// each factory's klass through go-gst's GetMetadata (which frees a string
// GStreamer owns) corrupted the factories: from the second check on, ProRes,
// HAP and AC-3 had "no GStreamer decoder" until the service restarted.
func TestDecodeCheckRepeated(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	gstInit()
	dir := t.TempDir()
	files := map[string][]string{
		"p.mov": {"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=25", "-frames:v", "5", "-c:v", "prores_ks"},
		"h.mov": {"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=25", "-frames:v", "5", "-c:v", "hap"},
		"a.mkv": {"-f", "lavfi", "-i", "sine=duration=1", "-c:a", "ac3"},
	}
	for name, args := range files {
		out := filepath.Join(dir, name)
		if b, err := exec.Command("ffmpeg", append(append([]string{"-v", "error", "-y"}, args...), out)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, b)
		}
	}
	for i := 0; i < 4; i++ {
		for name := range files {
			if err := CheckDecodable(filepath.Join(dir, name)); err != nil {
				t.Fatalf("check %d of %s: %v", i+1, name, err)
			}
		}
		runtime.GC()
	}
}
