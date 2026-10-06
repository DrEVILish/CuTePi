package webcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneSandboxDirs(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, info string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if info != "" {
			if err := os.WriteFile(filepath.Join(p, "bwrapinfo.json"), []byte(info), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
	}
	mk("webkit-100-1", `{"child-pid": 500}`, 0) // owner gone
	mk("webkit-200-1", `{"child-pid": 501}`, 0) // child exited
	mk("webkit-200-2", `{"child-pid": 600}`, 0) // in use
	mk("webkit-200-3", ``, time.Second)         // starting: no info yet
	mk("webkit-200-4", ``, 2*time.Minute)       // never got info
	mk("other-200-1", `{"child-pid": 501}`, 0)  // not WebKit's
	alive := func(pid int) bool { return pid == 200 || pid == 600 }

	if n := pruneSandboxDirs(dir, alive, time.Now()); n != 3 {
		t.Fatalf("removed %d, want 3", n)
	}
	for _, keep := range []string{"webkit-200-2", "webkit-200-3", "other-200-1"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s removed", keep)
		}
	}
	for _, gone := range []string{"webkit-100-1", "webkit-200-1", "webkit-200-4"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s kept", gone)
		}
	}
}
