package scanout

/*
#include <stdlib.h>
#include <gst/gst.h>
int livepage_child_run(const char *url, int w, int h, int fps, int transparent, int sock);
void livepage_remote_start(GstElement *appsrc, int sock, int standby);
void livepage_remote_stop(GstElement *appsrc);
void livepage_remote_pace(GstElement *appsrc, int fps);
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

// Out-of-process live pages (remote.c): WebKit renders in a child of the
// service, `cutepi --livepage-render URL W H FPS TRANSPARENT`, and its frames
// reach the cue's pipeline through an appsrc as dmabufs. A WebKit view torn
// down inside the service could crash it; the child is never torn down, it
// exits when the cue's pipeline stops.

const childFlag = "--livepage-render"

// ChildArg reports whether args (os.Args[1:]) start a live-page renderer.
func ChildArg(args []string) bool { return len(args) > 0 && args[0] == childFlag }

// RunChild is the renderer process: it serves frames on fd 3 until the
// service closes it. Returns the exit code.
// The child's page URL travels in its environment (urlEnv); urlFromEnv
// stands in for it on the command line.
const (
	urlEnv     = "CUTEPI_LIVEPAGE_URL"
	urlFromEnv = "-env"
)

func RunChild(args []string) int {
	if len(args) != 6 {
		return 3
	}
	w, _ := strconv.Atoi(args[2])
	h, _ := strconv.Atoi(args[3])
	fps, _ := strconv.Atoi(args[4])
	transparent := 0
	if args[5] == "1" {
		transparent = 1
	}
	gst.Init(nil)
	url := args[1]
	if url == urlFromEnv {
		url = os.Getenv(urlEnv)
		os.Unsetenv(urlEnv)
	}
	cu := C.CString(url)
	defer C.free(unsafe.Pointer(cu))
	return int(C.livepage_child_run(cu, C.int(w), C.int(h), C.int(fps), C.int(transparent), 3))
}

// StartRemote starts a renderer for url and feeds its frames into appsrc
// (caps already set). The renderer ends by itself when the pipeline holding
// appsrc goes back to NULL, or within a minute if it never starts. A standby
// renderer (a page loaded ahead of its GO) has no such minute: it renders,
// its frames dropped at the stopped appsrc, until the pipeline plays or
// StopRemote ends it.
func StartRemote(appsrc *gst.Element, url string, w, h, fps int, transparent, standby bool) error {
	if !Register() {
		return fmt.Errorf("live page renderer: wpedmabuf unavailable")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return err
	}
	tr := "0"
	if transparent {
		tr = "1"
	}
	childEnd := os.NewFile(uintptr(fds[1]), "livepage")
	// The URL can carry a screen key (a credential): it goes in the child's
	// environment (readable by the service user and root only), not on its
	// command line, which every local user can read in /proc.
	cmd := exec.Command(exe, childFlag, urlFromEnv, strconv.Itoa(w), strconv.Itoa(h), strconv.Itoa(fps), tr)
	cmd.Env = append(os.Environ(), urlEnv+"="+url)
	cmd.ExtraFiles = []*os.File{childEnd} // fd 3 in the child
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		childEnd.Close()
		syscall.Close(fds[0])
		return err
	}
	childEnd.Close()
	go cmd.Wait() // reap; it exits when the socket closes
	sb := 0
	if standby {
		sb = 1
	}
	C.livepage_remote_start((*C.GstElement)(appsrc.Unsafe()), C.int(fds[0]), C.int(sb))
	return nil
}

// PaceRemote sets how many frames a second appsrc's renderer takes from
// WebKit: a standby page paced low stops painting in between (WebKit paints
// a frame once the last one is taken); 0 resumes every frame, dropping the
// stale ones queued meanwhile.
func PaceRemote(appsrc *gst.Element, fps int) {
	C.livepage_remote_pace((*C.GstElement)(appsrc.Unsafe()), C.int(fps))
}

// StopRemote ends appsrc's renderer, if it has one still running.
func StopRemote(appsrc *gst.Element) {
	C.livepage_remote_stop((*C.GstElement)(appsrc.Unsafe()))
}
