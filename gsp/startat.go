package gsp

import (
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// Timed start (DESIGN §6.8c): a cue fired with a start instant shows its
// first frame (and starts its sound) at that wall-clock instant, not when its
// pipeline happened to finish building. The pipeline is pinned to the system
// clock and its base time set so running time 0 falls on the instant: every
// node of a multi-Pi show fired for the same instant then starts together, to
// the accuracy of their clock sync (NTP/PTP) plus at most one refresh (HDMI
// outputs are not genlocked, so each shows the frame at its own next vblank).

var (
	startAtMu  sync.Mutex
	startAt    time.Time // pending instant for the next loaded cue's start
	startAtGen uint64    // the load generation when it was armed
)

// MinStartLead is how far ahead a start instant must be when it is armed;
// at PLAYING half of it must be left. The prerolled first frame waits
// hidden on its plane and the warm slot starts in 1-3 ms (§6.1), so this
// covers only the PLAYING transition, not a load (was 150 ms before the
// warm slot waited on its own plane).
const MinStartLead = 10 * time.Millisecond

// StartAt arms a timed start for the cue loaded next: arm it, then load and
// play the cue. Only a pipeline loaded after the arm takes it (not whatever
// was on the wall, e.g. a panic image resuming). A zero or too-close instant
// clears it.
func StartAt(t time.Time) {
	g := Generation()
	startAtMu.Lock()
	defer startAtMu.Unlock()
	if t.IsZero() || time.Until(t) < MinStartLead {
		startAt = time.Time{}
		return
	}
	startAt, startAtGen = t, g
}

// takeStartAt returns and clears the armed instant when p is the focus and
// was loaded after the arm.
func takeStartAt(p *gst.Pipeline) time.Time {
	mgr.mu.Lock()
	ok := mgr.pipeline == p
	gen := mgr.gen
	mgr.mu.Unlock()
	startAtMu.Lock()
	defer startAtMu.Unlock()
	if !ok || startAt.IsZero() || gen == startAtGen {
		return time.Time{}
	}
	t := startAt
	startAt = time.Time{}
	return t
}

// peekStartAt is the instant takeStartAt would return for p, left armed.
func peekStartAt(p *gst.Pipeline) time.Time {
	mgr.mu.Lock()
	ok := mgr.pipeline == p
	gen := mgr.gen
	mgr.mu.Unlock()
	startAtMu.Lock()
	defer startAtMu.Unlock()
	if !ok || gen == startAtGen {
		return time.Time{}
	}
	return startAt
}

// showAt puts p's layer up at its start instant t (now if t has passed)
// and releases the cues it replaces; false when p stopped meanwhile. The
// level is written one refresh ahead: the presenter builds each commit
// during the refresh before it latches, so a write at t landed a refresh
// after the first frame's own commit.
func showAt(p *gst.Pipeline, gen uint64, t time.Time, level float64, at string, ref *gst.Pipeline) bool {
	if w := kmsWall(); w != nil && w.Refresh > 0 {
		t = t.Add(-time.Second / time.Duration(w.Refresh))
	}
	time.Sleep(time.Until(t))
	mgr.mu.Lock()
	ok := mgr.startCurrentLocked(p, gen)
	mgr.mu.Unlock()
	if !ok {
		incomingShown(p) // never leave a crossfade waiting
		return false
	}
	showLayer(p, level, at, ref)
	incomingOnScreen(p)
	return true
}

// applyStartAt pins p's timing so its running time 0 is rendered at t. Call
// with p PAUSED (prerolled, trim seek done), just before PLAYING.
func applyStartAt(p *gst.Pipeline, t time.Time) bool {
	if time.Until(t) < MinStartLead/2 {
		logs.PrintfWarn(logs.GSPFireTiming, "timed start: instant already passed (%v late); starting now", -time.Until(t).Round(time.Millisecond))
		return false
	}
	glwall.UseSystemClock(p) // a clock the instant can be expressed in (not an audio device's)
	clk := gst.ObtainSystemClock()
	now := clk.GetTime()
	lead := time.Until(t) // measured after reading the clock: errs a few µs early
	latency := pipelineLatency(p)
	base := uint64(now) + uint64(lead) - uint64(latency)
	p.SetStartTime(gst.ClockTimeNone) // keep the base time set here
	p.SetBaseTime(gst.ClockTime(base))
	logs.Printf(logs.GSPFireTiming, "timed start: first frame at %d ms (unix), due in %.1f ms (latency %v)", t.UnixMilli(), float64(lead)/1e6, latency)
	return true
}

// pwFirstFrameMax bounds GO to the first frame on the presenter.
const pwFirstFrameMax = 25 * time.Millisecond

// dueTime is when p's running time 0 is rendered (call in PLAYING).
func dueTime(p *gst.Pipeline) time.Time {
	clk := p.GetPipelineClock()
	if clk == nil {
		return time.Now()
	}
	return time.Now().Add(time.Duration(int64(p.GetBaseTime()) + int64(pipelineLatency(p)) - int64(clk.GetTime())))
}

// pipelineLatency is the latency the pipeline adds before running time 0 is
// rendered (its "latency" property where set, e.g. the presenter's start
// latency; 0 otherwise: these pipelines have no live source).
func pipelineLatency(p *gst.Pipeline) time.Duration {
	v, err := p.GetProperty("latency")
	if err != nil {
		return 0
	}
	if l, ok := v.(uint64); ok && l != uint64(gst.ClockTimeNone) {
		return time.Duration(l)
	}
	return 0
}
