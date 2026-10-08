package routes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A non-HEVC download (VP9 + Opus WebM, as YouTube sends) comes out as HEVC
// in MKV with its audio kept, under the same name, and the original is gone.
func TestHevcTranscode(t *testing.T) {
	if exec.Command("ffmpeg", "-hide_banner", "-h", "encoder=libx265").Run() != nil {
		t.Skip("ffmpeg with libx265 not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "Clip_Title.webm")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=s=320x180:r=30:d=1",
		"-f", "lavfi", "-i", "sine=d=1", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-c:a", "libopus", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot make the VP9 test clip: %v %s", err, out)
	}
	ctx := context.Background()
	if c, _ := probeVideoCodec(ctx, src); c != "vp9" {
		t.Fatalf("test clip codec %q", c)
	}
	var last float64
	out, err := hevcTranscode(ctx, src, func(pct float64, _ string) { last = pct })
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Clip_Title.mkv"); out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	if c, _ := probeVideoCodec(ctx, out); c != "hevc" {
		t.Errorf("output codec %q, want hevc", c)
	}
	a, _ := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", out).Output()
	if strings.TrimSpace(string(a)) != "opus" {
		t.Errorf("audio %q, want opus copied", a)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("original still present: %v", err)
	}
	if last != 100 {
		t.Errorf("last progress %v, want 100", last)
	}
}
