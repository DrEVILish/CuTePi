package gsp

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"CuTePi/config"
	"github.com/go-gst/go-gst/gst"
)

func TestAudioSettingsPlayback(t *testing.T) {
	dir := t.TempDir()
	config.SetDirsForTesting(dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.wav"), tinyWav(10), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadWithOpts("settings.wav", LoadOpts{Rate: 1.5, InPoint: 1, Volume: 3, LoudnessGain: -6, Balance: -0.5, Mute: true, FadeIn: 200}); err != nil {
		t.Fatal(err)
	}
	defer Panic()
	time.Sleep(300 * time.Millisecond)
	mgr.mu.Lock()
	p, vol, pan := mgr.pipeline, mgr.volumeEl, mgr.panEl
	mgr.mu.Unlock()
	if p == nil || vol == nil || pan == nil {
		t.Fatal("missing audio pipeline/controls")
	}
	checkGain := func(want float64) {
		t.Helper()
		value, err := vol.GetProperty("volume")
		if err != nil || math.Abs(value.(float64)-want) > 0.001 {
			t.Fatalf("volume=%v err=%v want=%v", value, err, want)
		}
	}
	checkRate := func(want float64) {
		t.Helper()
		var rate float64
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			q := gst.NewSegmentQuery(gst.FormatTime)
			ok := vol.Query(q)
			rate, _, _, _ = q.ParseSegment()
			q.Unref()
			if ok && rate == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("rate=%v want=%v", rate, want)
	}
	checkGain(0)
	SetVolume(6)
	checkGain(0)
	SetMute(false)
	checkGain(1)
	SetBalance(1)
	value, err := pan.GetProperty("panorama")
	if err != nil || value.(float32) != 1 {
		t.Fatalf("pan=%v err=%v", value, err)
	}
	checkRate(1.5)
	Seek(2)
	checkRate(1.5)
	SetRate(0.5)
	checkRate(0.5)
	mgr.mu.Lock()
	mgr.loop = true
	mgr.loopRemain = 2
	mgr.mu.Unlock()
	if !mgr.handleEnd(p) {
		t.Fatal("expected loop restart")
	}
	checkRate(0.5)
	Stop()
	Play()
	time.Sleep(250 * time.Millisecond)
	mgr.mu.Lock()
	vol = mgr.volumeEl
	mgr.mu.Unlock()
	if vol == nil {
		t.Fatal("no volume after stop/play")
	}
	checkRate(0.5)
	// A fade-in must start below full gain, finish at the normalized gain,
	// and a subsequent fade-out must never unmute the stream.
	if err := LoadWithOpts("settings.wav", LoadOpts{Volume: 6, LoudnessGain: -6, FadeIn: 200}); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	vol = mgr.volumeEl
	mgr.mu.Unlock()
	value, _ = vol.GetProperty("volume")
	if value.(float64) >= 1 {
		t.Fatal("fade-in starts at full gain")
	}
	time.Sleep(300 * time.Millisecond)
	checkGain(1)
	SetMute(true)
	done := make(chan struct{})
	go func() { FadeAndStop(100); close(done) }()
	time.Sleep(40 * time.Millisecond)
	checkGain(0)
	<-done
}
