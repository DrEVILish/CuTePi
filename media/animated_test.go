package media

import (
	"image"
	"image/color"
	"image/gif"
	"os"
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

func TestPixFmtHasAlpha(t *testing.T) {
	for f, want := range map[string]bool{
		"yuva420p": true, "yuva444p10le": true, "rgba": true, "bgra": true, "argb": true, "abgr": true,
		"gbrap12le": true, "ya8": true, "rgba64le": true,
		"yuv420p": false, "yuv422p10le": false, "rgb24": false, "gbrp12le": false, "nv12": false, "pal8": false, "": false,
	} {
		if got := PixFmtHasAlpha(f); got != want {
			t.Errorf("PixFmtHasAlpha(%q) = %v, want %v", f, got, want)
		}
	}
	vp9 := &MediaInfo{Video: &MediaVideoInfo{PixFmt: "yuv420p", Alpha: true}}
	old := &MediaInfo{Video: &MediaVideoInfo{PixFmt: "yuva420p"}}
	oldGIF := &MediaInfo{Video: &MediaVideoInfo{Codec: "gif", PixFmt: "bgra"}}
	if !vp9.HasAlpha() || !old.HasAlpha() || oldGIF.HasAlpha() || (&MediaInfo{}).HasAlpha() || (*MediaInfo)(nil).HasAlpha() {
		t.Error("HasAlpha: flag, pixel-format fallback or empty info wrong")
	}
}

func TestGIFTransparent(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, transparent bool) string {
		pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}, color.RGBA{0, 0, 0, 0}}
		img := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
		if transparent {
			img.SetColorIndex(1, 1, 2)
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := gif.Encode(f, img, nil); err != nil {
			t.Fatal(err)
		}
		return f.Name()
	}
	// The opaque file still carries a transparent palette entry, as encoders
	// reserve one: only pixels that use it count.
	if !GIFTransparent(write("t.gif", true)) {
		t.Error("transparent GIF not detected")
	}
	if GIFTransparent(write("o.gif", false)) {
		t.Error("opaque GIF with a reserved transparent colour reported transparent")
	}
	if GIFTransparent(filepath.Join(dir, "missing.gif")) {
		t.Error("missing file reported transparent")
	}
}
