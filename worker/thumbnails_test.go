package worker

import (
	"strings"
	"testing"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// TestWaveformFailureClearsFlag is the runnable check for the retry-forever
// fix: a pending waveform whose file cannot be decoded must have its
// waveform_pending flag cleared by one processOne call, so the worker stops
// re-running the full ffmpeg decode on every poll tick (the code comment
// always claimed this; the call was missing).
func TestWaveformFailureClearsFlag(t *testing.T) {
	dir := t.TempDir()
	config.SetDbLocation(":memory:")
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	// Register a media row whose file does not exist on disk.
	if err := ctp.RegisterMedia("ghost.wav", 10, media.Metadata{Mimetype: "audio/wav", Duration: 1}, "ghost.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	pending, err := ctp.PendingThumbnails()
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending row, got %d (err=%v)", len(pending), err)
	}

	// The file does not exist, so media.GeneratePeaks fails without any
	// external injection - exactly the production failure path. Mark the
	// thumbnail done first so only the waveform branch is active.
	if err := ctp.MarkThumbnailDone(pending[0].Media_id); err != nil {
		t.Fatalf("MarkThumbnailDone: %v", err)
	}
	pending, err = ctp.PendingThumbnails()
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 waveform-pending row, got %d (err=%v)", len(pending), err)
	}
	processOne(pending[0])
	pending, err = ctp.PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("waveform_pending not cleared after failure: still %d pending rows", len(pending))
	}
}

// TestThumbnailFailureClearsFlag is the runnable check for the thumbnail
// give-up behavior: when thumbnail generation fails (corrupt or missing
// file), thumbnail_pending must be cleared so the worker stops
// re-attempting it on every poll (including after restarts). Operators can
// always re-request generation via the refresh button.
func TestThumbnailFailureClearsFlag(t *testing.T) {
	dir := t.TempDir()
	config.SetDbLocation(":memory:")
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if err := ctp.RegisterMedia("missing.mp4", 0, media.Metadata{Mimetype: "video/mp4", Duration: 1}, "missing"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	pending, err := ctp.PendingThumbnails()
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending row, got %d (err=%v)", len(pending), err)
	}
	// Clear waveform so only the thumbnail branch fires.
	if err := ctp.FailWaveform(pending[0].Media_id); err != nil {
		t.Fatalf("FailWaveform: %v", err)
	}
	pending, err = ctp.PendingThumbnails()
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 thumbnail-pending row, got %d (err=%v)", len(pending), err)
	}
	processOne(pending[0])
	pending, err = ctp.PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("thumbnail_pending not cleared after failure: still %d pending rows", len(pending))
	}
}

// TestWaveformRebuildFlagsLowResOnly covers the comma-count heuristic used to
// spot pre-resolution-bump envelopes: old fixed-size archives must be flagged
// for regeneration, already-detailed ones (matching the per-duration target)
// must be left alone, and never-analysed/empty envelopes must not churn.
func TestWaveformRebuildFlagsLowResOnly(t *testing.T) {
	mk := func(wave string, dur float64) ctp.Media {
		return ctp.Media{Waveform: wave, Duration: dur}
	}
	cases := []struct {
		name string
		m    ctp.Media
		want bool
	}{
		{"empty (never analysed)", mk("", 120), false},
		{"old 300-bin archive, long file", mk(jsonFew(300), 600), true},
		{"old 300-bin archive, short file", mk(jsonFew(300), 2), true},
		{"built 2000-bin archive", mk(jsonFew(2000), 180), true},
		{"current resolution, short file", mk(jsonFew(500), 5), false},
		{"current resolution, capped long file", mk(jsonFew(16000), 600), false},
		{"corrupt envelope", mk("not-json", 60), true},
	}
	for _, c := range cases {
		if got := needsWaveformRebuild(c.m); got != c.want {
			t.Errorf("%s: needsWaveformRebuild = %v, want %v", c.name, got, c.want)
		}
	}
}

func jsonFew(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("0.5")
	}
	b.WriteByte(']')
	return b.String()
}
