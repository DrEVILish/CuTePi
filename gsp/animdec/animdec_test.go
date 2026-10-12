package animdec

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

// played is what came out of filesrc ! decodebin3 for one file.
type played struct {
	frames    int
	format    string
	w, h      int
	fpsN      int
	pts       []time.Duration
	durs      []time.Duration
	alphaSeen bool // a pixel with 0 < alpha < 255 or alpha 0
	decoder   string
}

// play decodes path through decodebin3 to an appsink (RGBA), optionally
// flush-seeking back to 0 after seekAfter frames and counting the frames
// after the seek separately (afterSeek).
func play(t *testing.T, path string, seekAfter int) (p played, afterSeek int) {
	t.Helper()
	pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! decodebin3 name=d ! videoconvert ! video/x-raw,format=RGBA ! appsink name=k sync=false max-buffers=4 max-buffers=4")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := pipe.GetElementByName("d")
	d.Connect("deep-element-added", func(_ *gst.Element, _ *gst.Bin, el *gst.Element) {
		if f := el.GetFactory(); f != nil {
			switch f.GetName() {
			case "cutepianimdec", "webpdec", "pngdec", "avdec_png":
				p.decoder = f.GetName()
			}
		}
	})
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	seeked := false
	for {
		gcNow(p.frames + afterSeek)
		s := sink.TryPullSample(gst.ClockTime(10 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		if seeked {
			afterSeek++
		} else {
			p.frames++
			p.pts = append(p.pts, time.Duration(b.PresentationTimestamp()))
			p.durs = append(p.durs, time.Duration(b.Duration()))
			if p.frames == 1 {
				st := s.GetCaps().GetStructureAt(0)
				if f, err := st.GetValue("format"); err == nil {
					p.format, _ = f.(string)
				}
				w, _ := st.GetValue("width")
				h, _ := st.GetValue("height")
				p.w, p.h = w.(int), h.(int)
				if fr, err := st.GetValue("framerate"); err == nil {
					if f, ok := fr.(*gst.FractionValue); ok {
						p.fpsN = f.Num()
					}
				}
			}
			if !p.alphaSeen && p.frames <= 3 { // a translucent area shows in the first frames
				m := b.Map(gst.MapRead)
				data := m.Bytes()
				for i := 3; i < len(data); i += 4 * 97 {
					if data[i] != 255 {
						p.alphaSeen = true
						break
					}
				}
				b.Unmap()
			}
			if seekAfter > 0 && p.frames == seekAfter {
				if !pipe.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush) {
					t.Errorf("%s: flush seek to 0 refused", filepath.Base(path))
					return
				}
				seeked = true
			}
		}
		if seeked && afterSeek >= 3 {
			break
		}
	}
	return p, afterSeek
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg %v: %v %s", args, err, out)
	}
}

