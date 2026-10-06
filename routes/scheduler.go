package routes

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/ws"
)

// scheduleFired tracks which enabled schedules have already been armed
// today, so the scheduler can't double-fire a cue. Keyed "cueID|YYYY-MM-DD"
// (local): cue_id, not the row position, so reordering the sheet mid-day
// can neither re-fire a moved cue nor block a different cue that slid into
// an already-fired position. Reset on date change so it can't grow without
// bound. Guarded by scheduleMu: the ticker arms, timer goroutines un-arm.
var (
	scheduleMu       sync.Mutex
	scheduleFired    = make(map[string]int64)
	scheduleFiredDay = ""
)

// RunScheduler starts the background scheduler that fires enabled schedules
// when their time-of-day arrives, regardless of what else is playing. The
// tick is 200ms and the due window 1s: a cue fires within ~a tick of its
// scheduled second (multi-node sync-fire needs this; a 30s tick can never
// hit 14:31:00). Firing is idempotent per cue+day via scheduleFired.
// ponytail: decision-accurate, not output-accurate — pipeline build takes
// ~100s of ms, so frame-exact multi-node output needs timed pre-roll (v2).
//
// Blocks forever: run it with `go RunScheduler()`. (It used to spawn its
// loop and return, and the deferred ticker.Stop() then fired immediately —
// the loop never received a tick, so scheduled cues never fired.)
func RunScheduler() { runScheduler(nil) }

// runScheduler is RunScheduler's loop; it returns when stop is closed (tests
// stop it so it cannot fire later tests' schedules behind their back).
func runScheduler(stop <-chan struct{}) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	imminent, lastCheck := false, time.Time{}
	// The due-cue query (a join over the sheet) only needs the 200ms
	// cadence near a scheduled time. Elsewhere it runs when something could
	// have changed what is due: a sheet edit, or Show mode switching on
	// (which may arm a cue that fell due in the last second).
	var nextDue time.Time   // zero: nothing scheduled
	var passedDue time.Time // the last nextDue the clock reached
	var prevTick time.Time  // the previous loop pass (stall detection)
	showWas, lastVersion := false, uint64(0)
	for {
		var now time.Time
		select {
		case <-stop:
			return
		case now = <-ticker.C:
		}
		prev := prevTick
		prevTick = now
		show := ctp.GetShowMode()
		if show && !showWas {
			lastCheck = time.Time{} // refresh nextDue now, not up to 1s later
		}
		// GO-button flash (§6.8b): push a sync when a scheduled cue enters
		// (or leaves) its final minute, so clients never poll.
		if now.Sub(lastCheck) >= time.Second {
			lastCheck = now
			if !nextDue.IsZero() && !nextDue.After(now) {
				passedDue = nextDue
			}
			nextDue = time.Time{}
			if show {
				if dueIn, _, _, ok := ctp.NextSchedule(now); ok {
					nextDue = now.Add(dueIn)
				}
			}
			if im := !nextDue.IsZero() && nextDue.Sub(now) < time.Minute; im != imminent {
				imminent = im
				ws.Broadcast()
			}
		}
		if !show {
			showWas = false
			continue
		}
		version := ctp.CuesheetVersion()
		near := (!nextDue.IsZero() && now.After(nextDue.Add(-scheduleLead))) ||
			(!passedDue.IsZero() && now.Before(passedDue.Add(scheduleTrail)))
		if showWas && !prev.IsZero() && now.Sub(prev) > scheduleStall {
			// The loop itself was held up (CPU starvation, a blocked
			// goroutine): cover the whole gap, not just the last second.
			schedulerCatchUp(prev, now)
		} else if near || !showWas || version != lastVersion {
			schedulerTick(now)
		}
		showWas, lastVersion = true, version
	}
}

// scheduleLead and scheduleTrail bound the stretch around a scheduled time
// in which the scheduler queries due cues every tick. GetScheduledCues
// compares whole seconds, so a cue only becomes due once the clock reaches
// its second, and stays due for 1s; by then NextSchedule (refreshed every
// second) may already point at the following schedule. The trail keeps the
// ticks coming through that whole due window.
const (
	scheduleLead  = 2 * time.Second
	scheduleTrail = 1500 * time.Millisecond
)

// scheduleStall is the gap between two scheduler passes (normally 200ms)
// that counts as a stall, and scheduleCatchUpMax the most a stalled pass
// looks back. Only a stall of the scheduler itself reaches back: enabling
// Show mode late or editing a schedule into the past still never replays
// past cues (§6.8b).
const (
	scheduleStall      = time.Second
	scheduleCatchUpMax = time.Minute
)

// schedulerCatchUp runs the pass after a stall: every cue due since the
// previous pass (prev, the last instant the loop saw) fires now, late.
func schedulerCatchUp(prev, now time.Time) {
	from := prev.Add(-time.Second) // the previous pass's own window, overlap is harmless
	if limit := now.Add(-scheduleCatchUpMax); from.Before(limit) {
		from = limit
	}
	log.Printf("CuTePi: scheduler stalled for %v; catching up", now.Sub(prev).Round(time.Millisecond))
	schedulerTickSince(from, now)
}

// schedulerTick runs one scheduler iteration at a point in time: query due
// cues and arm/fire them. Split out of the loop so tests can drive ticks
// deterministically instead of racing a real timer.
func schedulerTick(now time.Time) {
	schedulerTickSince(now.Add(-time.Second), now)
}

