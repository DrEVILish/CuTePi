package gsp

import (
	"testing"
	"time"
)

func TestIsStillFile(t *testing.T) {
	for _, f := range []string{"a.png", "B.JPG", "c.jpeg", "d.gif", "e.webp", "f.bmp"} {
		if !isStillFile(f) {
			t.Errorf("%q not treated as a still", f)
		}
	}
	for _, f := range []string{"a.mp4", "b.wav", "", "png"} {
		if isStillFile(f) {
			t.Errorf("%q treated as a still", f)
		}
	}
}

// Halts counts every operator Stop/Panic (even with nothing playing) and
// Loads every installed pipeline, so a delayed action can tell "the operator
// interrupted" apart from its own fade finishing (which also bumps the
// generation).
func TestHaltAndLoadCounters(t *testing.T) {
	gstAvailable(t)
	Panic()
	h0, l0 := Halts(), Loads()
	Stop()
	if Halts() != h0+1 {
		t.Errorf("Stop: halts %d -> %d, want +1", h0, Halts())
	}
	Panic()
	if Halts() != h0+2 {
		t.Errorf("Panic: halts %d -> %d, want +2", h0, Halts())
	}
	if Loads() != l0 {
		t.Errorf("halting changed loads %d -> %d", l0, Loads())
	}
	file := warmFixture(t, "counters.wav", tinyWav(1))
	if err := LoadWithOpts(file, LoadOpts{}); err != nil {
		t.Fatalf("LoadWithOpts: %v", err)
	}
	if Loads() != l0+1 {
		t.Errorf("load: loads %d -> %d, want +1", l0, Loads())
	}
	if Halts() != h0+2 {
		t.Errorf("load changed halts")
	}
	Panic()
	time.Sleep(200 * time.Millisecond)
}

// The warm slot prerolls audio on a fakesink and swaps the real output in at
// activation; otherwise the cue would ignore the configured device.
func TestWarmAudioRelinkedAtActivation(t *testing.T) {
	gstAvailable(t)
	Panic()
	file := warmFixture(t, "warm-audio-wire.wav", tinyWav(1))
	opts := LoadOpts{WarmPreroll: true}
	if err := Warm(file, opts); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	mgr.mu.Lock()
	slot := mgr.warm
	mgr.mu.Unlock()
	if slot == nil {
		t.Fatal("no slot armed")
	}
	if el, _ := slot.GetElementByName("warm-audio-sink"); el == nil {
		t.Fatal("warm slot audio does not end on the placeholder fakesink")
	}
	if !InstallWarm(file, opts) {
		t.Fatal("InstallWarm failed")
	}
	if el, _ := slot.GetElementByName("warm-audio-sink"); el != nil {
		t.Error("placeholder fakesink still in the activated pipeline")
	}
	Panic()
	time.Sleep(200 * time.Millisecond)
}
