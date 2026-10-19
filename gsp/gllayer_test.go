package gsp

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/gsp/bobdeint"
	"CuTePi/gsp/glwall"
	"CuTePi/gsp/sysdmabuf"
)

// A point on the picture, centred, y down: where each operation sends it.
type pt struct{ x, y int }

var dirMap = map[videoDir]func(pt) pt{
	dirIdentity: func(p pt) pt { return p },
	dir90R:      func(p pt) pt { return pt{-p.y, p.x} }, // clockwise
	dir180:      func(p pt) pt { return pt{-p.x, -p.y} },
	dir90L:      func(p pt) pt { return pt{p.y, -p.x} },
	dirHoriz:    func(p pt) pt { return pt{-p.x, p.y} },
	dirVert:     func(p pt) pt { return pt{p.x, -p.y} },
	dirULLR:     func(p pt) pt { return pt{p.y, p.x} },
	dirURLL:     func(p pt) pt { return pt{-p.y, -p.x} },
}

// TestGLDirection: rotate (clockwise), then mirror, as the plane wall does,
// must equal the single videoflip direction chosen.
func TestGLDirection(t *testing.T) {
	rot := map[int]videoDir{0: dirIdentity, 90: dir90R, 180: dir180, 270: dir90L}
	mir := map[string]videoDir{"none": dirIdentity, "": dirIdentity, "h": dirHoriz, "v": dirVert}
	probe := []pt{{1, 0}, {0, 1}, {2, 3}}
	for r, rd := range rot {
		for f, fd := range mir {
			got := glDirection(r, f)
			for _, p := range probe {
				want := dirMap[fd](dirMap[rd](p))
				if dirMap[got](p) != want {
					t.Errorf("rotation %d mirror %q: direction %d sends %v to %v, want %v", r, f, got, p, dirMap[got](p), want)
				}
			}
		}
	}
}

// TestGLCropEdges: a crop on a source edge must land on the edge that source
// edge is moved to by the flip.
func TestGLCropEdges(t *testing.T) {
	// Outward normal of each edge: left, right, top, bottom.
	normals := []pt{{-1, 0}, {1, 0}, {0, -1}, {0, 1}}
	for d, f := range dirMap {
		for src := 0; src < 4; src++ {
			in := [4]int{}
			in[src] = 7
			l, r, tp, b := glCropEdges(d, in[0], in[1], in[2], in[3])
			out := [4]int{l, r, tp, b}
			moved := f(normals[src])
			for dst := 0; dst < 4; dst++ {
				want := 0
				if normals[dst] == moved {
					want = 7
				}
				if out[dst] != want {
					t.Errorf("direction %d: crop on source edge %d gave %v", d, src, out)
					break
				}
			}
		}
	}
}

// Angles outside 0..359 and unknown mirror values must not fall through to
// "no turn" by accident: -90 is 270, 450 is 90, 360 is none; an unknown
// mirror is no mirror.
func TestGLDirectionNormalisesInput(t *testing.T) {
	cases := []struct {
		rot  int
		flip string
		want videoDir
	}{
		{-90, "none", dir90L}, {450, "", dir90R}, {360, "none", dirIdentity}, {-180, "h", dirVert},
		{90, "x", dir90R}, {0, "x", dirIdentity},
	}
	for _, c := range cases {
		if got := glDirection(c.rot, c.flip); got != c.want {
			t.Errorf("glDirection(%d, %q) = %d, want %d", c.rot, c.flip, got, c.want)
		}
	}
}

