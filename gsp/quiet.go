package gsp

import (
	"sync/atomic"
	"time"
)

// Playback quiet (DESIGN §12.14): background work that competes with a show
// for the cores and the GPU (the live-page cache's WebKit preloads) runs only
// while nothing plays, nothing is armed or being built, and that has held a
// while, and stops the moment any of that changes. A WebKit preload under
// way when a cue started made the cue's first seconds stall.

var lastBusy atomic.Int64 // unix ns of the last moment playback was seen busy

// NoteBusy records playback activity now (a cue build starting).
func NoteBusy() { lastBusy.Store(time.Now().UnixNano()) }

// PlaybackQuiet reports that nothing plays, is armed (a warm slot) or is
// shown (test pattern), and that nothing has for at least d.
func PlaybackQuiet(d time.Duration) bool {
	mgr.mu.Lock()
	armed := mgr.warm != nil
	mgr.mu.Unlock()
	if armed || !WallIdle() || len(RunningCues()) > 0 {
		NoteBusy()
		return false
	}
	return time.Since(time.Unix(0, lastBusy.Load())) >= d
}

// BusySince reports playback activity at or after t.
func BusySince(t time.Time) bool {
	return !PlaybackQuiet(0) || lastBusy.Load() >= t.UnixNano()
}