// Animated WebP, APNG and AVIF made here (8 frames, 10 fps, the right half
// translucent) play every frame, in order, at the file's
// timing, with their alpha; a flush seek to 0 (how a cue loops) starts over.
func TestAnimatedFormats(t *testing.T) {
	gst.Init(nil)
	have := Register()
	if have == "" {
		t.Skip("cutepianimdec not registered")
	}
	t.Logf("formats: %s", have)
	dir := t.TempDir()
	// testsrc2 (every frame differs: an animation encoder merges identical
	// frames) with the right half translucent.
	src := []string{"-f", "lavfi", "-i", "testsrc2=s=320x240:r=10:d=0.8,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(X,160),255,90)'"}
	cases := []struct {
		name string
		args []string
	}{
		{"anim.webp", []string{"-c:v", "libwebp_anim", "-lossless", "1", "-loop", "0"}},
		{"anim.apng", []string{"-c:v", "apng", "-plays", "0", "-f", "apng"}},
		// AVIF carries alpha as a second (auxiliary) sequence.
		{"anim.avif", []string{"-filter_complex", "[0]split[c][a];[c]format=yuv420p[c2];[a]alphaextract[a2]", "-map", "[c2]", "-map", "[a2]",
			"-c:v", "libaom-av1", "-cpu-used", "8", "-f", "avif"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			ffmpeg(t, append(append(append([]string{}, src...), c.args...), path)...)
			p, after := play(t, path, 4)
			if p.decoder != "cutepianimdec" {
				t.Errorf("decoder %q, want cutepianimdec", p.decoder)
			}
			if p.frames != 4 || after < 3 {
				t.Fatalf("%d frames before the seek (want 4), %d after (want 3)", p.frames, after)
			}
			if p.w != 320 || p.h != 240 || p.fpsN == 0 {
				t.Errorf("caps %dx%d fps %d, want 320x240 and a non-zero (animated) rate", p.w, p.h, p.fpsN)
			}
			for i := range p.pts {
				want := time.Duration(i) * 100 * time.Millisecond
				if d := p.pts[i] - want; d < -2*time.Millisecond || d > 2*time.Millisecond {
					t.Errorf("frame %d at %v, want %v", i, p.pts[i], want)
				}
				if d := p.durs[i] - 100*time.Millisecond; d < -2*time.Millisecond || d > 2*time.Millisecond {
					t.Errorf("frame %d lasts %v, want 100ms", i, p.durs[i])
				}
			}
			if !p.alphaSeen && p.frames <= 3 { // a translucent area shows in the first frames
				t.Errorf("no transparent pixels: alpha lost")
			}
		})
	}
}

// Stills still decode (one frame, framerate 0/1 so the wall treats them as
// stills), and a plain PNG stays with pngdec (the APNG type finder leaves it).
func TestStills(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	dir := t.TempDir()
	cases := []struct {
		name, decoder string
		args          []string
	}{
		{"still.webp", "cutepianimdec", []string{"-c:v", "libwebp"}},
		{"still.png", "pngdec", []string{"-c:v", "png"}},
		{"still.avif", "cutepianimdec", []string{"-c:v", "libaom-av1", "-cpu-used", "8", "-frames:v", "1", "-f", "avif"}},
		{"still.jxl", "cutepianimdec", []string{"-c:v", "libjxl", "-frames:v", "1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			ffmpeg(t, append(append([]string{"-f", "lavfi", "-i", "testsrc2=s=320x240:d=1"}, c.args...), "-frames:v", "1", path)...)
			if _, err := os.Stat(path); err != nil {
				t.Skip("not made")
			}
			p, _ := play(t, path, 0)
			if p.decoder != c.decoder {
				t.Errorf("decoder %q, want %q", p.decoder, c.decoder)
			}
			if p.frames != 1 || p.w != 320 || p.h != 240 {
				t.Errorf("%d frames %dx%d, want 1 at 320x240", p.frames, p.w, p.h)
			}
		})
	}
}

// Animated JPEG XL against libjxl's conformance set (FFmpeg 7.1 cannot make
// one): every frame, with the duration its test.json lists. The files are
// fetched outside the repo (CUTEPI_ANIM_TESTDATA, default the test Pi's
// /opt/raspberrypi-codecs/animated-images/testdata); skipped without them.
func TestJXLConformance(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	dir := os.Getenv("CUTEPI_ANIM_TESTDATA")
	if dir == "" {
		dir = "/opt/raspberrypi-codecs/animated-images/testdata"
	}
	files, _ := filepath.Glob(filepath.Join(dir, "animation_*.jxl"))
	if len(files) == 0 {
		t.Skip("no conformance files in " + dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(strings.TrimSuffix(f, ".jxl") + ".json")
			if err != nil {
				t.Skip("no test.json")
			}
			var ref struct {
				Frames []struct{ Duration float64 } `json:"frames"`
			}
			if err := json.Unmarshal(raw, &ref); err != nil {
				t.Fatal(err)
			}
			p, _ := play(t, f, 0)
			if p.decoder != "cutepianimdec" {
				t.Errorf("decoder %q", p.decoder)
			}
			if p.frames != len(ref.Frames) {
				t.Fatalf("%d frames, want %d", p.frames, len(ref.Frames))
			}
			var at time.Duration
			for i, fr := range ref.Frames {
				want := time.Duration(fr.Duration * float64(time.Second))
				if d := p.durs[i] - want; d < -time.Millisecond || d > time.Millisecond {
					t.Errorf("frame %d lasts %v, want %v", i, p.durs[i], want)
				}
				if d := p.pts[i] - at; d < -time.Millisecond || d > time.Millisecond {
					t.Errorf("frame %d at %v, want %v", i, p.pts[i], at)
				}
				at += want
			}
		})
	}
}

