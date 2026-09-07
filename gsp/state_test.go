package gsp

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"CuTePi/config"
)

// dbToGain is the core audio conversion: 0dB -> 1.0 and -Inf/-60dB -> silence,
// with +6dB roughly doubling and -6dB halving (never exact: 10^(±6/20)).
// False here means loudness is wrong.
func TestDBToGain(t *testing.T) {
	cases := []struct {
		db   float64
		want float64
	}{
		{0, 1.0},
		{-60, 0},
		{-80, 0},
	}
	for _, c := range cases {
		if got, want := dbToGain(c.db), c.want; math.Abs(got-want) > 1e-9 {
			t.Errorf("dbToGain(%v) = %v, want %v", c.db, got, want)
		}
	}
	if got, want := dbToGain(6), math.Pow(10, 6.0/20); math.Abs(got-want) > 1e-9 {
		t.Errorf("dbToGain(6) = %v, want %v", got, want)
	}
	// +6dB and -6dB are reciprocal gains.
	if got, want := dbToGain(-6), 1.0/dbToGain(6); math.Abs(got-want) > 1e-9 {
		t.Errorf("dbToGain(-6) should be the inverse of dbToGain(6), got %v (want %v)", got, want)
	}
	// Clamp keeps within the slider's range, and -inf → silence.
	if dbToGain(math.Inf(-1)) != 0 {
		t.Errorf("dbToGain(-Inf) should be 0 (silence), got %v", dbToGain(math.Inf(-1)))
	}
	if got := dbClamp(100); got != 12 {
		t.Errorf("dbClamp(100) = %v, want 12", got)
	}
	if got := dbClamp(-100); got != -60 {
		t.Errorf("dbClamp(-100) = %v, want -60", got)
	}
}

// The loop-count semantics: 0 is infinite (never exhausted), a finite N plays
// the clip N times (restart while a pass remains, stop once the count hits 0).
// This is the decision logic handleEnd applies to a looping clip at EOS.
func TestLoopSteps(t *testing.T) {
	cases := []struct {
		remaining int
		restart   bool
		next      int
	}{
		{0, true, 0},   // infinite: always restart
		{1, false, 0},  // single pass finished
		{2, true, 1},   // one of two passes done
		{3, true, 2},   // one of three passes done
		{4, true, 3},   // one of four passes done
		{-1, false, 0}, // defensive: a spent counter ends
	}
	for _, c := range cases {
		restart, next := loopSteps(c.remaining)
		if restart != c.restart || next != c.next {
			t.Errorf("loopSteps(%d) = (%v, %d), want (%v, %d)", c.remaining, restart, next, c.restart, c.next)
		}
	}
	// A finite count of 3 must play 3 times then stop: consume the passes.
	rem := 3
	for i := 0; i < 2; i++ {
		restart, next := loopSteps(rem)
		if !restart {
			t.Fatalf("loopSteps(%d) should restart, pass %d", rem, i)
		}
		rem = next
	}
	if restart, next := loopSteps(rem); restart || next != 0 {
		t.Fatalf("final pass of 3 should end, got restart=%v next=%d", restart, next)
	}
}

