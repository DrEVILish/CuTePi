package gsp

import (
	"testing"
	"time"

	"github.com/go-gst/go-gst/gst"
)

// Background work waits for playback to have been quiet a while, and a
// build starting (NoteBusy) stops it at once.
func TestPlaybackQuiet(t *testing.T) {
	lastBusy.Store(0)
	if !PlaybackQuiet(time.Second) {
		t.Fatal("idle and quiet for ages: not quiet")
	}
	start := time.Now()
	if BusySince(start) {
		t.Fatal("busy without activity")
	}
	NoteBusy()
	if !BusySince(start) {
		t.Error("a build started: not reported busy")
	}
	if PlaybackQuiet(time.Second) {
		t.Error("quiet right after activity")
	}
	if !PlaybackQuiet(0) {
		t.Error("nothing playing now: PlaybackQuiet(0) false")
	}
}

// Intra-only decoders get QoS off as they join a pipeline; others keep it.
func TestIntraNoQoS(t *testing.T) {
	gstInit()
	for name, want := range map[string]bool{"avdec_prores": false, "avdec_dnxhd": false, "jpegdec": false, "avdec_h264": true} {
		e, err := gst.NewElement(name)
		if err != nil {
			t.Logf("%s not installed", name)
			continue
		}
		intraNoQoS(e)
		if v, _ := e.GetProperty("qos"); v != want {
			t.Errorf("%s qos = %v, want %v", name, v, want)
		}
	}
}
