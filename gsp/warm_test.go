package gsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"CuTePi/config"

	"github.com/go-gst/go-gst/gst"
)

// Every test here needs real GStreamer (the warm slot IS a pipeline).
func gstAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping warm-slot test")
	}
}

func warmFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Skipf("no writable media fixture dir: %v", err)
	}
	return name
}

func slotState() (string, LoadOpts, *gst.Pipeline) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.warmFile, mgr.warmOpts, mgr.warm
}

// The core deck-style path: Warm arms the slot, LoadWithOpts with the same
// file+opts activates it (slot cleared, generation bumped, cue playing) and
// never rebuilds. A mismatched opts must fall back to the cold build while
// dropping the slot.
func TestWarmSlotActivateAndMismatch(t *testing.T) {
	gstAvailable(t)
	Panic() // isolate from the previous test's leftover playback
	file := warmFixture(t, "warm-a.wav", tinyWav(1))

	if err := Warm(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	gotFile, gotOpts, warm := slotState()
	if gotFile != file || !gotOpts.WarmPreroll {
		t.Fatalf("warm slot = (%q, %+v), want (%q, WarmPreroll)", gotFile, gotOpts, file)
	}
	if warm == nil {
		t.Fatalf("Warm left no pipeline in the slot")
	}

	gen := Generation()
	if err := LoadWithOpts(file, LoadOpts{WarmPreroll: false}); err != nil {
		t.Fatalf("LoadWithOpts mismatch: %v", err)
	}
	if wf, _, _ := slotState(); wf != "" {
		t.Errorf("mismatched LoadWithOpts kept the warm slot %q; it must drop it", wf)
	}
	if Generation() <= gen {
		t.Errorf("cold fallback did not bump the generation")
	}
	time.Sleep(300 * time.Millisecond)
	if got := CurrentPlaying(); got != file {
		t.Errorf("after mismatch fallback CurrentPlaying = %q, want %q", got, file)
	}

	// Matching opts again: the equal LoadWithOpts must activate the slot
	// itself (no fallback rebuild), leaving an empty slot and live playback.
	if err := Warm(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("Warm #2: %v", err)
	}
	gen2 := Generation()
	if err := LoadWithOpts(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("LoadWithOpts warm path: %v", err)
	}
	if wf, wo, _ := slotState(); wf != "" || wo != (LoadOpts{}) {
		t.Errorf("warm activation did not clear the slot: (%q, %+v)", wf, wo)
	}
	if Generation() <= gen2 {
		t.Errorf("warm activation did not bump the generation")
	}
	time.Sleep(300 * time.Millisecond)
	if got := CurrentPlaying(); got != file {
		t.Errorf("after warm activation CurrentPlaying = %q, want %q", got, file)
	}
}

// Fire with nothing armed must be a harmless no-op that touches no playback.
func TestInstallWarmWithoutSlot(t *testing.T) {
	gstAvailable(t)
	Panic() // isolate from the previous test's leftover playback
	if InstallWarm("never-armed.wav", LoadOpts{WarmPreroll: true}) {
		t.Fatalf("InstallWarm with no slot armed = true, want false")
	}
	if got := CurrentPlaying(); got != "" {
		t.Errorf("failed InstallWarm must not touch playback, CurrentPlaying = %q", got)
	}
}

// Warm replaces the previous arm (retiring its pipeline) — exactly one slot
// survives — and Panic empties the slot: a deck cue must never outlive the
// transport decision that retired it.
func TestWarmSlotReplacementAndPanic(t *testing.T) {
	gstAvailable(t)
	Panic() // isolate from the previous test's leftover playback
	file := warmFixture(t, "warm-x.wav", tinyWav(1))

	if err := Warm(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("Warm x: %v", err)
	}
	_, _, first := slotState()
	if err := Warm(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("Warm y: %v", err)
	}
	_, _, second := slotState()
	if second == nil || second == first {
		t.Fatalf("second Warm must install a FRESH pipeline in the slot")
	}

	Panic()
	if f, _, w := slotState(); f != "" || w != nil {
		t.Errorf("Panic left the warm slot (%q, %+v); it must be dropped", f, w)
	}
}

// Audio-only media has no video branch: the relink helper is a no-op that
// reports success, and the slot still activates into live playback.
func TestWarmWireVideoAudioSlotNoop(t *testing.T) {
	gstAvailable(t)
	Panic() // isolate from the previous test's leftover playback
	file := warmFixture(t, "warm-noop.wav", tinyWav(1))

	if err := Warm(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	mgr.mu.Lock()
	warm := mgr.warm
	mgr.mu.Unlock()
	if warm == nil {
		t.Fatalf("no slot armed")
	}
	if !warmWireVideo(warm) {
		t.Errorf("warmWireVideo on an audio-only slot = false, want true (no-op)")
	}
	gen := Generation()
	if err := LoadWithOpts(file, LoadOpts{WarmPreroll: true}); err != nil {
		t.Fatalf("LoadWithOpts after noop relink: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := CurrentPlaying(); got != file {
		t.Errorf("CurrentPlaying = %q, want %q", got, file)
	}
	if Generation() <= gen {
		t.Errorf("activation did not bump the generation")
	}
}

// The actual video-warm path: a real H.264 file armed with WarmPreroll
// prerolls onto the named fakesink (warm-video-sink), and the activation
// relink swaps the wall sink in and plays — the first frame must reach the
// real autovideosink without the pipeline erroring. This is the test path
// for the realtime-rule fix (warm preroll paints NOTHING).
func TestWarmVideoPrewarmAndRelink(t *testing.T) {
	gstAvailable(t)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build a video fixture")
	}
	dir := t.TempDir()
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	out := filepath.Join(dir, "warm-video.mp4")
	// Video-only MJPEG: this CI box has no audio device (audio branch
	// fails blocking preroll for env reasons) and no H.264 decoder plugin,
	// so MJPEG video covers the fakesink preroll + relink path everywhere.
	if err := runCmd("ffmpeg", "-v", "error",
		"-f", "lavfi", "-i", "color=c=gray:size=64x64:rate=10:duration=2",
		"-c:v", "mjpeg",
		"-c:a", "aac", out); err != nil {
		t.Skipf("ffmpeg fixture failed: %v", err)
	}

	t.Setenv("CUTEPI_WALL_SINK", "fakesink") // headless env: no display sink
	opts := LoadOpts{WarmPreroll: true}
	if err := Warm("warm-video.mp4", opts); err != nil {
		t.Fatalf("Warm video: %v", err)
	}
	mgr.mu.Lock()
	warm := mgr.warm
	mgr.mu.Unlock()
	if warm == nil {
		t.Fatalf("no slot armed after video Warm")
	}
	// Preroll onto the FAKE sink: the named anchors must exist, the fake
	// sink element must be present, and nothing may paint (assert the
	// pipeline can reach PAUSED without a real wall sink).
	if !warmContains(warm, "warm-video-sink") || !warmContains(warm, "warm-vf-tail") {
		t.Fatalf("video warm build did not name its relink anchors (fakesink path)")
	}
	if !warmSettled(warm, 3*time.Second) {
		t.Errorf("video warm pipeline never prerolled to PAUSED on fakesink")
	}

	// Activation: relink autovideosink, play, live output holds the file.
	if err := LoadWithOpts("warm-video.mp4", opts); err != nil {
		t.Fatalf("LoadWithOpts warm video: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if got := CurrentPlaying(); got != "warm-video.mp4" {
		t.Errorf("CurrentPlaying = %q, want warm-video.mp4", got)
	}
	mgr.mu.Lock()
	p, warmAfter := mgr.pipeline, mgr.warm
	mgr.mu.Unlock()
	if warmAfter != nil {
		t.Errorf("slot not cleared by activation: file %q", mgr.warmFile)
	}
	if p != nil && warmContains(p, "warm-video-sink") {
		t.Errorf("active pipeline still contains the fake sink — relink did not run")
	}
}

// warmContains reports whether the pipeline holds an element by name.
func warmContains(p *gst.Pipeline, name string) bool {
	e, err := p.GetElementByName(name)
	return err == nil && e != nil
}

// warmSettled waits for the pipeline to reach the wanted (prerolled) state.
func warmSettled(p *gst.Pipeline, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, state := p.GetState(gst.StateNull, gst.ClockTimeNone); state == gst.StatePaused || state == gst.StatePlaying {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func runCmd(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// LoadOpts comparability is load-bearing: warm activation compares the
// whole opts struct. A pointer/list field would silently break equality.
func TestWarmOptsEquality(t *testing.T) {
	if (LoadOpts{Volume: 6} != LoadOpts{Volume: 6}) {
		t.Fatalf("LoadOpts lost comparability — warm equality relies on it")
	}
	if (LoadOpts{Volume: 6} == LoadOpts{Volume: 6, WarmPreroll: true}) {
		t.Errorf("WarmPreroll does not participate in opts equality")
	}
	if (LoadOpts{WarmPreroll: true} == LoadOpts{}) {
		t.Errorf("WarmPreroll zero-participation: true opts equals empty opts")
	}
}