// APNG compositing (sub-rectangle frames, dispose and blend operations, which
// FFmpeg's apng encoder uses to make deltas) gives the same pixels as
// FFmpeg's apng decoder, frame for frame.
func TestAPNGMatchesFFmpeg(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "delta.apng")
	// A moving translucent box over a still card: frames differ in a small
	// region, so the encoder writes deltas with blending.
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=s=200x150:r=10:d=1.2,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(Y,75),255,128)'",
		"-f", "lavfi", "-i", "color=c=blue@0.5:s=40x40:r=10,format=rgba",
		"-filter_complex", "[0][1]overlay=x='mod(n*17,150)':y=40:shortest=1", "-c:v", "apng", "-plays", "0", "-f", "apng", path)
	ref := filepath.Join(dir, "ref.rgba")
	ffmpeg(t, "-i", path, "-f", "rawvideo", "-pix_fmt", "rgba", ref)
	want, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! decodebin3 ! videoconvert ! video/x-raw,format=RGBA ! appsink name=k sync=false max-buffers=4 max-buffers=4")
	if err != nil {
		t.Fatal(err)
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	const fsz = 200 * 150 * 4
	n := 0
	for ; ; n++ {
		gcNow(n)
		s := sink.TryPullSample(gst.ClockTime(5 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		got := m.Bytes()
		if (n+1)*fsz <= len(want) {
			exp := want[n*fsz : (n+1)*fsz]
			diff := 0
			for i := range exp {
				if d := int(got[i]) - int(exp[i]); d > 1 || d < -1 {
					diff++
				}
			}
			if diff > 0 {
				t.Errorf("frame %d: %d bytes differ by more than 1 from FFmpeg", n, diff)
			}
		}
		b.Unmap() // the wrapper that mapped it: a map holds the frame
	}
	if n != len(want)/fsz {
		t.Errorf("%d frames, FFmpeg decodes %d", n, len(want)/fsz)
	}
}

// apngFrame is one frame of a hand-built APNG: a w x h straight-alpha RGBA
// patch at x, y with its dispose and blend operations.
type apngFrame struct {
	x, y, w, h     int
	px             [4]byte // the patch is one colour
	dispose, blend byte
}

// buildAPNG writes an APNG (canvas cw x ch, first frame is the default image).
func buildAPNG(t *testing.T, path string, cw, ch int, frames []apngFrame) {
	t.Helper()
	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	chunk := func(typ string, body []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(body)))
		out.Write(l[:])
		c := crc32.NewIEEE()
		c.Write([]byte(typ))
		c.Write(body)
		out.WriteString(typ)
		out.Write(body)
		binary.BigEndian.PutUint32(l[:], c.Sum32())
		out.Write(l[:])
	}
	idat := func(f apngFrame) []byte { // the patch as 8-bit RGBA rows (filter 0), zlib-compressed
		var raw bytes.Buffer
		for y := 0; y < f.h; y++ {
			raw.WriteByte(0)
			for x := 0; x < f.w; x++ {
				raw.Write(f.px[:])
			}
		}
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(raw.Bytes())
		zw.Close()
		return z.Bytes()
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, uint32(cw))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(ch))
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	actl := make([]byte, 8)
	binary.BigEndian.PutUint32(actl, uint32(len(frames)))
	chunk("acTL", actl)
	seq := uint32(0)
	for i, f := range frames {
		fc := make([]byte, 26)
		binary.BigEndian.PutUint32(fc, seq)
		seq++
		binary.BigEndian.PutUint32(fc[4:], uint32(f.w))
		binary.BigEndian.PutUint32(fc[8:], uint32(f.h))
		binary.BigEndian.PutUint32(fc[12:], uint32(f.x))
		binary.BigEndian.PutUint32(fc[16:], uint32(f.y))
		binary.BigEndian.PutUint16(fc[20:], 1)
		binary.BigEndian.PutUint16(fc[22:], 10)
		fc[24], fc[25] = f.dispose, f.blend
		chunk("fcTL", fc)
		if i == 0 {
			chunk("IDAT", idat(f))
			continue
		}
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body, seq)
		seq++
		chunk("fdAT", append(body, idat(f)...))
	}
	chunk("IEND", nil)
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every dispose (none, background, previous) and blend (source, over)
// operation, against a canvas worked out here from the APNG specification.
func TestAPNGDisposeBlend(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	const cw, ch = 64, 48
	frames := []apngFrame{
		{0, 0, cw, ch, [4]byte{200, 0, 0, 255}, 0, 0},    // opaque red canvas, kept
		{8, 8, 16, 16, [4]byte{0, 0, 255, 128}, 1, 1},    // half blue OVER red, then cleared to transparent
		{20, 10, 20, 20, [4]byte{0, 255, 0, 255}, 2, 0},  // green, then the canvas before it comes back
		{30, 20, 10, 10, [4]byte{255, 255, 0, 64}, 0, 1}, // faint yellow OVER whatever is there
		{0, 30, 64, 18, [4]byte{10, 20, 30, 0}, 0, 0},    // a transparent strip (SOURCE replaces)
	}
	path := filepath.Join(t.TempDir(), "ops.apng")
	buildAPNG(t, path, cw, ch, frames)

	// Expected canvases, straight alpha, as the specification describes.
	over := func(d, s [4]byte) [4]byte {
		sa, da := int(s[3]), int(d[3])
		oa := sa*255 + da*(255-sa)
		if oa == 0 {
			return [4]byte{}
		}
		var r [4]byte
		for c := 0; c < 3; c++ {
			r[c] = byte((int(s[c])*sa*255 + int(d[c])*da*(255-sa) + oa/2) / oa)
		}
		r[3] = byte((oa + 127) / 255)
		return r
	}
	canvas := make([][4]byte, cw*ch)
	var want [][]byte
	var saved [][4]byte
	var prev *apngFrame
	for i := range frames {
		f := frames[i]
		if prev != nil {
			switch prev.dispose {
			case 1:
				for y := prev.y; y < prev.y+prev.h; y++ {
					for x := prev.x; x < prev.x+prev.w; x++ {
						canvas[y*cw+x] = [4]byte{}
					}
				}
			case 2:
				copy(canvas, saved)
			}
		}
		if f.dispose == 2 {
			saved = append([][4]byte(nil), canvas...)
		}
		for y := f.y; y < f.y+f.h; y++ {
			for x := f.x; x < f.x+f.w; x++ {
				if f.blend == 1 {
					canvas[y*cw+x] = over(canvas[y*cw+x], f.px)
				} else {
					canvas[y*cw+x] = f.px
				}
			}
		}
		b := make([]byte, 0, cw*ch*4)
		for _, p := range canvas {
			b = append(b, p[:]...)
		}
		want = append(want, b)
		prev = &frames[i]
	}

	pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! decodebin3 ! appsink name=k sync=false max-buffers=4 max-buffers=4 caps=video/x-raw,format=RGBA")
	if err != nil {
		t.Fatal(err)
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	n := 0
	for ; ; n++ {
		gcNow(n)
		s := sink.TryPullSample(gst.ClockTime(5 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		got := m.Bytes()
		if n < len(want) {
			for i := 0; i < cw*ch*4; i++ {
				if d := int(got[i]) - int(want[n][i]); d > 1 || d < -1 {
					px := i / 4
					t.Errorf("frame %d pixel (%d,%d) channel %d: %d, want %d", n, px%cw, px/cw, i%4, got[i], want[n][i])
					break
				}
			}
		}
		b.Unmap() // the wrapper that mapped it: a map holds the frame
	}
	if n != len(frames) {
		t.Errorf("%d frames, want %d", n, len(frames))
	}
}

// Decode speed of real files, flat out (not an assertion): set
// CUTEPI_ANIM_BENCH to a colon-separated list of image files.
func TestThroughput(t *testing.T) {
	list := os.Getenv("CUTEPI_ANIM_BENCH")
	if list == "" {
		t.Skip("CUTEPI_ANIM_BENCH not set")
	}
	gst.Init(nil)
	Register()
	for _, f := range strings.Split(list, ":") {
		start := time.Now()
		p, _ := play(t, f, 0)
		el := time.Since(start)
		var anim time.Duration
		for _, d := range p.durs {
			anim += d
		}
		t.Logf("%s: %s, %d frames %dx%d alpha=%v in %.2f s = %.1f fps (animation %.2f s, %.1f fps)", filepath.Base(f), p.decoder,
			p.frames, p.w, p.h, p.alphaSeen, el.Seconds(), float64(p.frames)/el.Seconds(), anim.Seconds(), float64(p.frames)/anim.Seconds())
	}
}

// WebP frames decoded in parallel and composited here match libwebp's own
// animation decoder, frame for frame (the FFmpeg-made file, and the support
// corpus's 1080p file when present).
func TestWebPMatchesLibwebp(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	dir := t.TempDir()
	made := filepath.Join(dir, "anim.webp")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=s=200x150:r=10:d=1.2,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(Y,75),255,128)'",
		"-f", "lavfi", "-i", "color=c=blue@0.5:s=40x40:r=10,format=rgba",
		"-filter_complex", "[0][1]overlay=x='mod(n*17,150)':y=40:shortest=1", "-c:v", "libwebp_anim", "-lossless", "0", "-loop", "0", made)
	files := []string{made}
	if _, err := os.Stat("/root/cutepi-testmedia/support/image/webp_anim_30fps.webp"); err == nil {
		files = append(files, "/root/cutepi-testmedia/support/image/webp_anim_30fps.webp")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ref := newWebpRef(data)
			if ref == nil {
				t.Skip("libwebp reference decoder not available")
			}
			defer ref.close()
			w, h := ref.w, ref.h
			want := make([]byte, w*h*4)
			pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! decodebin3 ! appsink name=k sync=false max-buffers=4 max-buffers=4 caps=video/x-raw,format=RGBA")
			if err != nil {
				t.Fatal(err)
			}
			ke, _ := pipe.GetElementByName("k")
			sink := app.SinkFromElement(ke)
			pipe.SetState(gst.StatePlaying)
			defer pipe.SetState(gst.StateNull)
			n := 0
			for ; ; n++ {
				gcNow(n)
				s := sink.TryPullSample(gst.ClockTime(10 * time.Second))
				if s == nil {
					break
				}
				b := s.GetBuffer()
				m := b.Map(gst.MapRead)
				got := m.Bytes()
				if !ref.next(want) {
					t.Errorf("frame %d: libwebp has no more frames", n)
				} else if len(got) >= w*h*4 {
					for i := 0; i < w*h*4; i++ {
						if got[i] != want[i] {
							px := i / 4
							t.Errorf("frame %d pixel (%d,%d) channel %d: %d, libwebp %d", n, px%w, px/w, i%4, got[i], want[i])
							break
						}
					}
				}
				b.Unmap() // the wrapper that mapped it: a map holds the frame
			}
			if n != ref.n {
				t.Errorf("%d frames, libwebp %d", n, ref.n)
			}
		})
	}
}

