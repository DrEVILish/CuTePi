package routes

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// Cue standby (DESIGN §12.14, gsp/livestandby.go, gsp.Warm): what GO fires
// next is loaded ahead of it, so GO shows it at once.
//
//   - Live pages: every live cue in the sheet stands by (the selected cue's
//     and the next cue's first, then the rest in sheet order, up to
//     gsp.liveStandbyMax), from start-up on, so GO finds its page loaded
//     however briefly the operator highlighted it; a cue already running is
//     not stood by again.
//   - Files: the selected cue (GO's) is armed in gsp's warm slot, its first
//     frame on a hidden plane, once the selection has rested for
//     fileWarmSettle (stepping through the sheet does not decode every row),
//     then the next cue in the spare slot (gsp.WarmNext), so GO, GO finds
//     both armed. A playing cue is armed as well (GO on it re-fires a copy).
//
// Nothing new starts while a fired cue is starting, nor within
// liveStandbyAfterFire (pages) or fileWarmAfterFire (files) of it: WebKit's
// start-up or a second decode beside a cue starting made the cue stall (as
// with armNextCue).
const (
	liveStandbyTick = 100 * time.Millisecond
	fileWarmSettle  = 150 * time.Millisecond
	// liveStandbyAfterFire matches armNextCue's wait (1.2 s) plus margin.
	liveStandbyAfterFire = 1500 * time.Millisecond
	// fileWarmAfterFire: a file arm is one decode, not a WebKit start-up.
	fileWarmAfterFire = 500 * time.Millisecond
)

// RunLiveStandby runs the keeper (call once, in its own goroutine).
func RunLiveStandby() {
	pages := gsp.PageCacheAvailable() // no WebKit: files only
	// No liveCacheStartDelay here: that guards in-process WebKit GL against
	// the wall's start-up, and standby pages render out of process (the wall
	// is open before this starts). The pages start loading with the service:
	// a GO in the first seconds after a restart waits for its page's load.
	var (
		selPos, gen        = -1, gsp.Generation()
		selSince, genSince = time.Now(), time.Now()
		retryAt            time.Time
		sheetVer           = ^uint64(0)
		livePos            []int
		warming            atomic.Bool
		failedAt           sync.Map // file name -> time.Time: a file that failed to arm waits a minute
	)
	for {
		time.Sleep(liveStandbyTick)
		now := time.Now()
		if g := gsp.Generation(); g != gen || gsp.Starting() || gsp.LastFire().After(genSince) {
			gen, genSince = g, now // a fire counts until its cue has started (a cold load prerolls first)
		}
		pos, err := ctp.SelectedCuePos()
		if err != nil {
			continue
		}
		if pos != selPos {
			selPos, selSince = pos, now
		}
		running := gsp.RunningCues()
		build := now.Sub(genSince) >= liveStandbyAfterFire
		if pages {
			if v := ctp.CuesheetVersion(); v != sheetVer {
				sheetVer, livePos = v, liveCuePositions()
			}
			var wants []gsp.LiveStandbyWant
			seen := map[int]bool{}
			for _, p := range append(liveStandbyPositions(pos), livePos...) {
				if seen[p] {
					continue
				}
				seen[p] = true
				if cue, ok := liveStandbyCue(p, running); ok {
					wants = append(wants, gsp.LiveStandbyWant{Title: liveLabel(cue), URL: cue.EndpointURL, Opts: cueOpts(cue, false)})
				}
			}
			if err := gsp.StandbyLive(wants, build && now.After(retryAt)); err != nil {
				logs.Printf(logs.GSPWarm, "live page standby: %v", err)
				retryAt = now.Add(time.Minute) // a failing page: try again in a minute, not every tick
			}
		}
		if now.Sub(genSince) >= fileWarmAfterFire && now.Sub(selSince) >= fileWarmSettle && !warming.Load() {
			// The selected cue in the slot (taken from the spare when it was
			// armed as the next cue), then the next cue in the spare.
			arm, next := gsp.Warm, false
			cue, ok := fileWarmCue(pos)
			if ok && gsp.IsWarmSlot(cue.Filename, cueOpts(cue, false)) {
				ok = false
				if n, err := ctp.NextCuePos(pos); err == nil && n > 0 && n != pos {
					cue, ok = fileWarmCue(n)
					arm, next = gsp.WarmNext, true
				}
			}
			if t, failed := failedAt.Load(cue.Filename); ok && failed && now.Sub(t.(time.Time)) < time.Minute {
				ok = false
			}
			if ok && (!next || !gsp.IsWarm(cue.Filename, cueOpts(cue, false))) {
				warming.Store(true)
				goSafe(func() {
					defer warming.Store(false)
					t0, opts := time.Now(), cueOpts(cue, false)
					if err := arm(cue.Filename, opts); err != nil {
						logs.Printf(logs.GSPWarm, "prewarm %q: %v", cue.Filename, err)
						failedAt.Store(cue.Filename, time.Now())
					} else if gsp.IsWarm(cue.Filename, opts) {
						what := "selected"
						if next {
							what = "next"
						}
						logs.Printf(logs.GSPWarm, "%s cue armed: %q in %.0fms", what, cue.Filename, time.Since(t0).Seconds()*1000)
					}
				})
			}
		}
	}
}

// liveStandbyPositions are the cues GO fires next: the selected one, then the
// one after it.
func liveStandbyPositions(sel int) []int {
	if sel <= 0 {
		return nil
	}
	out := []int{sel}
	if next, err := ctp.NextCuePos(sel); err == nil && next > 0 && next != sel {
		out = append(out, next)
	}
	return out
}

// liveCuePositions lists the sheet's live cues in sheet order.
func liveCuePositions() []int {
	cs, err := ctp.GetCuesheet()
	if err != nil {
		return nil
	}
	var out []int
	for _, c := range cs.Cues {
		if c.SourceKind == "endpoint" {
			out = append(out, c.CuePos)
		}
	}
	return out
}

// liveStandbyCue is the cue at pos when it is a live page that is not
// running.
func liveStandbyCue(pos int, running []int) (ctp.Cue, bool) {
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil || cue.SourceKind != "endpoint" || cue.EndpointURL == "" || slices.Contains(running, pos) {
		return ctp.Cue{}, false
	}
	return cue, true
}

// fileWarmCue is the cue at pos when it is a media file. One that is
// playing is armed too: a GO on it starts a fresh copy (a re-fire).
func fileWarmCue(pos int) (ctp.Cue, bool) {
	if pos <= 0 {
		return ctp.Cue{}, false
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil || cue.SourceKind == "endpoint" || cue.Filename == "" {
		return ctp.Cue{}, false
	}
	return cue, true
}
