package ctp

import (
	"testing"
	"time"
)

// A schedule due now must come back from GetScheduledCues with its filename
// and geometry. Regression: the query selected c.filename (no such column),
// so the scheduler errored every tick and never fired anything.
func TestGetScheduledCuesRoundTrip(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "sched.mp4")
	if err := AddCue("sched.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	now := time.Now()
	day := int(now.Weekday())
	if day == 0 {
		day = 7
	}
	// Due = this exact second (within the scheduler's 1s due window). A
	// schedule from seconds or hours ago must NOT come back: enabling Show
	// mode late used to fire the whole day's past cues at once, and
	// minute-rounded times can never hit an exact-second sync-fire.
	thisSec := now.Hour()*3600 + now.Minute()*60 + now.Second()
	if err := SetCueSchedule(1, true, day, thisSec); err != nil {
		t.Fatalf("SetCueSchedule: %v", err)
	}
	if err := UpdateCueFields("1", map[string]string{"fit_mode": "stretch", "rotation": "90", "flip": "h"}); err != nil {
		t.Fatalf("UpdateCueFields geometry: %v", err)
	}
	rows, err := GetScheduledCues(now)
	if err != nil {
		t.Fatalf("GetScheduledCues: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 due cue, got %d", len(rows))
	}
	got := rows[0]
	if got.Filename != "sched.mp4" {
		t.Fatalf("filename = %q, want sched.mp4", got.Filename)
	}
	if got.FitMode != "stretch" || got.Rotation != 90 || got.Flip != "h" {
		t.Fatalf("geometry = %+v, want stretch/90/h", got)
	}
	// A schedule from five seconds ago is already stale, not due: the
	// due window is 1s (one missed tick + jitter), not minutes. (Skipped
	// just after midnight, where "5s ago" is yesterday.)
	if thisSec < 10 {
		t.Skip("too close to midnight for the staleness checks")
	}
	if err := SetCueSchedule(1, true, day, thisSec-5); err != nil {
		t.Fatalf("SetCueSchedule recent: %v", err)
	}
	rows, err = GetScheduledCues(now)
	if err != nil {
		t.Fatalf("GetScheduledCues recent: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 due cues for a 5s-old schedule, got %d", len(rows))
	}
	// A schedule from two hours ago is stale, not due. (Skipped just
	// after midnight, where "2h ago" would fall on the previous day.)
	if thisSec < 7205 {
		t.Skip("too close to midnight for the stale-schedule check")
	}
	if err := SetCueSchedule(1, true, day, thisSec-7200); err != nil {
		t.Fatalf("SetCueSchedule stale: %v", err)
	}
	rows, err = GetScheduledCues(now)
	if err != nil {
		t.Fatalf("GetScheduledCues stale: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 due cues for a 2h-old schedule, got %d", len(rows))
	}
	// Out-of-range values never reach the DB (day=0 used to panic the
	// server with 1<<-1; day=8 stored a bit no reader matches).
	for _, tc := range [][2]int{{0, thisSec}, {8, thisSec}, {day, -1}, {day, 86400}} {
		if err := SetCueSchedule(1, true, tc[0], tc[1]); err == nil {
			t.Fatalf("SetCueSchedule(%d, %d): want error, got nil", tc[0], tc[1])
		}
	}
}