// ffmpegFrames streams path's frames decoded by FFmpeg as w x h RGBA: next
// fills buf and reports false at the end.
type ffmpegFrames struct {
	cmd *exec.Cmd
	out io.ReadCloser
}

func newFFmpegFrames(t *testing.T, path string) *ffmpegFrames {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", path, "-f", "rawvideo", "-pix_fmt", "rgba", "-fps_mode", "passthrough", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skip("ffmpeg: ", err)
	}
	return &ffmpegFrames{cmd, out}
}

func (f *ffmpegFrames) next(buf []byte) bool {
	_, err := io.ReadFull(f.out, buf)
	return err == nil
}

// rest counts the frames FFmpeg has left, and waits for it.
func (f *ffmpegFrames) rest(size int) int {
	n := 0
	buf := make([]byte, size)
	for f.next(buf) {
		n++
	}
	f.cmd.Wait()
	return n
}

// samePixels compares two RGBA frames as shown: equal alpha, and equal colour
// wherever the pixel is not fully transparent (a transparent pixel's colour
// is never seen). Returns a description of the first difference.
func samePixels(got, want []byte, w int) string {
	for i := 0; i+3 < len(want); i += 4 {
		if got[i+3] != want[i+3] || (want[i+3] != 0 && (got[i] != want[i] || got[i+1] != want[i+1] || got[i+2] != want[i+2])) {
			px := i / 4
			return fmt.Sprintf("pixel (%d,%d): %v, want %v", px%w, px/w, got[i:i+4], want[i:i+4])
		}
	}
	return ""
}

