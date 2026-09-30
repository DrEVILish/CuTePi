package gsp

// Panic holding image on standby (§12.9). A cold cut to the holding image
// builds and decodes it after the panic (~330 ms on the Pi 4). Instead the
// image is kept armed: built, prerolled and presented on its own display
// plane at alpha 0, outside the visible stack. A panic raises that plane to
// the top at full alpha (two display commits), mutes the running audio, and
// only then retires everything else underneath and adopts the standby as
// the transport — exactly what the cold path would have left playing.
//
// KMS wall only (the plane is what makes it free to keep the frame up);
// stills only. The standby never gets a bus watch (a bus takes one, and the
// activation's watchAndPlay needs it), so it is health-checked when used.

import (
	"fmt"
	"os"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/logs"

	"github.com/go-gst/go-gst/gst"
)

var standby struct {
	mu    sync.Mutex
	p     *gst.Pipeline
	file  string
	stamp string // size and mtime when armed: a replaced file re-arms
}

// armMu serialises ArmPanicHold (the reconciler and a post-panic re-arm).
var armMu sync.Mutex

func panicHoldOpts() LoadOpts { return LoadOpts{Hold: true} }

// fileStamp identifies the file's content version ("" when unreadable).
func fileStamp(file string) string {
	fi, err := os.Stat(config.MediaLocation() + "/" + file)
	if err != nil || fi.IsDir() {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

// ArmPanicHold keeps file armed as the panic picture, or disarms when file
// is "" or can't be armed (no KMS wall, not a still, unreadable). Idempotent:
// an armed, unchanged file is left as is, so it is cheap to call often.
func ArmPanicHold(file string) {
	armMu.Lock()
	defer armMu.Unlock()
	stamp := ""
	if file != "" && Layered() && isStillFile(file) {
		stamp = fileStamp(file)
	}
	standby.mu.Lock()
	if stamp != "" && standby.p != nil && standby.file == file && standby.stamp == stamp {
		standby.mu.Unlock()
		return
	}
	old := standby.p
	standby.p, standby.file, standby.stamp = nil, "", ""
	standby.mu.Unlock()
	retireAll(old)
	if stamp == "" {
		return
	}
	gstInit()
	t0 := time.Now()
	p, err := buildPipeline(pipelineSpec{filename: file, opts: panicHoldOpts()})
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: arming panic image %q: %v", file, err)
		return
	}
	// PAUSED prerolls the frame onto the plane; the plane stays at alpha 0.
	p.SetState(gst.StatePaused)
	if result, _ := p.GetState(gst.StateNull, gst.ClockTime(5*time.Second)); result != gst.StateChangeSuccess {
		logs.Printf(logs.GSPPipeDebug, "gsp: arming panic image %q: preroll %v", file, result)
		retirePipeline(p)
		return
	}
	standby.mu.Lock()
	standby.p, standby.file, standby.stamp = p, file, stamp
	standby.mu.Unlock()
	logs.Printf(logs.GSPWarm, "panic image armed: %s (%.0f ms)", file, time.Since(t0).Seconds()*1000)
}

// PanicHoldArmed reports the armed file ("" when none).
func PanicHoldArmed() string {
	standby.mu.Lock()
	defer standby.mu.Unlock()
	if standby.p == nil {
		return ""
	}
	return standby.file
}

// PanicToHold cuts to the armed holding image. False when file isn't armed
// (or the file changed since): the caller falls back to a cold load.
func PanicToHold(file string) bool {
	t0 := time.Now()
	standby.mu.Lock()
	p := standby.p
	ok := p != nil && standby.file == file && standby.stamp == fileStamp(file)
	if ok {
		standby.p, standby.file, standby.stamp = nil, "", ""
	}
	standby.mu.Unlock()
	if !ok {
		return false
	}
	if _, state := p.GetState(gst.StateNull, 0); state != gst.StatePaused || layerOf(p) == nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: armed panic image not ready (%v): cold load", state)
		retirePipeline(p)
		return false
	}
	// 1. Picture first: the armed plane goes above everything, fully on.
	raiseLayer(p)
	shown := time.Since(t0)
	// 2. Silence what was playing, before the (slower) teardown.
	mgr.mu.Lock()
	mgr.halts++
	if mgr.volumeEl != nil {
		mgr.volumeEl.Set("volume", 0.0)
	}
	mgr.mu.Unlock()
	stopBackground()
	retireOutgoing()
	// 3. Retire the rest underneath and adopt the standby as the transport.
	mgr.swap(p, file, panicHoldOpts())
	if err := watchAndPlay(p); err != nil {
		logs.Printf(logs.GSPPipeStopped, "gsp: panic image failed to start: %v", err)
	}
	broadcastSoon()
	logs.Printf(logs.GSPFireTiming, "panic cut to armed image %q: on screen %.1f ms, done %.1f ms",
		file, shown.Seconds()*1000, time.Since(t0).Seconds()*1000)
	return true
}