// glVideoTail's element order is relied on by index: configureGLTail and
// glConfigureFlip set capsfilters[0] and [1], the last v4l2convert feeds the
// wall. Every tail starts with a queue and ends in the appsink; what reaches
// the appsink is reported by glRoute.
func TestGLVideoTailShapes(t *testing.T) {
	gst.Init(nil)            // videoDownload probes for v4l2convert once and caches the answer
	dl := videoDownload()    // v4l2convert when present
	hwTurnedRoute := "alpha" // ISP scale, CPU turn, RGBA for the wall's copy route
	if len(dl) == 0 {
		hwTurnedRoute = "isp" // no ISP: the software path
	}
	swRoute, swCaps := "isp", 2 // I420 and NV12: system dma-heap buffers where the heap opens
	if sysdmabuf.Register() {
		swRoute, swCaps = "heap", 1
	}
	cases := []struct {
		name                string
		dmabuf, flip, hw    bool
		format              string
		route               string
		capsfilters         int
		wantIdentity, wantF bool
	}{
		{"hardware decoder", true, false, true, "", "dmabuf", 1, false, false},
		{"alpha", false, false, false, "RGBA", "alpha", 1, false, false},
		{"alpha as ARGB", false, false, false, "ARGB", "alpha", 1, false, false},
		{"packed RGB (qtrle, BMP)", false, false, false, "RGB", "alpha", 1, false, false},
		{"packed BGRx", false, false, false, "BGRx", "alpha", 1, false, false},
		{"software I420", false, false, false, "I420", swRoute, swCaps, false, false},
		{"software 10-bit 4:2:2", false, false, false, "I422_10LE", swRoute, swCaps, false, false},
		{"software via ISP", false, false, false, "YUY2", "isp", 2, false, false},
		{"software turned", false, true, false, "I420", "isp", 2, false, true},
		{"alpha turned", false, true, false, "RGBA", "alpha", 1, false, true},
		{"hardware turned", false, true, true, "", hwTurnedRoute, 2, len(dl) > 0, true},
	}
	for _, c := range cases {
		names := glVideoTail(c.dmabuf, c.format, c.flip, c.hw)
		if names[0] != "queue" || names[len(names)-1] != "appsink" {
			t.Errorf("%s: tail %v must run queue .. appsink", c.name, names)
		}
		if got := glRoute(names); got != c.route {
			t.Errorf("%s: tail %v route %q, want %q", c.name, names, got, c.route)
		}
		n := 0
		for _, e := range names {
			if e == "capsfilter" {
				n++
			}
		}
		if n != c.capsfilters {
			t.Errorf("%s: tail %v has %d capsfilters, want %d", c.name, names, n, c.capsfilters)
		}
		if (indexOfName(names, "identity") >= 0) != c.wantIdentity {
			t.Errorf("%s: tail %v identity present = %v, want %v", c.name, names, !c.wantIdentity, c.wantIdentity)
		}
		vf, vc := indexOfName(names, "videoflip"), indexOfName(names, "videocrop")
		if (vf >= 0) != c.wantF {
			t.Errorf("%s: tail %v videoflip present = %v, want %v", c.name, names, vf >= 0, c.wantF)
		}
		if c.wantF && (vc < 0 || vc > vf) {
			t.Errorf("%s: tail %v must crop before it turns", c.name, names)
		}
		if c.route == "isp" && c.flip {
			// The ISP that feeds the wall comes after the turn.
			if last := len(names) - 1 - indexOfName(reverse(names), "v4l2convert"); last < vf {
				t.Errorf("%s: tail %v converts for the wall before turning", c.name, names)
			}
		}
	}
}

func reverse(s []string) []string {
	r := make([]string, len(s))
	for i, v := range s {
		r[len(s)-1-i] = v
	}
	return r
}