// pull plays path through decodebin3 and calls f for each RGBA frame.
func pull(t *testing.T, path string, f func(n int, frame []byte)) int {
	t.Helper()
	pipe, err := gst.NewPipelineFromString("filesrc location=" + path + " ! decodebin3 ! appsink name=k sync=false max-buffers=4 max-buffers=4 caps=video/x-raw,format=RGBA")
	if err != nil {
		t.Fatal(err)
	}
	ke, _ := pipe.GetElementByName("k")
	sink := app.SinkFromElement(ke)
	pipe.SetState(gst.StatePlaying)
	defer pipe.SetState(gst.StateNull)
	n := 0
	for ; ; n++ {
		gcNow(n)
		s := sink.TryPullSample(gst.ClockTime(10 * time.Second))
		if s == nil {
			break
		}
		b := s.GetBuffer()
		m := b.Map(gst.MapRead)
		f(n, m.Bytes())
		b.Unmap() // the wrapper that mapped it: a map holds the frame
	}
	return n
}

// GIF (frames decoded in parallel, composited here) shows what FFmpeg's GIF
// decoder shows: an FFmpeg-made animation with transparency and sub-rectangle
// deltas, and the support corpus's GIFs when present.
func TestGIFMatchesFFmpeg(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	made := filepath.Join(t.TempDir(), "anim.gif")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=s=200x150:r=25:d=1.2,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(Y,75),255,0)'",
		"-f", "lavfi", "-i", "color=c=blue:s=40x40:r=25,format=rgba",
		"-filter_complex", "[0][1]overlay=x='mod(n*7,150)':y=40:shortest=1,split[a][b];[a]palettegen=reserve_transparent=1[p];[b][p]paletteuse=alpha_threshold=128",
		"-gifflags", "+transdiff+offsetting", "-loop", "0", made)
	files := []string{made}
	for _, f := range []string{"gif_anim.gif", "gif_anim_alpha.gif", "gif_anim_50fps.gif", "gif_alpha.gif", "gif.gif"} {
		if _, err := os.Stat("/root/cutepi-testmedia/support/image/" + f); err == nil {
			files = append(files, "/root/cutepi-testmedia/support/image/"+f)
		}
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) < 10 {
				t.Fatal(err)
			}
			w, h := int(raw[6])|int(raw[7])<<8, int(raw[8])|int(raw[9])<<8
			ref := newFFmpegFrames(t, path)
			want := make([]byte, w*h*4)
			bad, refN := 0, 0
			n := pull(t, path, func(i int, got []byte) {
				if !ref.next(want) {
					return
				}
				refN++
				if bad < 3 {
					if d := samePixels(got, want, w); d != "" {
						t.Errorf("frame %d: %s", i, d)
						bad++
					}
				}
			})
			refN += ref.rest(w * h * 4)
			if n != refN {
				t.Errorf("%d frames, FFmpeg %d", n, refN)
			}
		})
	}
}

