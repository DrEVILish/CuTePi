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

// writeGIF writes frames (each a 4x4 picture on a canvas of cw x ch) to a
// GIF whose palette reserves a transparent colour, as ffmpeg's does.
func writeGIF(t *testing.T, path string, cw, ch int, frames ...*image.Paletted) string {
	t.Helper()
	g := &gif.GIF{Image: frames, Delay: make([]int, len(frames)), Config: image.Config{Width: cw, Height: ch}}
	g.Config.ColorModel = frames[0].Palette
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := gif.EncodeAll(f, g); err != nil {
		t.Fatal(err)
	}
	return path
}

var gifPal = color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}, color.RGBA{0, 0, 0, 0}}

// An animation whose first frame is opaque but whose later frames use the
// transparent colour for unchanged pixels (how ffmpeg encodes) is opaque.
func TestGIFTransparentOnlyFirstFrameCounts(t *testing.T) {
	dir := t.TempDir()
	f1 := image.NewPaletted(image.Rect(0, 0, 4, 4), gifPal)
	f2 := image.NewPaletted(image.Rect(0, 0, 4, 4), gifPal)
	for i := range f2.Pix {
		f2.Pix[i] = 2
	}
	if GIFTransparent(writeGIF(t, filepath.Join(dir, "a.gif"), 4, 4, f1, f2)) {
		t.Error("opaque first frame, delta-coded later frames: reported transparent")
	}
}

// A first frame smaller than the canvas leaves the rest of the canvas
// transparent.
func TestGIFTransparentSmallFirstFrame(t *testing.T) {
	f1 := image.NewPaletted(image.Rect(0, 0, 4, 4), gifPal)
	if !GIFTransparent(writeGIF(t, filepath.Join(t.TempDir(), "s.gif"), 8, 8, f1)) {
		t.Error("first frame 4x4 on an 8x8 canvas: not reported transparent")
	}
}

// Not a GIF, or a GIF cut short: never alpha (and never a panic).
func TestGIFTransparentNotAGIF(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "x.gif")
	os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n not a gif"), 0o644)
	if GIFTransparent(png) {
		t.Error("PNG bytes named .gif reported transparent")
	}
	f1 := image.NewPaletted(image.Rect(0, 0, 4, 4), gifPal)
	f1.Pix[0] = 2
	full := writeGIF(t, filepath.Join(dir, "t.gif"), 4, 4, f1)
	b, _ := os.ReadFile(full)
	cut := filepath.Join(dir, "cut.gif")
	os.WriteFile(cut, b[:20], 0o644)
	if GIFTransparent(cut) {
		t.Error("truncated GIF reported transparent")
	}
}

// streamHasAlpha: VP8/VP9 alpha comes from the Matroska alpha_mode tag (any
// case), not the pixel format; a GIF is judged by its pixels, not BGRA.
func TestStreamHasAlpha(t *testing.T) {
	dir := t.TempDir()
	opaque := writeGIF(t, filepath.Join(dir, "o.gif"), 4, 4, image.NewPaletted(image.Rect(0, 0, 4, 4), gifPal))
	cases := []struct {
		s    ffprobeStream
		path string
		want bool
	}{
		{ffprobeStream{CodecName: "vp9", PixFmt: "yuv420p", Tags: map[string]string{"alpha_mode": "1"}}, "", true},
		{ffprobeStream{CodecName: "vp9", PixFmt: "yuv420p", Tags: map[string]string{"ALPHA_MODE": "1"}}, "", true},
		{ffprobeStream{CodecName: "vp9", PixFmt: "yuv420p", Tags: map[string]string{"alpha_mode": "0"}}, "", false},
		{ffprobeStream{CodecName: "vp9", PixFmt: "yuv420p"}, "", false},
		{ffprobeStream{CodecName: "prores", PixFmt: "yuva444p10le"}, "", true},
		{ffprobeStream{CodecName: "gif", PixFmt: "bgra"}, opaque, false},
	}
	for i, c := range cases {
		if got := streamHasAlpha(&c.s, c.path); got != c.want {
			t.Errorf("case %d (%s %s %v): %v, want %v", i, c.s.CodecName, c.s.PixFmt, c.s.Tags, got, c.want)
		}
	}
}
