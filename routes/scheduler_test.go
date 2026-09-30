package routes

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"
)

// scheduleFixture registers one audio cue scheduled ~dueIn from now and
// arms Show Mode. Returns the due instant.
func scheduleFixture(t *testing.T, name string, dueIn time.Duration) time.Time {
	t.Helper()
	setupTestDB(t)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), name), buildTinyWav(2), 0o644); err != nil {
		t.Fatalf("write wav fixture: %v", err)
	}
	if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "audio/wav", Duration: 2}, name); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue(name, ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	scheduleMu.Lock()
	scheduleFired = make(map[string]int64)
	scheduleMu.Unlock()
	if err := ctp.SetShowMode(true); err != nil {
		t.Fatalf("SetShowMode: %v", err)
	}
	t.Cleanup(func() { ctp.SetShowMode(false) })
	due := time.Now().Add(dueIn)
	day := int(due.Weekday())
	if day == 0 {
		day = 7
	}
	if err := ctp.SetCueSchedule(1, true, day, due.Hour()*3600+due.Minute()*60+due.Second()); err != nil {
		t.Fatalf("SetCueSchedule: %v", err)
	}
	return due
}

// RunScheduler must actually tick on its own. It used to start its loop in a
// goroutine and return, and the deferred ticker.Stop() killed the ticker at
// once: in production no scheduled cue ever fired. Unlike TestSchedulerWarmFire
// this drives NO manual ticks — only RunScheduler's own loop.
func TestRunSchedulerTicksOnItsOwn(t *testing.T) {
	due := scheduleFixture(t, "schedself.wav", 1500*time.Millisecond)
	go RunScheduler()
	deadline := due.Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if gsp.CurrentPlaying() == "schedself.wav" {
			gsp.Panic()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	gsp.Panic()
	t.Fatal("RunScheduler never fired the scheduled cue on its own")
}

// A fired scheduled cue must record its cue association and result on every
// path — the warm-slot path used to skip both.
func TestScheduledFireRecordsCue(t *testing.T) {
	due := scheduleFixture(t, "schedrec.wav", 900*time.Millisecond)
	deadline := due.Add(4 * time.Second)
	for time.Now().Before(deadline) {
		schedulerTick(time.Now())
		if gsp.CurrentPlaying() == "schedrec.wav" {
			// SetCuePos follows the load; give the fire goroutine a moment.
			time.Sleep(100 * time.Millisecond)
			pos := gsp.CurrentCuePos()
			cue, err := ctp.GetCue("1")
			gsp.Panic()
			if pos != 1 {
				t.Fatalf("scheduled fire left cuePos=%d, want 1", pos)
			}
			if err != nil || cue.LastResult != ctp.CueResultOK {
				t.Fatalf("scheduled fire did not record OK result: %v %+v", err, cue.LastResult)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	gsp.Panic()
	t.Fatal("scheduled cue never played")
}

// Leaving Show Mode after a schedule is armed must cancel the fire.
func TestScheduledFireCancelledByShowModeOff(t *testing.T) {
	due := scheduleFixture(t, "schedoff.wav", 1200*time.Millisecond)
	schedulerTick(due.Add(-200 * time.Millisecond)) // arm inside the look-ahead window
	if err := ctp.SetShowMode(false); err != nil {
		t.Fatalf("SetShowMode: %v", err)
	}
	time.Sleep(time.Until(due) + 700*time.Millisecond)
	playing := gsp.CurrentPlaying()
	gsp.Panic()
	if playing == "schedoff.wav" {
		t.Fatal("armed schedule fired after Show Mode was turned off")
	}
}

// scheduledAt must build the wall-clock time from fields, not midnight+ms
// (an hour off on daylight-saving change days).
func TestScheduledAtDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026-03-29: clocks go forward 01:00 -> 02:00 in London.
	now := time.Date(2026, 3, 29, 9, 0, 0, 0, loc)
	at := scheduledAt(now, (14*3600+30*60)*1000)
	if at.Hour() != 14 || at.Minute() != 30 {
		t.Fatalf("scheduledAt on a DST day = %v, want 14:30 local", at)
	}
}