// glTailFormat: a fixed format wins; unfixed caps (a caps query listing
// what the decoder could produce) are ignored in favour of the import
// metadata; no lookup, or no filename, gives "".
func TestGLTailFormat(t *testing.T) {
	gst.Init(nil)
	saved := AlphaLookup
	defer func() { AlphaLookup = saved }()
	asked := ""
	AlphaLookup = func(name string) bool { asked = name; return name == "alpha.mov" }
	fixed := gst.NewCapsFromString("video/x-raw,format=NV12,width=1920,height=1080")
	unfixed := gst.NewCapsFromString("video/x-raw,format={RGB,RGBA}")
	if got := glTailFormat(fixed, "alpha.mov"); got != "NV12" {
		t.Errorf("fixed NV12 caps: %q, want NV12 (the decoder's actual format wins)", got)
	}
	if got := glTailFormat(unfixed, "alpha.mov"); got != "RGBA" {
		t.Errorf("unfixed caps, file with alpha: %q, want RGBA", got)
	}
	if got := glTailFormat(unfixed, "plain.mov"); got != "" {
		t.Errorf("unfixed caps, file without alpha: %q, want \"\" (RGB is only a possibility)", got)
	}
	// One format, size not known yet (cutepianimdec): that format.
	if got := glTailFormat(gst.NewCapsFromString("video/x-raw,format=RGBA,width=[1,32768],height=[1,32768]"), "anim.webp"); got != "RGBA" {
		t.Errorf("single-format caps: %q, want RGBA", got)
	}
	// A 24-bit RGB still keeps the ISP route ("").
	if got := glTailFormat(gst.NewCapsFromString("video/x-raw,format=RGB,width=64,height=48,framerate=0/1"), "still.png"); got != "" {
		t.Errorf("RGB still: %q, want \"\"", got)
	}
	asked = "-"
	if got := glTailFormat(nil, ""); got != "" || asked != "-" {
		t.Errorf("no caps, no filename: %q (lookup asked %q), want \"\" without a lookup", got, asked)
	}
	AlphaLookup = nil
	if got := glTailFormat(nil, "alpha.mov"); got != "" {
		t.Errorf("no lookup installed: %q, want \"\"", got)
	}
	// A packed RGB file (qtrle's rgb24) takes the copy route before its pad is fixed.
	savedCodec := CodecLookup
	defer func() { CodecLookup = savedCodec }()
	CodecLookup = func(name string) string {
		return map[string]string{"anim.mov": "qtrle/rgb24", "argb.mov": "qtrle/argb", "h264.mov": "h264/yuv420p",
			"prores.mov": "prores/yuv422p10le", "odd.mov": "ffv1/yuv444p"}[name]
	}
	// ... and a YUV file the layout the dma-heap tail is chosen by.
	for name, want := range map[string]string{"anim.mov": "RGB", "argb.mov": "ARGB", "h264.mov": "I420",
		"prores.mov": "I422_10LE", "odd.mov": "", "none.mov": ""} {
		if got := glTailFormat(unfixed, name); got != want {
			t.Errorf("unfixed caps, %s: %q, want %q", name, got, want)
		}
	}
}

// glISPCanTake / glTailDMABuf, decided from what the decoder's pad can
// produce (a capsfilter's src pad stands in for it).
func TestGLISPCanTake(t *testing.T) {
	gst.Init(nil)
	pad := func(caps string) *gst.Pad {
		cf, err := gst.NewElement("capsfilter")
		if err != nil {
			t.Fatal(err)
		}
		cf.Set("caps", gst.NewCapsFromString(caps))
		return cf.GetStaticPad("src")
	}
	h264 := pad("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=YU12")
	hevc := pad("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=NV12:0x0700000000000004")
	sw := pad("video/x-raw,format=I420")
	if !glISPCanTake(h264) {
		t.Error("H.264 YU12 DMABufs: the ISP can scale them")
	}
	if glISPCanTake(hevc) {
		t.Error("HEVC SAND (Broadcom modifier) DMABufs: v4l2convert has no mapping, must take the CPU path")
	}
	if glISPCanTake(sw) {
		t.Error("a software decoder is not a hardware decoder")
	}
	if !glTailDMABuf(h264, LoadOpts{}, false) {
		t.Error("an unturned hardware cue goes to the wall as the decoder's DMABufs")
	}
	if glTailDMABuf(h264, LoadOpts{Rotation: 90}, false) || glTailDMABuf(h264, LoadOpts{Flip: "h"}, false) {
		t.Error("a turned or mirrored hardware cue must not take the zero-copy route (the turn is on the CPU)")
	}
	if glTailDMABuf(h264, LoadOpts{}, true) {
		t.Error("test patterns never take the DMABuf route")
	}
}

func TestGLCurve(t *testing.T) {
	for name, want := range map[string]glwall.Curve{"": glwall.CurveLinear, "linear": glwall.CurveLinear,
		"smooth": glwall.CurveSmooth, "log": glwall.CurveLog, "exp": glwall.CurveExp, "bogus": glwall.CurveLinear} {
		if got := glCurve(name); got != want {
			t.Errorf("glCurve(%q) = %d, want %d", name, got, want)
		}
	}
}

