package gsp

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"CuTePi/config"
)

// voicesFixture writes a solid PNG per name into a fresh media dir.
func voicesFixture(t *testing.T, names ...string) {
	t.Helper()
	gstAvailable(t)
	quiescePlayback(t)
	dir := t.TempDir()
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	for i, n := range names {
		img := image.NewRGBA(image.Rect(0, 0, 320, 240))
		for p := range img.Pix {
			img.Pix[p] = byte(40 * (i + 1))
		}
		img.Set(0, 0, color.RGBA{255, 0, 0, 255})
		var b bytes.Buffer
		png.Encode(&b, img)
		if err := os.WriteFile(filepath.Join(dir, n), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { Stop() })
}

func fire(t *testing.T, file string, opts LoadOpts) {
	t.Helper()
	if err := LoadWithOpts(file, opts); err != nil {
		t.Fatalf("load %s: %v", file, err)
	}
	Play()
}

func running() []int {
	r := RunningCues()
	sort.Ints(r)
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A cue that keeps the others running joins the stack and becomes the focus;
// stopping one cue alone leaves the rest, and the focus passes to the newest
// remaining cue.
func TestVoicesStackFocusAndStopOne(t *testing.T) {
	voicesFixture(t, "va.png", "vb.png", "vc.png")
	fire(t, "va.png", LoadOpts{CuePos: 1, Hold: true})
	fire(t, "vb.png", LoadOpts{CuePos: 2, Hold: true, KeepOthers: true, Layer: LayerTop})
	fire(t, "vc.png", LoadOpts{CuePos: 3, Hold: true, KeepOthers: true, Layer: LayerBottom})
	if got := running(); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("running = %v, want [1 2 3]", got)
	}
	if CurrentCuePos() != 3 {
		t.Fatalf("focus = cue %d, want the newest (3)", CurrentCuePos())
	}
	if vs := Voices(); len(vs) != 3 {
		t.Fatalf("Voices() = %+v, want 3", vs)
	}
	if !StopCue(3, 0) || CurrentCuePos() != 2 {
		t.Fatalf("after stopping cue 3 alone: focus %d, running %v; want focus 2", CurrentCuePos(), running())
	}
	if !StopCue(1, 0) || CurrentCuePos() != 2 || len(running()) != 1 {
		t.Fatalf("after stopping cue 1 (a lower layer): focus %d, running %v", CurrentCuePos(), running())
	}
	if StopCue(1, 0) {
		t.Fatal("StopCue on a cue that is not running reported true")
	}
}

// Without KeepOthers a fire stops every running cue, as before (§6.5).
func TestVoicesStopOthersByDefault(t *testing.T) {
	voicesFixture(t, "sa.png", "sb.png", "sc.png")
	fire(t, "sa.png", LoadOpts{CuePos: 1, Hold: true})
	fire(t, "sb.png", LoadOpts{CuePos: 2, Hold: true, KeepOthers: true})
	fire(t, "sc.png", LoadOpts{CuePos: 3, Hold: true})
	if got := running(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("running = %v, want only [3]", got)
	}
}

// Pause and Play reach every running cue; Stop ends them all.
func TestVoicesTransportActsOnAll(t *testing.T) {
	voicesFixture(t, "pa.png", "pb.png")
	fire(t, "pa.png", LoadOpts{CuePos: 1, Loop: true})
	fire(t, "pb.png", LoadOpts{CuePos: 2, Loop: true, KeepOthers: true})
	Pause()
	for _, v := range Voices() {
		if !v.Paused {
			t.Fatalf("after Pause, cue %d not paused: %+v", v.CuePos, Voices())
		}
	}
	Play()
	waitFor(t, "every cue resumed", func() bool {
		for _, v := range Voices() {
			if v.Paused {
				return false
			}
		}
		return true
	})
	Stop()
	if got := running(); len(got) != 0 {
		t.Fatalf("after Stop, running = %v", got)
	}
	// Stop keeps the focus pipeline for a resume: not a running cue.
	if vs := Voices(); len(vs) != 0 {
		t.Fatalf("after Stop, Voices() = %+v", vs)
	}
}

// A lower-layer cue that ends by itself leaves the stack and reports its end
// (auto-continue); the focus stays. When the focus ends, the cue below takes
// over.
func TestVoicesEndOnTheirOwn(t *testing.T) {
	voicesFixture(t, "hold.png", "top.png")
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available for the clip fixture")
	}
	dir := config.MediaLocation()
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=d=1:s=320x240:r=25",
		"-c:v", "mpeg1video", filepath.Join(dir, "short.mpg")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	var mu sync.Mutex
	var ended []int
	SetCueEndHook(func(pos int) { mu.Lock(); ended = append(ended, pos); mu.Unlock() })
	t.Cleanup(func() { SetCueEndHook(nil) })

	// The 1 s clip runs underneath; a held still fired on top keeps it.
	fire(t, "short.mpg", LoadOpts{CuePos: 1})
	fire(t, "top.png", LoadOpts{CuePos: 2, Hold: true, KeepOthers: true, Layer: LayerTop})
	waitFor(t, "the lower clip's end", func() bool { mu.Lock(); defer mu.Unlock(); return len(ended) > 0 })
	mu.Lock()
	gotEnded := append([]int(nil), ended...)
	mu.Unlock()
	if gotEnded[0] != 1 || CurrentCuePos() != 2 || len(running()) != 1 {
		t.Fatalf("ended %v, focus %d, running %v; want cue 1 ended, cue 2 still the focus", gotEnded, CurrentCuePos(), running())
	}

	// The focus ends while a held still runs underneath: the still takes over.
	fire(t, "hold.png", LoadOpts{CuePos: 3, Hold: true})
	fire(t, "short.mpg", LoadOpts{CuePos: 4, KeepOthers: true})
	waitFor(t, "the focus clip's end", func() bool { return CurrentCuePos() == 3 })
	if got := running(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("after the focus ended: running %v, want [3]", got)
	}
}

