package media

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestImageAnimation builds still and animated GIF, PNG/APNG and WebP files
// with ffmpeg and checks the header reader tells them apart, loop count included.
func TestImageAnimation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	src := []string{"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=1"}
	cases := []struct {
		name string
		args []string
		want Animation
	}{
		{"still.gif", []string{"-frames:v", "1"}, Animation{}},
		{"anim.gif", []string{"-loop", "0"}, Animation{Animated: true, LoopForever: true, Loops: 1}},
		{"anim3.gif", []string{"-loop", "2"}, Animation{Animated: true, Loops: 3}},
		{"once.gif", []string{"-loop", "-1"}, Animation{Animated: true, Loops: 1}},
		{"still.png", []string{"-frames:v", "1"}, Animation{}},
		{"anim.apng", []string{"-c:v", "apng", "-plays", "0", "-f", "apng"}, Animation{Animated: true, LoopForever: true, Loops: 1}},
		{"still.webp", []string{"-frames:v", "1", "-c:v", "libwebp"}, Animation{}},
		{"anim.webp", []string{"-c:v", "libwebp_anim", "-loop", "0"}, Animation{Animated: true, LoopForever: true, Loops: 1}},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name)
		args := append(append([]string{"-v", "error", "-y"}, src...), append(c.args, p)...)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Logf("%s: ffmpeg cannot make it here (%v: %s); skipped", c.name, err, out)
			continue
		}
		if got := ImageAnimation(p); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	if got := ImageAnimation(filepath.Join(dir, "missing.gif")); got.Animated {
		t.Errorf("missing file reported animated")
	}
}
