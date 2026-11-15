package gsp

import (
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
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
			if ok && rate == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("rate=%v want=%v", rate, want)
	}
	// Live mix changes go straight to the manager (ApplyCueMix needs a cue).
	mix := func(f func()) {
		mgr.mu.Lock()
		f()
		mgr.applyGain()
		mgr.panEl.Set("panorama", float32(mgr.balance))
		mgr.mu.Unlock()
	}
	checkGain(0)
	mix(func() { mgr.volume = 6 })
	checkGain(0)
	mix(func() { mgr.mute = false })
	checkGain(1)
	mix(func() { mgr.balance = 1 })
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
	mgr.mu.Lock()
	mgr.mute = true
	mgr.applyGain()
	mgr.mu.Unlock()
	done := make(chan struct{})
	go func() { FadeAndStop(100); close(done) }()
	time.Sleep(40 * time.Millisecond)
	checkGain(0)
	<-done
}

// The bus's silence reaches the device as sound, never as GAP buffers: the
// sink waits on a GAP until its time plus the whole bus latency, and the
// first ~200 ms of a cue starting on a silent bus came too late and were lost.
func TestAudioBusSilenceIsNotGap(t *testing.T) {
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	owner, err := gst.NewPipeline("gap-test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audioBusInput(owner); err != nil {
		t.Skip(err)
	}
	defer audioBusRelease(owner)
	abus.mu.Lock()
	sink := abus.sink
	abus.mu.Unlock()
	var bufs, gaps atomic.Int32
	sink.GetStaticPad("sink").AddProbe(gst.PadProbeTypeBuffer, func(_ *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		bufs.Add(1)
		if b := info.GetBuffer(); b != nil && b.HasFlags(gst.BufferFlagGap) {
			gaps.Add(1)
		}
		return gst.PadProbeOK
	})
	waitFor(t, "the bus to play silence", func() bool { return bufs.Load() >= 20 })
	if g := gaps.Load(); g != 0 {
		t.Fatalf("%d of %d buffers reached the bus sink as GAP", g, bufs.Load())
	}
}

// busLatency (how far ahead every input plays) is the bus's own latency, and
// the presenter's start latency leaves room for it plus the input sink's
// render delay, so a cue's first sound is not late.
func TestAudioBusLatency(t *testing.T) {
	if pwStartLatency < busLatency+busSrcLatency {
		t.Fatalf("pwStartLatency %v < busLatency %v + %v", pwStartLatency, busLatency, busSrcLatency)
	}
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	owner, err := gst.NewPipeline("latency-test-owner")
	if err != nil {
		t.Fatal(err)
	}
	in, err := audioBusInput(owner)
	if err != nil {
		t.Skip(err)
	}
	defer audioBusRelease(owner)
	if off, _ := in.GetProperty("ts-offset"); !glOpen && off.(int64) != -int64(busLatency) {
		t.Fatalf("input ts-offset %v, want %v", off, -busLatency)
	}
	abus.mu.Lock()
	p := abus.p
	abus.mu.Unlock()
	// Without an audio device (autoaudiosink's fakesink) the sink adds nothing.
	var min time.Duration
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && min <= busSrcLatency+busMixOutput; time.Sleep(20 * time.Millisecond) {
		if q := gst.NewLatencyQuery(); p.Query(q) {
			_, m, _ := q.ParseLatency()
			min = time.Duration(m)
		}
	}
	if min <= busSrcLatency+busMixOutput {
		t.Skipf("no audio device behind the bus (latency %v)", min)
	}
	if d := min - busLatency; d > time.Millisecond || d < -time.Millisecond { // a resampling input adds a few samples
		t.Fatalf("bus latency %v, busLatency %v", min, busLatency)
	}
}

