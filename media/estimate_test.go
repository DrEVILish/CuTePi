package media

import (
	"encoding/binary"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaybackWarning(t *testing.T) {
	cases := []struct {
		name string
		v    MediaVideoInfo
		hz   int
		want string // substring; "" means no warning
	}{
		{"h264 1080p60 plays", MediaVideoInfo{Codec: "h264", Width: 1920, Height: 1080, FPS: 60, PixFmt: "yuv420p"}, 60, ""},
		{"h264 4k is software", MediaVideoInfo{Codec: "h264", Width: 3840, Height: 2160, FPS: 30, PixFmt: "yuv420p"}, 60, "software decode"},
		{"h264 10-bit 1080p60 short", MediaVideoInfo{Codec: "h264", Width: 1920, Height: 1080, FPS: 60, PixFmt: "yuv420p10le"}, 60, "about 51 fps"},
		{"h264 10-bit 1080p25 fine", MediaVideoInfo{Codec: "h264", Width: 1920, Height: 1080, FPS: 25, PixFmt: "yuv420p10le"}, 60, ""},
		{"hevc 4k60 plays", MediaVideoInfo{Codec: "hevc", Width: 3840, Height: 2160, FPS: 60}, 60, ""},
		{"hevc 8k does not", MediaVideoInfo{Codec: "hevc", Width: 7680, Height: 4320, FPS: 30}, 60, "hardware decode, about 15 fps"},
		{"prores 4444 alpha", MediaVideoInfo{Codec: "prores", Profile: "4444", Width: 1920, Height: 1080, FPS: 30, Alpha: true}, 60, "about 27 fps"},
		{"prores 422 hq unmeasured limit", MediaVideoInfo{Codec: "prores", Profile: "HQ", Width: 1920, Height: 1080, FPS: 60}, 60, ""},
		{"cfhd 422 at 50", MediaVideoInfo{Codec: "cfhd", Width: 1920, Height: 1080, FPS: 60, PixFmt: "yuv422p10le"}, 60, "about 50 fps"},
		{"cfhd 422 at 50 for 50 Hz", MediaVideoInfo{Codec: "cfhd", Width: 1920, Height: 1080, FPS: 50, PixFmt: "yuv422p10le"}, 50, ""},
		{"vp9 alpha", MediaVideoInfo{Codec: "vp9", Width: 1920, Height: 1080, FPS: 60, Alpha: true}, 60, "about 26 fps"},
		{"unknown codec says nothing", MediaVideoInfo{Codec: "rawvideo", Width: 7680, Height: 4320, FPS: 60}, 60, ""},
		{"720p cfhd alpha fits", MediaVideoInfo{Codec: "cfhd", Width: 1280, Height: 720, FPS: 60, Alpha: true}, 60, ""},
	}
	for _, c := range cases {
		got := playbackWarning(&c.v, c.hz)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The A/V sync signal's arithmetic: frame offsets from the sync point and the
// EBU R37 zones.
func TestAVSyncLayout(t *testing.T) {
	l := newAVLayout(1920, 1080, 60)
	if l.n != 120 || l.offset(1) != 1 || l.offset(119) != -1 || l.offset(60) != 60 {
		t.Fatalf("offsets: n=%d, %d %d %d", l.n, l.offset(1), l.offset(119), l.offset(60))
	}
	// At 60 Hz the meter (+-80 ms) holds the sync frame and four either side.
	for f, want := range map[int]bool{0: true, 4: true, 5: false, 60: false, 115: false, 116: true, 119: true} {
		if l.onMeter(f) != want {
			t.Errorf("onMeter(%d) = %v, want %v", f, !want, want)
		}
	}
	if x := l.meterX(0); x != (avMeterX0+avMeterX1)/2 || l.meterX(-AVSyncMeterMs) != avMeterX0 || l.meterX(AVSyncMeterMs) != avMeterX1 {
		t.Errorf("meterX: 0 at %v", x)
	}
	// Sound up to 40 ms early or 60 ms late is inside EBU R37 (not red); the
	// inner half of that is green.
	for ms, want := range map[float64]avRGB{-41: avRed, -40: avAmber, -21: avAmber, -20: avGreen, 0: avGreen, 30: avGreen, 31: avAmber, 60: avAmber, 61: avRed} {
		if got := avZone(ms); got != want {
			t.Errorf("avZone(%v) = %v, want %v", ms, got, want)
		}
	}
	// Markers: green on the sync frames, blue for the first half, white on
	// the half-cycle tick frames.
	for f, want := range map[int][3]bool{0: {true, true, false}, 1: {true, true, false}, 2: {false, true, false}, 59: {false, true, false},
		60: {false, false, true}, 61: {false, false, true}, 62: {}, 119: {}} {
		if got := l.marks(f); got != want {
			t.Errorf("marks(%d) = %v, want %v", f, got, want)
		}
	}
}

// The A/V sync clip, decoded: every frame's flash, corner markers and meter
// cursor are where the frame index says, and the tones start on the exact
// sample of the sync point and the half cycle.
func TestGenerateAVSync(t *testing.T) {
	if testing.Short() {
		t.Skip("generates and decodes a 20 s clip")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	const w, h, hz = 640, 360, 50
	path := filepath.Join(t.TempDir(), "avsync.mkv")
	if err := GenerateAVSync(path, w, h, hz); err != nil {
		t.Fatal(err)
	}
	l := newAVLayout(w, h, hz)

	cmd := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "rawvideo", "-pix_fmt", "rgb24", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, w*h*3)
	at := func(x, y int) (r, g, b int) { p := frame[(y*w+x)*3:]; return int(p[0]), int(p[1]), int(p[2]) }
	frames := 0
	for ; ; frames++ {
		if _, err := io.ReadFull(out, frame); err != nil {
			break
		}
		f := frames % l.n
		// The background is white on the sync frames only.
		if r, g, b := at(l.px(avProbeX, avProbeY)); (min(r, g, b) > 200) != l.sync(f) || (!l.sync(f) && max(r, g, b) > 100) {
			t.Fatalf("frame %d: background %d,%d,%d, flash wanted %v", frames, r, g, b, l.sync(f))
		}
		// Corner markers, at their centres.
		for i, want := range l.marks(f) {
			r, g, b := at(l.px(avMarks[i].x, avMarks[i].y))
			if lit := max(r, g, b) > 180; lit != want || (!lit && max(r, g, b) > 60) {
				t.Fatalf("frame %d: marker %d is %d,%d,%d, lit wanted %v", frames, i, r, g, b, want)
			}
		}
		// The meter cursor's pointer is the only bright thing in its lane.
		x0, y := l.px(avMeterX0-15, avMeterLaneY)
		x1, _ := l.px(avMeterX1+15, avMeterLaneY)
		sum, n := 0.0, 0.0
		for x := x0; x <= x1; x++ {
			if r, g, b := at(x, y); min(r, g, b) > 110 {
				sum, n = sum+float64(x)+0.5, n+1
			}
		}
		if (n > 0) != l.onMeter(f) {
			t.Fatalf("frame %d: meter cursor shown=%v, want %v", frames, n > 0, l.onMeter(f))
		}
		if want := l.ox + l.meterX(l.offsetMs(f))*l.u; n > 0 && math.Abs(sum/n-want) > 1 {
			t.Fatalf("frame %d (%+.0f ms): meter cursor at x=%.1f, want %.1f", frames, l.offsetMs(f), sum/n, want)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if frames != AVSyncSeconds*hz {
		t.Fatalf("got %d frames, want %d", frames, AVSyncSeconds*hz)
	}

	// Audio: both channels the same; 1 kHz from the sync point's sample and
	// 500 Hz from the half cycle's, two frames long, silence everywhere else.
	pcm, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	const cycle, tone = AVSyncCycle * avRate, AVSyncToneFrames * avRate / hz
	if len(pcm) != AVSyncSeconds*avRate*4 {
		t.Fatalf("got %d audio samples, want %d", len(pcm)/4, AVSyncSeconds*avRate)
	}
	for s := 0; s < len(pcm)/4; s++ {
		left, right := int16(binary.LittleEndian.Uint16(pcm[4*s:])), int16(binary.LittleEndian.Uint16(pcm[4*s+2:]))
		if left != right {
			t.Fatalf("sample %d: left %d, right %d", s, left, right)
		}
		want := 0.0 // both tones start at phase 0, so their first sample is 0 too
		switch m := s % cycle; {
		case m < tone:
			want = 0.5 * math.Sin(2*math.Pi*1000*float64(m)/avRate)
		case m >= cycle/2 && m < cycle/2+tone:
			want = 0.25 * math.Sin(2*math.Pi*500*float64(m-cycle/2)/avRate)
		}
		if math.Abs(float64(left)-want*32767) > 2 {
			t.Fatalf("sample %d (%d into its cycle): %d, want %.0f", s, s%cycle, left, want*32767)
		}
	}
}
