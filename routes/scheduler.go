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
func RunScheduler() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	imminent, lastCheck := false, time.Time{}
	for now := range ticker.C {
		// GO-button flash (§6.8b): push a sync when a scheduled cue enters
		// (or leaves) its final minute, so clients never poll.
		if now.Sub(lastCheck) >= time.Second {
			lastCheck = now
			if im := scheduleImminent(now); im != imminent {
				imminent = im
				ws.Broadcast()
			}
		}
		if !ctp.GetShowMode() {
			continue
		}
		schedulerTick(now)
	}
}

// scheduleImminent reports whether an armed schedule fires within a minute.
func scheduleImminent(now time.Time) bool {
	if !ctp.GetShowMode() {
		return false
	}
	dueIn, _, _, ok := ctp.NextSchedule(now)
	return ok && dueIn < time.Minute
}

// schedulerTick runs one scheduler iteration at a point in time: query due
// cues and arm/fire them. Split out of the loop so tests can drive ticks
// deterministically instead of racing a real timer.
func schedulerTick(now time.Time) {
	rows, err := ctp.GetScheduledCues(now)
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
		// flush-seek that never re-prerolls stalls activation.
		if strings.HasPrefix(cue.Mimetype, "image/") {
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
	if !(tryWarm && gsp.InstallWarm(cue.Filename, opts)) {
		if err := gsp.LoadWithOpts(cue.Filename, cueOpts(cue, false)); err != nil {
			ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
			log.Printf("CuTePi: failed to fire scheduled cue %d: %v", cue.CuePos, err)
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
