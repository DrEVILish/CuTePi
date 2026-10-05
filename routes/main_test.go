package routes

import (
	"os"
	"testing"
	"time"
)

// TestMain disarms the process-level restart/shutdown hooks for the whole
// package. A settings save that changes the port or display schedules a
// restart 500 ms later on its own goroutine; with the real hooks in place
// that goroutine outlived its test and called restartProcess, which
// re-executed this test binary (same args, detached) — every full run
// cascaded into overlapping copies of the suite that fought over the audio
// device (the Awards tests' "timed out waiting for m1 playing") and tripled
// the run time. Under systemd it would have restarted the real service.
// Tests that check the hooks are invoked swap in their own recorders.
func TestMain(m *testing.M) {
	restartServer = func(time.Duration) error { return nil }
	shutdownServer = func(time.Duration) {}
	os.Exit(m.Run())
}