// An interlaced GIF (rows stored 0, 8, 16, …, 4, 12, …, 2, 6, …, 1, 3, …)
// comes out in display order. Made by encoding the rows in stored order with
// image/gif (which never interlaces) and setting the descriptor's flag.
func TestGIFInterlaced(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	const w, h = 37, 29
	pal := color.Palette{}
	for i := 0; i < 256; i++ {
		pal = append(pal, color.RGBA{uint8(i), uint8(255 - i), uint8(i * 7), 255})
	}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, uint8(y*8+x))
		}
	}
	var order []int
	for _, ps := range [][2]int{{0, 8}, {4, 8}, {2, 4}, {1, 2}} {
		for y := ps[0]; y < h; y += ps[1] {
			order = append(order, y)
		}
	}
	stored := image.NewPaletted(img.Rect, pal)
	for i, y := range order {
		copy(stored.Pix[i*w:(i+1)*w], img.Pix[y*w:(y+1)*w])
	}
	var b bytes.Buffer
	if err := gif.Encode(&b, stored, &gif.Options{NumColors: 256}); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	i := bytes.IndexByte(data[13+3*256:], 0x2c) + 13 + 3*256 // the image descriptor (after the global table)
	data[i+9] |= 0x40
	path := filepath.Join(t.TempDir(), "interlaced.gif")
	os.WriteFile(path, data, 0o644)
	n := pull(t, path, func(_ int, got []byte) {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := pal[img.ColorIndexAt(x, y)].(color.RGBA)
				o := (y*w + x) * 4
				if got[o] != c.R || got[o+1] != c.G || got[o+2] != c.B || got[o+3] != 255 {
					t.Fatalf("pixel (%d,%d): %v, want %v", x, y, got[o:o+4], c)
				}
			}
		}
	})
	if n != 1 {
		t.Errorf("%d frames, want 1", n)
	}
}

