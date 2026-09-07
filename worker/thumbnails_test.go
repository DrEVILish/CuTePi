package worker

import (
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// Regression test: before the failureTracker existed, a persistently
// failing thumbnail (e.g. a corrupt upload) was retried on every single
// poll tick forever, spamming ffmpeg failures and burning CPU. The tracker
// must suppress retries for retryBackoff after a failure, then allow one
// again once the backoff has elapsed.
func TestFailureTrackerBacksOffThenRetries(t *testing.T) {
	f := newFailureTracker()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const backoff = 60 * time.Second

	if f.shouldSkip(1, backoff, base) {
		t.Fatalf("expected no skip before any failure is recorded")
	}

	f.recordFailure(1, base)

	if !f.shouldSkip(1, backoff, base.Add(1*time.Second)) {
		t.Fatalf("expected skip immediately after a failure")
	}
	if !f.shouldSkip(1, backoff, base.Add(59*time.Second)) {
		t.Fatalf("expected skip while still within the backoff window")
	}
	if f.shouldSkip(1, backoff, base.Add(61*time.Second)) {
		t.Fatalf("expected retry to be allowed once the backoff window has elapsed")
	}
}

func TestFailureTrackerClearAllowsImmediateRetry(t *testing.T) {
	f := newFailureTracker()
	now := time.Now()
	const backoff = 60 * time.Second

	f.recordFailure(2, now)
	if !f.shouldSkip(2, backoff, now) {
		t.Fatalf("expected skip right after a failure")
	}

	f.clear(2)
	if f.shouldSkip(2, backoff, now) {
		t.Fatalf("expected no skip immediately after clearing a failure")
	}
}

func TestFailureTrackerIsPerMediaID(t *testing.T) {
	f := newFailureTracker()
	now := time.Now()
	const backoff = 60 * time.Second

	f.recordFailure(1, now)
	if f.shouldSkip(2, backoff, now) {
		t.Fatalf("failure for media 1 must not affect media 2")
	}
}

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