// An input that joins while the bus is idle (every input left, the device
// not yet released) is heard: the bus keeps running between cues.
func TestAudioBusInputAfterIdle(t *testing.T) {
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	a, err := gst.NewPipeline("idle-test-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audioBusInput(a); err != nil {
		t.Skip(err)
	}
	time.Sleep(300 * time.Millisecond)
	audioBusRelease(a)
	time.Sleep(300 * time.Millisecond)
	b, _ := gst.NewPipeline("idle-test-b")
	if _, err := audioBusInput(b); err != nil {
		t.Fatal(err)
	}
	defer audioBusRelease(b)
	abus.mu.Lock()
	sink := abus.sink
	abus.mu.Unlock()
	var bufs atomic.Int32
	sink.GetStaticPad("sink").AddProbe(gst.PadProbeTypeBuffer, func(*gst.Pad, *gst.PadProbeInfo) gst.PadProbeReturn {
		bufs.Add(1)
		return gst.PadProbeOK
	})
	waitFor(t, "the bus to play after an idle spell", func() bool { return bufs.Load() >= 20 })
}

// A cue at another sample rate than the bus's is heard: the mixer takes one
// rate, and a 44.1 kHz input on a 48 kHz bus was refused (not-negotiated).
func TestAudioBusMixedRates(t *testing.T) {
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	a, _ := gst.NewPipeline("rate-test-a")
	if _, err := audioBusInput(a); err != nil { // 48 kHz: the bus's rate
		t.Skip(err)
	}
	defer audioBusRelease(a)
	b, _ := gst.NewPipeline("rate-test-b")
	in, err := audioBusInput(b)
	if err != nil {
		t.Fatal(err)
	}
	defer audioBusRelease(b)
	els, err := gst.NewElementMany("audiotestsrc", "capsfilter")
	if err != nil {
		t.Fatal(err)
	}
	els[0].Set("is-live", true)
	els[1].Set("caps", gst.NewCapsFromString("audio/x-raw,rate=44100,channels=2"))
	b.AddMany(append(els, in)...)
	gst.ElementLinkMany(append(els, in)...)
	b.SetState(gst.StatePlaying)
	defer b.SetState(gst.StateNull)
	abus.mu.Lock()
	ch := abus.owners[b][0]
	pad := abus.inputs[ch].pad
	abus.mu.Unlock()
	var bufs atomic.Int32
	pad.AddProbe(gst.PadProbeTypeBuffer, func(*gst.Pad, *gst.PadProbeInfo) gst.PadProbeReturn {
		bufs.Add(1)
		return gst.PadProbeOK
	})
	time.Sleep(time.Second)
	if n := bufs.Load(); n < 100 {
		t.Fatalf("%d buffers in 1 s from a 44.1 kHz input", n)
	}
}

// An audio settings change rebuilds the bus at once, inputs attached: a
// playing cue's channel is read again by the new bus (its sound moves to the
// new output instead of waiting for the bus to go idle).
func TestAudioBusResetKeepsInputs(t *testing.T) {
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	b, _ := gst.NewPipeline("reset-test")
	in, err := audioBusInput(b)
	if err != nil {
		t.Skip(err)
	}
	defer audioBusRelease(b)
	src, _ := gst.NewElement("audiotestsrc")
	src.Set("is-live", true)
	b.AddMany(src, in)
	src.Link(in)
	b.SetState(gst.StatePlaying)
	defer b.SetState(gst.StateNull)
	abus.mu.Lock()
	old := abus.p
	abus.mu.Unlock()
	ResetAudioBus()
	abus.mu.Lock()
	p, n := abus.p, len(abus.inputs)
	pad := abus.inputs[abus.owners[b][0]].pad
	abus.mu.Unlock()
	if p == nil || p == old || n != 1 {
		t.Fatalf("after reset: bus %p (old %p), %d inputs", p, old, n)
	}
	var bufs atomic.Int32
	pad.AddProbe(gst.PadProbeTypeBuffer, func(_ *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		if bf := info.GetBuffer(); bf != nil && !bf.HasFlags(gst.BufferFlagGap) {
			bufs.Add(1)
		}
		return gst.PadProbeOK
	})
	waitFor(t, "the cue's sound on the rebuilt bus", func() bool { return bufs.Load() >= 20 })
}
