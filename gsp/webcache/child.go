package webcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-gst/go-gst/gst"
)

// The cache keeper's WebKit work (Preload, Clear) runs in a child process of
// the service binary (cutepi --webcache-preload / --webcache-clear). Tearing
// a WebKit view down can crash in GStreamer 1.26's wpe plugin
// (WPEBackend-fdo release_exported_image on the view being deleted: about one
// start-up in five on the KMS wall, one in two on the GPU wall). In a child
// that costs one attempt, which the keeper retries; in the service it took
// the show down. Same binary, user and WebKit data directories, so the child
// fills the cache the service's live pages read.

// ChildArg reports whether args (os.Args[1:]) ask for a cache child.
func ChildArg(args []string) bool {
	return len(args) > 0 && strings.HasPrefix(args[0], "--webcache-")
}

// childLoaded is the preload child's "page loaded" line: success even if the
// child then dies tearing WebKit down.
const childLoaded = "loaded"

type clearReply struct {
	Removed []string `json:"removed"`
	Error   string   `json:"error,omitempty"`
}

// RunChild does the job a cache child was started for and returns the
// process exit code: 0 done, 2 not loaded / not answered, 3 usage.
func RunChild(args []string) int {
	gst.Init(nil)
	switch {
	case len(args) == 3 && args[0] == "--webcache-preload":
		ms, err := strconv.Atoi(args[2])
		if err != nil {
			return 3
		}
		if err := Preload(args[1], time.Duration(ms)*time.Millisecond, func() bool { return true }); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		// Loaded: say so before the view's teardown (the step that crashes).
		fmt.Println(childLoaded)
		return 0
	case len(args) == 4 && args[0] == "--webcache-clear":
		var remove, keep []string
		ms, err1 := strconv.Atoi(args[3])
		if json.Unmarshal([]byte(args[1]), &remove) != nil || json.Unmarshal([]byte(args[2]), &keep) != nil || err1 != nil {
			return 3
		}
		removed, err := Clear(remove, keep, time.Duration(ms)*time.Millisecond)
		r := clearReply{Removed: removed}
		if err != nil {
			r.Error = err.Error()
		}
		json.NewEncoder(os.Stdout).Encode(r)
		if err != nil {
			return 2
		}
		return 0
	}
	return 3
}

// child runs the service binary with args, killing it when keepGoing turns
// false (ErrStopped) or after limit; stdout is returned.
func child(args []string, limit time.Duration, keepGoing func() bool) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(limit)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				return out.Bytes(), nil
			}
			msg := strings.TrimSpace(errb.String())
			if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
				msg = msg[i+1:]
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() < 0 {
				return out.Bytes(), fmt.Errorf("WebKit child process died (%v): retried later", ee.ProcessState)
			}
			if msg == "" {
				msg = err.Error()
			}
			return out.Bytes(), errors.New(msg)
		case <-tick.C:
			if !keepGoing() {
				cmd.Process.Kill()
				<-done
				return nil, ErrStopped
			}
		case <-deadline:
			cmd.Process.Kill()
			<-done
			return nil, fmt.Errorf("WebKit child process gave no answer within %v", limit)
		}
	}
}

// PreloadIsolated is Preload in a child process.
func PreloadIsolated(url string, timeout time.Duration, keepGoing func() bool) error {
	out, err := child([]string{"--webcache-preload", url, strconv.FormatInt(timeout.Milliseconds(), 10)}, timeout+15*time.Second, keepGoing)
	if err != nil && err != ErrStopped && strings.Contains(string(out), childLoaded) {
		return nil // loaded; only the teardown failed
	}
	return err
}

// ClearIsolated is Clear in a child process.
func ClearIsolated(remove, keep []string, timeout time.Duration) ([]string, error) {
	rj, _ := json.Marshal(remove)
	kj, _ := json.Marshal(keep)
	out, err := child([]string{"--webcache-clear", string(rj), string(kj), strconv.FormatInt(timeout.Milliseconds(), 10)},
		timeout+15*time.Second, func() bool { return true })
	var r clearReply
	if jerr := json.Unmarshal(out, &r); jerr == nil {
		if r.Error != "" {
			return r.Removed, errors.New(r.Error)
		}
		return r.Removed, nil
	}
	if err == nil {
		err = errors.New("WebKit child process gave no reply")
	}
	return nil, err
}
