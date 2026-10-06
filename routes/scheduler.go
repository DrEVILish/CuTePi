package routes

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
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
			// goroutine): what fell due in the gap has failed; the
			// current window is still served.
			schedulerStalled(prev, now)
			schedulerTick(now)
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
// that counts as a stall. A stalled scheduler has failed: the cues that fell
// due in the gap are not fired late, they are recorded as failed (§6.8b).
const scheduleStall = time.Second

// schedulerStalled records the cues that fell due while the scheduler was
// stalled (between prev, its last pass, and the 1s window now covers) as
// failed: error result, a warning in the log and an audit record. Cues due
// inside now's own window still fire normally.
func schedulerStalled(prev, now time.Time) {
	gap := now.Sub(prev).Round(time.Millisecond)
	rows, err := ctp.GetScheduledCuesSince(prev.Add(-time.Second), now.Add(-time.Second))
	if err != nil {
		logs.PrintfWarn(logs.SCHStalled, "scheduler stalled for %v; checking for missed cues: %v", gap, err)
		return
	}
	logs.PrintfWarn(logs.SCHStalled, "scheduler stalled for %v; %d scheduled cue(s) fell due in the gap", gap, len(rows))
	day := now.Format("2006-01-02")
	for _, cue := range rows {
		key := strconv.Itoa(cue.CueID) + "|" + day
		scheduleMu.Lock()
		if scheduleFiredDay != day {
			scheduleFired = make(map[string]int64)
			scheduleFiredDay = day
		}
		_, done := scheduleFired[key]
		if !done {
			scheduleFired[key] = now.UnixMilli() // decided: it will not fire today
		}
		scheduleMu.Unlock()
		if !done {
			scheduleFailed(cue.CuePos, cue.Title, fmt.Sprintf("missed while the scheduler was stalled for %v", gap))
		}
	}
}

// scheduleFailed records a scheduled cue that did not fire.
func scheduleFailed(pos int, title, why string) {
	ctp.SetCueResult(pos, ctp.CueResultError)
	logs.PrintfWarn(logs.SCHFailed, "scheduled cue %d %q failed: %s", pos, title, why)
	logs.Emit(logs.AuditEvent{Event: "schedule_failed", Pos: pos, Title: title})
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
		logs.PrintfWarn(logs.SCHQueryFailed, "scheduler query failed: %v", err)
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
			logs.PrintfWarn(logs.SCHPrewarm, "scheduled cue %d prewarm (it will load cold): %v", cue.CuePos, warmErr)
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
	fireScheduledAttempt(armed, key, at, opts, tryWarm, 1)
}

// fireScheduledAttempt is one attempt (1 or 2) at a scheduled fire.
func fireScheduledAttempt(armed ctp.ScheduleInfo, key string, at time.Time, opts gsp.LoadOpts, tryWarm bool, attempt int) {
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
			logs.PrintfWarn(logs.SCHFireFailed, "scheduled cue %d %q, attempt %d: %v", cue.CuePos, cue.Title, attempt, err)
			if attempt == 1 {
				retryScheduled(armed, key, at, markTransport())
			} else {
				scheduleFailed(cue.CuePos, cue.Title, "both attempts failed")
			}
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
		logs.PrintfWarn(logs.SCHLate, "scheduled cue %d %q fired %v late (attempt %d)", cue.CuePos, cue.Title, late.Round(time.Millisecond), attempt)
	}
}

// scheduledLoad loads a scheduled cue (a variable so tests can make a fire
// fail on demand).
var scheduledLoad = loadCueSource

// scheduleRetryDelay: a scheduled fire that fails (device busy, decoder
// error, file briefly unavailable) is tried once more after this pause;
// if that fails too, the cue has failed for the day. A variable for tests.
var scheduleRetryDelay = 250 * time.Millisecond

// retryScheduled makes the one retry of a failed scheduled fire, unless the
// operator has acted on the transport meanwhile (mark: a cue fired, Stop or
// Panic win over the retry; the cue is then recorded as failed).
// fireScheduledAttempt re-checks Show mode and the schedule.
func retryScheduled(armed ctp.ScheduleInfo, key string, at time.Time, mark transportMark) {
	time.AfterFunc(scheduleRetryDelay, safe(func() {
		if !mark.unchanged() {
			scheduleFailed(armed.CuePos, armed.Title, "not retried: the operator took over")
			return
		}
		fireScheduledAttempt(armed, key, at, cueOpts(armed.AsCue(), false), false, 2)
	}))
}