// The change counter the Now Playing poller depends on: it must bump on real
// playback state changes (pipeline swap, play, pause, stop, panic) and stay
// stable while idle (no pipeline), so the client only re-renders when the
// widget's content can actually have changed. Requires real GStreamer, so it
// is gated the same way as the runtime smoke test. The package is a singleton
// (shared with the runtime test), so assertions are relative to a snapshot
// taken at test start rather than absolute.
func TestStateVersionBumpsOnPlaybackOperations(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping state-version test")
	}

	v0 := StateVersion()
	if err := ShowTest("smpte"); err != nil {
		t.Fatalf("ShowTest: %v", err)
	}
	if v := StateVersion(); v <= v0 {
		t.Errorf("expected ShowTest (pipeline swap) to bump the version, v0=%d v1=%d", v0, v)
	}

	v2 := StateVersion()
	Play()
	if v := StateVersion(); v <= v2 {
		t.Errorf("expected Play to bump the version, v2=%d v3=%d", v2, v)
	}

	v3 := StateVersion()
	Pause()
	if v := StateVersion(); v <= v3 {
		t.Errorf("expected Pause to bump the version, v3=%d v4=%d", v3, v)
	}

	v4 := StateVersion()
	TogglePause()
	if v := StateVersion(); v <= v4 {
		t.Errorf("expected TogglePause to bump the version, v4=%d v5=%d", v4, v)
	}

	v5 := StateVersion()
	Stop()
	if v := StateVersion(); v <= v5 {
		t.Errorf("expected Stop to bump the version, v5=%d v6=%d", v5, v)
	}

	v6 := StateVersion()
	Panic()
	if v := StateVersion(); v <= v6 {
		t.Errorf("expected Panic to bump the version, v6=%d v7=%d", v6, v)
	}

	// Idle (no pipeline): position queries must not bump the version.
	v7 := StateVersion()
	CurrentPosition()
	CurrentPosition()
	if v := StateVersion(); v != v7 {
		t.Errorf("expected idle position queries to leave the version unchanged, v7=%d v8=%d", v7, v)
	}
}

// Stop must end the cue association along with playback: the auto-continue
// and slideshow runners treat "CurrentCuePos() still == my cue" as "the chain
// is still armed", so a cuePos surviving Stop made the next cue fire even
// after the operator pressed Stop. Same gating as the state-version test.
func TestStopClearsCueAssociation(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping stop-state test")
	}

	if err := ShowTest("smpte"); err != nil {
		t.Fatalf("ShowTest: %v", err)
	}
	SetCuePos(7)
	if got := CurrentCuePos(); got != 7 {
		t.Fatalf("SetCuePos(7) then CurrentCuePos() = %d, want 7", got)
	}

	// Generation must advance on pipeline swap (load) and on stop. (Whether a
	// no-pipeline Stop bumps can't be asserted here: a concurrent sink error
	// lands via clearIfCurrent and legitimately bumps.)
	genAfterLoad := Generation()
	Stop()
	if Generation() <= genAfterLoad {
		t.Errorf("Stop must bump the generation, %d -> %d", genAfterLoad, Generation())
	}
	genStopped := Generation()

	if err := ShowTest("smpte"); err != nil {
		t.Fatalf("ShowTest: %v", err)
	}
	genAfterReload := Generation()
	if genAfterReload <= genStopped {
		t.Errorf("pipeline swap must bump the generation, %d -> %d", genStopped, genAfterReload)
	}
	Panic()
	if Generation() <= genAfterReload {
		t.Errorf("Panic must bump the generation, %d -> %d", genAfterReload, Generation())
	}

	if got := CurrentCuePos(); got != 0 {
		t.Errorf("after Stop CurrentCuePos() = %d, want 0 (chain must be disarmed)", got)
	}
	if got := CurrentPlaying(); got != "" {
		t.Errorf("after Stop CurrentPlaying() = %q, want empty", got)
	}

	// Stopping with no pipeline must stay a harmless no-op.
	Stop()
	if got := CurrentCuePos(); got != 0 {
		t.Errorf("Stop with no pipeline changed cuePos to %d, want 0", got)
	}
}

// The cue-end hook must fire for non-hold cues too. handleEnd used to read
// the cue association after teardown, and teardown (clearIfCurrent) zeroes
// it — so a cue without hold ended silently: no cue_end audit, no
// auto-continue. Regression: capture-before-teardown fires the hook with the
// position that just finished, for both the hold and the teardown path.
func TestCueEndHookFiresWithoutHold(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping cue-end hook test")
	}

	// A real (tiny) WAV on disk so filesrc can open it; the media dir is
	// pointed at a temp location for the duration of the test.
	dir := t.TempDir()
	config.SetDirsForTesting(dir)
	if err := os.WriteFile(filepath.Join(dir, "hook-test.wav"), tinyWav(1), 0o644); err != nil {
		t.Fatalf("writing media fixture: %v", err)
	}

	fired := make(chan int, 1)
	SetCueEndHook(func(pos int) { fired <- pos })
	defer SetCueEndHook(nil)

	for _, hold := range []bool{false, true} {
		if err := LoadWithOpts("hook-test.wav", LoadOpts{Hold: hold}); err != nil {
			t.Fatalf("LoadWithOpts: %v", err)
		}
		SetCuePos(7)
		mgr.mu.Lock()
		p := mgr.pipeline
		mgr.mu.Unlock()
		if p == nil {
			t.Fatalf("no pipeline after load")
		}
		mgr.handleEnd(p)

		select {
		case got := <-fired:
			if got != 7 {
				t.Errorf("hook fired with pos %d, want 7 (hold=%v)", got, hold)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("cue-end hook did not fire (hold=%v)", hold)
		}
	}
}