// gcNow collects every 16 frames: go-gst releases a sample (and the pool
// buffer it holds) only from a finalizer.
func gcNow(n int) {
	if n%16 == 15 {
		runtime.GC()
	}
}

// AVIF sequences decoded by dav1d directly (frame threading) give the same
// pictures as libavif's own sequence decoding (CUTEPI_ANIMDEC_LIBAVIF=1).
func TestAVIFSequenceMatchesLibavif(t *testing.T) {
	gst.Init(nil)
	if Register() == "" {
		t.Skip("cutepianimdec not registered")
	}
	path := filepath.Join(t.TempDir(), "anim.avif")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=s=320x240:r=10:d=1.2,format=rgba,geq=r='r(X,Y)':g='g(X,Y)':b='b(X,Y)':a='if(lt(X,160),255,90+X/4)'",
		"-filter_complex", "[0]split[c][a];[c]format=yuv420p[c2];[a]alphaextract[a2]", "-map", "[c2]", "-map", "[a2]",
		"-c:v", "libaom-av1", "-cpu-used", "8", "-f", "avif", path)
	var ref [][]byte
	os.Setenv("CUTEPI_ANIMDEC_LIBAVIF", "1")
	pull(t, path, func(_ int, f []byte) { ref = append(ref, append([]byte(nil), f...)) })
	os.Unsetenv("CUTEPI_ANIMDEC_LIBAVIF")
	n := pull(t, path, func(i int, got []byte) {
		if i < len(ref) {
			if d := samePixels(got, ref[i], 320); d != "" {
				t.Errorf("frame %d: %s", i, d)
			}
		}
	})
	if n != len(ref) || n != 12 {
		t.Errorf("%d frames, libavif %d, want 12", n, len(ref))
	}
}