// schedulerTickSince arms every cue due after from, up to now.
func schedulerTickSince(from, now time.Time) {
	rows, err := ctp.GetScheduledCuesSince(from, now)
	if err != nil {
		log.Printf("CuTePi: scheduler query failed: %v", err)
		return
	}
	day := now.Format("2006-01-02")
	scheduleMu.Lock()
	defer scheduleMu.Unlock()
	if day != scheduleFiredDay {
		scheduleFired = make(map[string]int64)
		scheduleFiredDay = day
	}
	for _, cue := range rows {
		key := strconv.Itoa(cue.CueID) + "|" + day
		if _, ok := scheduleFired[key]; ok {
			continue
		}
		scheduleFired[key] = now.UnixMilli()
		armScheduled(cue, key, scheduledAt(now, cue.ScheduleMs))
	}
}

// scheduledAt is the wall-clock instant of ms-since-midnight on now's date.
// Built from hour/minute/second fields rather than midnight+duration, which
// lands an hour off on daylight-saving change days.
func scheduledAt(now time.Time, ms int) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(),
		ms/3_600_000, (ms/60_000)%60, (ms/1000)%60, (ms%1000)*int(time.Millisecond), now.Location())
}

// armScheduled fires cue at at: the 200ms tick only ARMS — the fire goes to
// an exact timer at the cue's scheduled second, so timed cues land on the
// clock instead of up to a tick late. A late wake-up (slot already passed)
// fires at once. Inside the look-ahead window the media is prewarmed (any
// type but stills: video warms on fakesink, silent and unpainted) so the
// fire can take the warm slot; a prewarm that has not finished by then
// falls back to the plain build path.
func armScheduled(cue ctp.ScheduleInfo, key string, at time.Time) {
	opts := cueOpts(cue.AsCue(), false)
	d := time.Until(at)
	if d <= 0 {
		goSafe(func() { fireScheduled(cue, key, at, opts, false) })
		return
	}
	goSafe(func() {
		// Stills never warm (see armNextCue): instant EOS plus a
		// flush-seek that never re-prerolls stalls activation. Live pages
		// have no file to preroll.
		if strings.HasPrefix(cue.Mimetype, "image/") || cue.SourceKind == "endpoint" {
			return
		}
		if warmErr := gsp.Warm(cue.Filename, opts); warmErr != nil {
			log.Printf("CuTePi: scheduled cue %d prewarm: %v", cue.CuePos, warmErr)
		}
	})
	time.AfterFunc(d, safe(func() { fireScheduled(cue, key, at, opts, true) }))
}

// fireScheduled plays an armed schedule, re-validating it first: between
// arming and firing the operator may have left Show Mode, disabled or moved
// the schedule, or deleted the cue — none of which may still fire the old
// arm. The cue is re-fetched by cue_id (latest edits; its CURRENT position
// is what gets recorded).
func fireScheduled(armed ctp.ScheduleInfo, key string, at time.Time, opts gsp.LoadOpts, tryWarm bool) {
	cue, err := ctp.GetCueByID(armed.CueID)
	if err != nil || !ctp.GetShowMode() || !cue.ScheduleEnabled || cue.ScheduleTimeMs != armed.ScheduleMs {
		// Disarmed. Forget the arm so an edited schedule later today can
		// arm again at its new time.
		scheduleMu.Lock()
		delete(scheduleFired, key)
		scheduleMu.Unlock()
		return
	}
	// The warm slot was built from the armed snapshot; it only matches if
	// the cue's playback settings are unchanged (InstallWarm compares file
	// and opts), otherwise the fresh cue is built cold.
	if !(tryWarm && cue.SourceKind != "endpoint" && gsp.InstallWarm(cue.Filename, opts)) {
		if err := scheduledLoad(cue, cueOpts(cue, false)); err != nil {
			ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
			log.Printf("CuTePi: failed to fire scheduled cue %d: %v", cue.CuePos, err)
			retryScheduled(armed, key, at, markTransport())
			return
		}
	}
	// Both paths record the cue: the warm path used to skip this, leaving
	// a scheduled cue with no playing-cue association (no highlight, no
	// result, no auto-continue).
	ctp.SetCueResult(cue.CuePos, ctp.CueResultOK)
	gsp.SetCuePos(cue.CuePos)
	gsp.Play()
	if late := time.Since(at); late > time.Second {
		log.Printf("CuTePi: scheduled cue %d fired %v late", cue.CuePos, late.Round(time.Millisecond))
	}
}

// scheduledLoad loads a scheduled cue (a variable so tests can make a fire
// fail on demand).
var scheduledLoad = loadCueSource

// scheduleRetryEvery and scheduleRetryWindow: a scheduled fire that fails
// (decoder or device busy, file briefly unavailable) is tried again every
// second until this long after its scheduled time. It stays marked fired
// for the day, so it can never fire twice.
const scheduleRetryEvery = time.Second

var scheduleRetryWindow = 10 * time.Second // a variable for tests

// retryScheduled tries a failed scheduled fire again after a second, unless
// the operator has acted on the transport since (mark: a cue fired, Stop or
// Panic win over a late retry) or the retry window has passed. fireScheduled
// re-checks Show mode and the schedule, and retries again on failure.
func retryScheduled(armed ctp.ScheduleInfo, key string, at time.Time, mark transportMark) {
	if time.Since(at)+scheduleRetryEvery > scheduleRetryWindow {
		log.Printf("CuTePi: scheduled cue %d: giving up after %v", armed.CuePos, scheduleRetryWindow)
		return
	}
	time.AfterFunc(scheduleRetryEvery, safe(func() {
		if !mark.unchanged() {
			return
		}
		fireScheduled(armed, key, at, cueOpts(armed.AsCue(), false), false)
	}))
}
