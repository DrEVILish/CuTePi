// Package webcache keeps WebKit's HTTP cache for live-page cues (DESIGN
// §12.14): it preloads a page off screen so its assets are on disk before
// the cue fires, and removes a site's cached assets once no cue uses it.
//
// Both run through a private wpevideosrc into a fakesink: no display plane,
// no audio, nothing on the wall. WebKit itself is looked up at run time, so
// CuTePi builds without its development headers; where it is missing
// Available reports false and both calls do nothing.
package webcache

/*
#cgo pkg-config: gobject-2.0 gio-2.0
#cgo LDFLAGS: -ldl
#include <stdlib.h>
#include "webcache.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/go-gst/go-gst/gst"
)

var (
	availOnce sync.Once
	avail     bool
)

// Available reports whether WebKit (and so the cache) can be used here.
func Available() bool {
	availOnce.Do(func() {
		if f := gst.Find("wpevideosrc"); f == nil {
			return
		}
		avail = C.cutepi_wk_available() == 1
	})
	return avail
}

// ErrStopped means the caller's keepGoing said stop (the wall became busy).
var ErrStopped = errors.New("preload stopped")

// Preload loads url in a hidden view until WebKit reports it loaded, which
// leaves its assets in the cache by the server's own cache rules. It stops
// early, returning ErrStopped, as soon as keepGoing reports false (the wall
// is busy: a preload costs about 1.5 cores for its ~2 s on a Pi 4).
func Preload(url string, timeout time.Duration, keepGoing func() bool) error {
	p, err := gst.NewPipelineFromString(
		"wpevideosrc name=src ! video/x-raw,format=BGRA,width=1920,height=1080,framerate=1/1 ! fakesink sync=true")
	if err != nil {
		return err
	}
	defer p.SetState(gst.StateNull)
	src, err := p.GetElementByName("src")
	if err != nil {
		return err
	}
	src.Set("location", url)
	p.SetState(gst.StatePlaying)
	bus := p.GetPipelineBus()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !keepGoing() {
			return ErrStopped
		}
		msg := bus.TimedPopFiltered(gst.ClockTime(100*time.Millisecond), gst.MessageElement|gst.MessageError)
		if msg == nil {
			continue
		}
		if msg.Type() == gst.MessageError {
			return msg.ParseError()
		}
		if loaded(msg) {
			return nil
		}
	}
	return fmt.Errorf("not loaded within %v", timeout)
}

// loaded reports wpevideosrc's "page loaded" (wpe-stats at 100%).
func loaded(msg *gst.Message) bool {
	st := msg.GetStructure()
	if st == nil || st.Name() != "wpe-stats" {
		return false
	}
	v, err := st.GetValue("estimated-load-progress")
	f, ok := v.(float64)
	return err == nil && ok && f >= 100
}

// Clear drops the cached assets of every site that one of remove belongs to
// and none of keep does; with keep empty (no live cue left) it drops the
// whole cache. WebKit keeps its cache per site (timer.example.com is cached
// as example.com), so a site shared with a kept host stays. It returns the
// sites removed.
func Clear(remove, keep []string, timeout time.Duration) ([]string, error) {
	job := (*C.cutepi_wk_job)(C.calloc(1, C.sizeof_cutepi_wk_job))
	job.mode = C.CUTEPI_WK_REMOVE
	if len(keep) == 0 {
		job.mode = C.CUTEPI_WK_CLEAR_ALL
	}
	job.remove_hosts = C.CString(strings.Join(remove, "\n")) // freed with the job
	job.keep_hosts = C.CString(strings.Join(keep, "\n"))
	p, err := gst.NewPipelineFromString(
		"wpevideosrc name=src location=about:blank ! video/x-raw,format=BGRA,width=64,height=64,framerate=1/1 ! fakesink sync=true")
	if err != nil {
		C.cutepi_wk_job_free(job)
		return nil, err
	}
	src, err := p.GetElementByName("src")
	if err != nil {
		C.cutepi_wk_job_free(job)
		return nil, err
	}
	C.cutepi_wk_attach(unsafe.Pointer(src.Unsafe()), job)
	p.SetState(gst.StatePlaying)
	deadline := time.Now().Add(timeout)
	for C.cutepi_wk_done(job) == 0 && C.cutepi_wk_started(job) >= 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	p.SetState(gst.StateNull)
	switch {
	case C.cutepi_wk_started(job) < 0:
		C.cutepi_wk_job_free(job)
		return nil, errors.New("WebKit cache not reachable")
	case C.cutepi_wk_done(job) == 0:
		// WebKit may still answer: leave the job allocated (a small leak on
		// a rare timeout beats freeing memory its reply will write).
		return nil, fmt.Errorf("WebKit cache did not answer within %v", timeout)
	}
	defer C.cutepi_wk_job_free(job)
	if job.error != nil {
		return nil, errors.New(C.GoString(job.error))
	}
	if job.mode == C.CUTEPI_WK_CLEAR_ALL {
		return []string{"(all)"}, nil
	}
	var removed []string
	if job.removed != nil {
		removed = strings.Fields(C.GoString(job.removed))
	}
	return removed, nil
}
