package media

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Import and conversion tools run at nice 19 and best-effort I/O level 7,
// with the tool's own PID (nice and ionice exec it in place).
func TestBackgroundCommand(t *testing.T) {
	if bgNice == "" || bgIonice == "" {
		t.Skip("nice or ionice not installed")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	out, err := BackgroundCommand(context.Background(), "sh", "-c", "cat /proc/$$/stat; ionice -p $$").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(out), "\n", 2)
	f := strings.Fields(lines[0][strings.LastIndex(lines[0], ")")+1:])
	if comm := lines[0][strings.Index(lines[0], "(")+1 : strings.LastIndex(lines[0], ")")]; comm != "sh" {
		t.Errorf("process is %q, want the tool itself (sh)", comm)
	}
	if nice := f[16]; nice != "19" {
		t.Errorf("nice %s, want 19", nice)
	}
	if io := strings.TrimSpace(lines[1]); io != "best-effort: prio 7" {
		t.Errorf("I/O priority %q, want best-effort: prio 7", io)
	}
}

// While something plays, the import's decodes run on one thread.
func TestBusyThreads(t *testing.T) {
	saved := PlaybackBusy
	defer func() { PlaybackBusy = saved }()
	PlaybackBusy = nil
	if a := busyThreads(); a != nil {
		t.Errorf("no hook: %v, want none", a)
	}
	PlaybackBusy = func() bool { return false }
	if a := busyThreads(); a != nil {
		t.Errorf("idle: %v, want none", a)
	}
	PlaybackBusy = func() bool { return true }
	if a := strings.Join(busyThreads(), " "); a != "-threads 1 -filter_threads 1" {
		t.Errorf("playing: %q", a)
	}
}
