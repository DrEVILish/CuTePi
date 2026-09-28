package gsp

import (
	"testing"

	"github.com/go-gst/go-gst/gst"
)

// The wall sink resolves from the environment so headless CI (fakesink),
// field consoles (fbdevsink) and desktops (autovideosink default) all build
// the same chain shape around a different tail.
func TestWallVideoSink(t *testing.T) {
	t.Setenv("CUTEPI_WALL_SINK", "")
	if got := wallVideoSink(); got != "autovideosink" {
		t.Errorf("wallVideoSink() = %q, want autovideosink", got)
	}
	t.Setenv("CUTEPI_WALL_SINK", "fbdevsink")
	if got := wallVideoSink(); got != "fbdevsink" {
		t.Errorf("wallVideoSink() = %q, want fbdevsink", got)
	}
}

// The video branch must always run flips-then-converter before the sink:
// videoflip handles only a subset of raw formats, and without a converter
// on the sink side a picky wall sink (fbdevsink's RGB16 framebuffer)
// cannot negotiate at all — decodebin then fails delayed linking and the
// cue silently never plays (Pi hardware validation, 2026-09).
func TestVideoStemEndsConvertThenSink(t *testing.T) {
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	full := append(videoStem(false), wallVideoSink())
	if full[0] != "queue" {
		t.Errorf("chain starts with %q, want queue", full[0])
	}
	if n := len(full); full[n-1] != "fakesink" || full[n-2] != "videoconvert" {
		t.Errorf("chain ends with %v, want [... videoconvert fakesink]", full[n-2:])
	}
	count := func(want string) int {
		n := 0
		for _, e := range full {
			if e == want {
				n++
			}
		}
		return n
	}
	if got := count("videoflip"); got != 2 {
		t.Errorf("chain has %d videoflip stages, want 2", got)
	}
	if got := count("videoconvert"); got != 2 {
		t.Errorf("chain has %d videoconvert stages, want 2 (pre- and post-flip)", got)
	}
}

// The DMABuf download stage applies to file decodes only: the
// test-pattern source emits system memory and negotiates worse with
// v4l2convert in the chain (device caps probe can fail the query and
// leave the source unlinked — silent no-pattern).
func TestVideoStemDownloadScope(t *testing.T) {
	plain := videoStem(false)
	for _, e := range plain {
		if e == "v4l2convert" {
			t.Errorf("videoStem(false) contains v4l2convert — test-pattern chains must skip the download stage")
		}
	}
	withDL := videoStem(true)
	if len(withDL) != len(plain) && len(withDL) != len(plain)+1 {
		t.Errorf("videoStem(true) = %v, want plain stem plus at most the download stage", withDL)
	}
}

// Unreadable caps default to including the download (hardware-first).
func TestDmaBufUpstreamNilDefaultsInclude(t *testing.T) {
	if !dmaBufUpstream(nil) {
		t.Errorf("dmaBufUpstream(nil) = false, want true (hardware-first default)")
	}
}

// Per-cue settings (brightness, fit, rotation, flip) resolve through a
// role map built from the same slice the chain was created from, so
// optional stages (v4l2convert on hardware decode) can never shift them
// onto the wrong element. The stem layout below is what that wiring
// depends on: balance/scale/flips/converters present exactly once each
// (flips and converters twice).
func TestVideoStemRoleLayout(t *testing.T) {
	for _, download := range []bool{false, true} {
		stem := videoStem(download)
		count := map[string]int{}
		for _, e := range stem {
			count[e]++
		}
		if count["queue"] != 1 || count["videobalance"] != 1 || count["videoscale"] != 1 {
			t.Errorf("videoStem(%v) = %v, want one queue/balance/scale", download, stem)
		}
		if count["videoflip"] != 2 || count["videoconvert"] != 2 {
			t.Errorf("videoStem(%v) = %v, want two flips and two converters", download, stem)
		}
		if stem[0] != "queue" || stem[len(stem)-1] != "videoconvert" {
			t.Errorf("videoStem(%v) = %v, want queue-led, converter-tailed", download, stem)
		}
	}
}

func TestFirstByFactory(t *testing.T) {
	gst.Init(nil)
	balance, err := gst.NewElement("videobalance")
	if err != nil {
		t.Skipf("videobalance unavailable: %v", err)
	}
	scale, err := gst.NewElement("videoscale")
	if err != nil {
		t.Skipf("videoscale unavailable: %v", err)
	}
	m := map[string][]*gst.Element{"videobalance": {balance}, "videoscale": {scale}}
	if firstByFactory(m, "videobalance") != balance {
		t.Errorf("firstByFactory missed videobalance")
	}
	if firstByFactory(m, "videoflip") != nil {
		t.Errorf("firstByFactory found a videoflip that is not there")
	}
}
