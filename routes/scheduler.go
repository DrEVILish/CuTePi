package routes

import (
	"log"
	"strconv"
	"strings"
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
				scheduleFired[key] = now.UnixMilli()
				gsp.Stop()
				opts := gsp.LoadOpts{
					InPoint:      float64(cue.PosStart) / 1000,
					OutPoint:     float64(cue.PosEnd) / 1000,
					Hold:         cue.Hold && (strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "image/")),
					Loop:         cue.Loop,
					LoopCount:    cue.LoopCount,
					Volume:       cue.Volume,
					LoudnessGain: cue.LoudnessGain,
					Rate:         cue.Rate,
					Balance:      cue.Balance,
					Mute:         cue.Mute,
					FadeIn:       cue.FadeIn,
					FadeCurve:    cue.FadeCurve,
					FitMode:      cue.FitMode,
					Rotation:     cue.Rotation,
					Flip:         cue.Flip,
				}
				if err := gsp.LoadWithOpts(cue.Filename, opts); err != nil {
					ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
					log.Printf("CuTePi: failed to fire scheduled cue %d: %v", cue.CuePos, err)
					continue
				}
				ctp.SetCueResult(cue.CuePos, ctp.CueResultOK)
				gsp.SetCuePos(cue.CuePos)
				gsp.Play()
			}
		}
	}()
}