// The Stop crash: glHide and glDrop both freed one layer. A layer can be
// taken from its record exactly once.
func TestGLTakeLayerOnce(t *testing.T) {
	l := &glLayer{layer: &glwall.Layer{}}
	if glTakeLayerLocked(l) == nil {
		t.Fatal("first take returned nil")
	}
	if glTakeLayerLocked(l) != nil {
		t.Error("second take returned the layer again: two callers would free it")
	}
	if glTakeLayerLocked(nil) != nil {
		t.Error("nil record must give nil")
	}
}

func TestRampLevel(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		from, to     float64
		elapsed, dur time.Duration
		curve        string
		want         float64
	}{
		{0, 1, 0, 0, "linear", 1},                // zero-length fade started now: done, not NaN
		{0.8, 0, 50 * ms, 0, "smooth", 0},        // zero-length fade-out
		{0, 1, -20 * ms, 1000 * ms, "linear", 0}, // not started yet
		{0, 1, 500 * ms, 1000 * ms, "linear", 0.5},
		{0.6, 0, 500 * ms, 1000 * ms, "linear", 0.3},
		{0, 1, 2000 * ms, 1000 * ms, "exp", 1}, // past the end
		{0, 1, 500 * ms, 1000 * ms, "smooth", 0.5},
	}
	for _, c := range cases {
		got := rampLevel(c.from, c.to, c.elapsed, c.dur, c.curve)
		if math.IsNaN(got) || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("rampLevel(%v, %v, %v, %v, %q) = %v, want %v", c.from, c.to, c.elapsed, c.dur, c.curve, got, c.want)
		}
	}
}

func TestFadeLandAndAudioSyncOffWall(t *testing.T) {
	saved := glOpen
	defer func() { glOpen = saved }()
	glOpen = false
	if fadeLand() != alphaLand {
		t.Errorf("plane wall: fadeLand %v, want %v", fadeLand(), alphaLand)
	}
	if d, a := GLAudioSync(); d != 0 || a != 0 {
		t.Errorf("GL wall off: GLAudioSync = %v, %v, want 0, 0", d, a)
	}
	glOpen = true
	if fadeLand() != alphaLand+glWallDelay {
		t.Errorf("GL wall: fadeLand %v, want %v (the picture is shown ~100 ms after its time)", fadeLand(), alphaLand+glWallDelay)
	}
}