// tinyWav builds a minimal valid PCM WAV of n seconds of 440Hz tone
// (same fixture approach as the routes tests).
func tinyWav(seconds int) []byte {
	const sampleRate = 8000
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+2*seconds*sampleRate))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(2*seconds*sampleRate))
	for s := 0; s < seconds*sampleRate; s++ {
		v := int16(3000 * math.Sin(2*math.Pi*440*float64(s%int(sampleRate))/float64(sampleRate)))
		binary.Write(&buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

// Rapid cue swaps must leave the ACTIVE pipeline owning the volume/fade
// handles (pad-added of a replaced pipeline fires asynchronously and used to
// re-point them at the dead pipeline), and each retired pipeline's bus watch
// must unregister (retirePipeline wake-up + identity precheck) instead of
// pinning pipeline+watch for the process lifetime. Requires real GStreamer.
// ponytail: the pad-added race itself is timing-dependent, so this asserts
// the invariant after settle windows rather than proving the old failure.
func TestSwapKeepsHandlesOnActivePipeline(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping swap-handle test")
	}
	dir := t.TempDir()
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := os.WriteFile(filepath.Join(dir, "swap.wav"), tinyWav(1), 0o644); err != nil {
		t.Skipf("no writable media fixture dir: %v", err)
	}

	assertHandlesCurrent := func() {
		t.Helper()
		time.Sleep(700 * time.Millisecond) // pad-added settles asynchronously
		mgr.mu.Lock()
		p, vol, bright := mgr.pipeline, mgr.volumeEl, mgr.brightEl
		mgr.mu.Unlock()
		if p == nil {
			t.Fatalf("no active pipeline")
		}
		if vol == nil {
			t.Fatalf("active pipeline has no volume handle (audio pad-added did not fire)")
		}
		name := vol.GetName()
		activeVol, err := p.GetElementByName(name)
		if err != nil || activeVol == nil {
			t.Fatalf("active pipeline does not contain the volume handle element %q (stale handle from a retired pipeline): %v", name, err)
		}
		if vol.Instance() != activeVol.Instance() {
			t.Fatalf("volume handle %q belongs to a RETIRED pipeline, not the active one", name)
		}
		_ = bright // videotestsrc/na tests have no video element; wav has no brightness
	}

	// Load A, then swap A->B, then a stress burst: the handle must always
	// track the newest pipeline and every retirement must clean up its watch.
	if err := LoadWithOpts("swap.wav", LoadOpts{Volume: 6}); err != nil {
		t.Fatalf("load A: %v", err)
	}
	assertHandlesCurrent()
	if err := LoadWithOpts("swap.wav", LoadOpts{Volume: -6}); err != nil {
		t.Fatalf("load B: %v", err)
	}
	assertHandlesCurrent()
	for i := 0; i < 5; i++ {
		if err := Load("swap.wav"); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}
	assertHandlesCurrent()

	// Panic retires the pipeline: the wake-up must unregister its watch
	// without side effects, and the machinery must keep working afterwards.
	Panic()
	time.Sleep(300 * time.Millisecond)
	if got := CurrentPlaying(); got != "" {
		t.Fatalf("after Panic CurrentPlaying() = %q, want empty", got)
	}
	if err := ShowTest("smpte"); err != nil {
		t.Fatalf("ShowTest after panic: %v", err)
	}
	Stop()
}
