package webcache

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// WebKit starts each web process in a bubblewrap sandbox and leaves a
// folder per launch in ~/.cache/.flatpak (webkit-<owner pid>-<n>, holding
// bwrapinfo.json) that it never removes: one per live-page fire, preload
// and cache operation, piling up for ever.

var sandboxName = regexp.MustCompile(`^webkit-(\d+)-\d+$`)

// sandboxGrace keeps a folder without readable sandbox info this long: the
// sandbox may still be starting.
const sandboxGrace = time.Minute

// PruneSandboxDirs removes the stale WebKit sandbox folders: those whose
// owner process is gone, or whose sandboxed web process has exited. It
// returns how many it removed.
func PruneSandboxDirs() int {
	cache := userCacheDir()
	if cache == "" {
		return 0
	}
	return pruneSandboxDirs(filepath.Join(cache, ".flatpak"), processAlive, time.Now())
}

// userCacheDir finds the cache folder the way GLib (and so WebKit) does:
// XDG_CACHE_HOME, else $HOME/.cache, else the account's home from the
// password database. A systemd service has no HOME, where os.UserCacheDir
// fails but WebKit still writes to /root/.cache.
func userCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return dir
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".cache")
	}
	return ""
}

func pruneSandboxDirs(dir string, alive func(pid int) bool, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		m := sandboxName.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !sandboxStale(path, m[1], alive, now) {
			continue
		}
		if os.RemoveAll(path) == nil {
			removed++
		}
	}
	return removed
}

func sandboxStale(path, ownerPID string, alive func(int) bool, now time.Time) bool {
	owner, _ := strconv.Atoi(ownerPID)
	if !alive(owner) {
		return true
	}
	var info struct {
		ChildPID int `json:"child-pid"`
	}
	b, err := os.ReadFile(filepath.Join(path, "bwrapinfo.json"))
	if err != nil || json.Unmarshal(b, &info) != nil || info.ChildPID <= 0 {
		st, err := os.Stat(path)
		return err == nil && now.Sub(st.ModTime()) > sandboxGrace
	}
	return !alive(info.ChildPID)
}

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
