package worker

import (
	"testing"
	"time"
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
