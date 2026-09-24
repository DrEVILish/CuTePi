package routes

import (
	"log"
	"strconv"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
)

// scheduleFired tracks which enabled schedules have already fired today,
// so the scheduler can't double-fire a cue that's already been played.
// Keyed "cuePos|YYYY-MM-DD" (local): no arithmetic collisions, and the map
// is reset on date change so it can't grow without bound.
var scheduleFired = make(map[string]int64)
var scheduleFiredDay = ""

// RunScheduler starts the background scheduler that fires enabled schedules
// when their time-of-day arrives, regardless of what else is playing. The
// tick is 200ms and the due window 1s: a cue fires within ~a tick of its
// scheduled second (multi-node sync-fire needs this; a 30s tick can never
// hit 14:31:00). Firing is idempotent per cue+day via scheduleFired.
// ponytail: decision-accurate, not output-accurate — pipeline build takes
// ~100s of ms, so frame-exact multi-node output needs timed pre-roll (v2).
func RunScheduler() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			if !ctp.GetShowMode() {
				continue
			}
			now := time.Now()
			rows, err := ctp.GetScheduledCues(now)
			if err != nil {
				log.Printf("CuTePi: scheduler query failed: %v", err)
				continue
			}
			day := now.Format("2006-01-02")
			if day != scheduleFiredDay {
				scheduleFired = make(map[string]int64)
				scheduleFiredDay = day
			}
			for _, cue := range rows {
				key := strconv.Itoa(cue.CuePos) + "|" + day
				if _, ok := scheduleFired[key]; ok {
					continue
				}
				// Mark armed (not fired): the 200ms tick only ARMS — fires go
				// to an exact timer at the cue's scheduled second (midnight +
				// ms-of-day), so timed cues land on the clock instead of up
				// to a tick late. A late wake-up (slot already passed) fires
				// at once.
				at := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
					Add(time.Duration(cue.ScheduleMs) * time.Millisecond)
				fireScheduled := func() {
					// Re-fetch at fire time (latest edits; the 250ms look-ahead
					// and the warm slot snapshot must not play stale values),
					// then build opts through the ONE shared builder.
					fcue := cue.AsCue()
					if fresh, cerr := ctp.GetCue(strconv.Itoa(cue.CuePos)); cerr == nil {
						fcue = fresh
					}
					if err := gsp.LoadWithOpts(fcue.Filename, cueOpts(fcue, false)); err != nil {
						ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
						log.Printf("CuTePi: failed to fire scheduled cue %d: %v", cue.CuePos, err)
						return
					}
					ctp.SetCueResult(cue.CuePos, ctp.CueResultOK)
					gsp.SetCuePos(cue.CuePos)
					gsp.Play()
				}
				scheduleFired[key] = now.UnixMilli()
				d := time.Until(at)
				switch {
				case d <= 0:
					fireScheduled()
				default:
					// Prewarm inside the look-ahead window (any media type:
					// video warms on fakesink, silent and unpainted) and fire
					// on the exact second via the warm slot. A warm preroll
					// runs on a protected goroutine (must not stall the tick)
					// and might not finish before S — then InstallWarm misses
					// and the fire falls back to the plain build path.
					opts := cueOpts(cue.AsCue(), false)
					goSafe(func() {
						if warmErr := gsp.Warm(cue.Filename, opts); warmErr != nil {
							log.Printf("CuTePi: scheduled cue %d prewarm: %v", cue.CuePos, warmErr)
						}
					})
					time.AfterFunc(d, safe(func() {
						if !gsp.InstallWarm(cue.Filename, opts) {
							fireScheduled()
						}
					}))
				}
			}
		}
	}()
}