// Placement in a bottom-to-top stack.
func TestStackIndex(t *testing.T) {
	st := []string{"a", "b", "c"}
	if i := stackIndex(st, "", LayerTop); i != 3 {
		t.Fatalf("top = %d", i)
	}
	if i := stackIndex(st, "", LayerBottom); i != 0 {
		t.Fatalf("bottom = %d", i)
	}
	if i := stackIndex(st, "b", LayerUnder); i != 1 {
		t.Fatalf("under b = %d", i)
	}
	if i := stackIndex(st, "zz", LayerUnder); i != 3 {
		t.Fatalf("under a layer not in the stack = %d, want top", i)
	}
	if got := insertLayer(append([]string(nil), st...), "x", 1); len(got) != 4 || got[1] != "x" || got[2] != "b" {
		t.Fatalf("insert = %v", got)
	}
}

// Two running cues with sound are both heard: each feeds the audio bus (one
// mixer on the device), where two device sinks could not open at once. When
// they stop, the bus lets the device go.
func TestVoicesShareTheAudioDevice(t *testing.T) {
	voicesFixture(t)
	a := warmFixture(t, "aa.wav", tinyWav(3))
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "ab.wav"), tinyWav(3), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { busIdle = d }(busIdle)
	busIdle = 300 * time.Millisecond
	fire(t, a, LoadOpts{CuePos: 1})
	fire(t, "ab.wav", LoadOpts{CuePos: 2, KeepOthers: true})
	if got := running(); len(got) != 2 {
		t.Fatalf("running = %v, want both cues", got)
	}
	waitFor(t, "both cues on the bus", func() bool { return AudioBusInputs() == 2 })
	Stop()
	if n := AudioBusInputs(); n != 0 {
		t.Fatalf("bus inputs after Stop = %d", n)
	}
	waitFor(t, "the bus to release the device", func() bool {
		abus.mu.Lock()
		defer abus.mu.Unlock()
		return abus.p == nil
	})
}