// An interlaced cue's plain software tail becomes videoconvert → cutepibob
// (DMABufs out) → queue → appsink, without the ISP; the software turning
// tail and the alpha and packed-RGB tails get videoconvert, cutepibob and a
// queue before their first videoconvert; the turned hardware tail stays
// woven; progressive tails are left alone, and an interlaced file leaves the
// DMABuf route.
func TestGLDeinterlace(t *testing.T) {
	gst.Init(nil)
	sw := glVideoTail(false, "I420", false, false)
	got := glDeinterlace(sw, true)
	di, vc := indexOfName(got, "cutepibob"), indexOfName(got, "videoconvert")
	if di < 0 || vc != di-1 || indexOfName(got, "v4l2convert") >= 0 ||
		strings.Join(got[di:], " ") != "cutepibob queue capsfilter appsink" {
		t.Errorf("interlaced software tail %v: want videoconvert cutepibob queue capsfilter appsink, no ISP", got)
	}
	if p := indexOfName(got, "cutepiyuvpack"); p >= 0 && p > di {
		t.Errorf("deinterlacer %d before the packer %d in %v", di, p, got)
	}
	if r := glRoute(got); r != "bob" {
		t.Errorf("route %q, want bob", r)
	}
	for _, f := range []string{"RGBA", "BGRx", "A420"} {
		got := glDeinterlace(glVideoTail(false, f, false, false), true)
		if strings.Join(got, " ") != "queue videoconvert cutepibob queue videoconvert capsfilter appsink" {
			t.Errorf("%s tail %v: want videoconvert, cutepibob, queue ahead of the tail's videoconvert", f, got)
		}
	}
	flip := glDeinterlace(glVideoTail(false, "I420", true, false), true)
	di, vc = indexOfName(flip, "cutepibob"), indexOfName(flip, "videoconvert")
	if di < 1 || vc != di-1 || flip[di+1] != "queue" || flip[di+2] != "videoconvert" {
		t.Errorf("interlaced turning tail %v: want videoconvert, cutepibob, queue, videoconvert", flip)
	}
	if alpha := glDeinterlace(glVideoTail(false, "RGBA", true, false), true); indexOfName(alpha, "cutepibob") < 0 {
		t.Errorf("turning RGBA tail %v: no deinterlacer", alpha)
	}
	if tail := glVideoTail(false, "I420", true, true); indexOfName(tail, "identity") >= 0 {
		if got := glDeinterlace(tail, true); strings.Join(got, " ") != strings.Join(tail, " ") {
			t.Errorf("turned hardware tail %v: want it left woven, got %v", tail, got)
		}
	}
	if got := glDeinterlace(sw, false); indexOfName(got, "cutepibob") >= 0 {
		t.Errorf("progressive tail got a deinterlacer: %v", got)
	}
	saved := FieldOrderLookup
	defer func() { FieldOrderLookup = saved }()
	FieldOrderLookup = func(name string) string {
		if name == "i.mov" {
			return "tt"
		}
		return ""
	}
	if !glInterlaced(nil, "i.mov") || glInterlaced(nil, "p.mov") {
		t.Errorf("metadata lookup not used")
	}
	if !glInterlaced(gst.NewCapsFromString("video/x-raw,interlace-mode=interleaved"), "p.mov") {
		t.Errorf("interleaved caps not seen as interlaced")
	}
	if glInterlaced(gst.NewCapsFromString("video/x-raw,interlace-mode=progressive"), "p.mov") {
		t.Errorf("progressive caps seen as interlaced")
	}
}

// The deinterlacer's field order comes from the import metadata (second
// letter of ffprobe's field order), else the caps' field-order, else top first.
func TestConfigureDeinterlace(t *testing.T) {
	gst.Init(nil)
	if !bobdeint.Register() {
		t.Skip("cutepibob not registered")
	}
	saved := FieldOrderLookup
	defer func() { FieldOrderLookup = saved }()
	FieldOrderLookup = func(name string) string { return map[string]string{"t.mov": "tt", "b.mov": "tb", "bt.mov": "bt"}[name] }
	bff := gst.NewCapsFromString("video/x-raw,interlace-mode=interleaved,field-order=bottom-field-first")
	for _, c := range []struct {
		file string
		caps *gst.Caps
		tff  bool
	}{{"t.mov", nil, true}, {"b.mov", nil, false}, {"bt.mov", nil, true}, {"p.mov", bff, false}, {"p.mov", nil, true}} {
		f, err := gst.NewElement("cutepibob")
		if err != nil {
			t.Fatal(err)
		}
		configureDeinterlace(map[string][]*gst.Element{"cutepibob": {f}}, c.caps, c.file)
		if v, _ := f.GetProperty("tff"); v != c.tff {
			t.Errorf("%s (caps %v): tff %v, want %v", c.file, c.caps, v, c.tff)
		}
	}
}

// CUTEPI_GL_SWROUTE=isp keeps I420 on the ISP; formats the dma-heap tail
// does not copy (YUY2) always take the ISP.
func TestGLHeapRoute(t *testing.T) {
	gst.Init(nil)
	if !sysdmabuf.Register() {
		t.Skip("no system dma-heap")
	}
	if !glHeapRoute("I420") || !glHeapRoute("NV12") || glHeapRoute("YUY2") || glHeapRoute("") {
		t.Error("heap route: I420/NV12 only")
	}
	t.Setenv("CUTEPI_GL_SWROUTE", "isp")
	if glHeapRoute("I420") {
		t.Error("CUTEPI_GL_SWROUTE=isp must keep the ISP")
	}
}
