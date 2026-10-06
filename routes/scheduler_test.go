package routes

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go runScheduler(stop)
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

// waitPlaying polls until name plays or the deadline passes.
func waitPlaying(name string, until time.Time) bool {
	for time.Now().Before(until) {
		if gsp.CurrentPlaying() == name {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// A scheduler pass delayed past the normal 1s window must still fire the
// schedule it skipped over (catch-up over the stalled span), while a normal
// pass at the same moment correctly treats it as stale.
func TestSchedulerStallCatchUp(t *testing.T) {
	due := scheduleFixture(t, "schedstall.wav", 1500*time.Millisecond)
	before := due.Add(-1500 * time.Millisecond)
	schedulerTick(before) // the last pass before the stall: nothing due yet
	time.Sleep(time.Until(due.Add(1300 * time.Millisecond)))
	now := time.Now()
	schedulerTick(now) // a plain pass: the schedule is >1s old, stale
	if waitPlaying("schedstall.wav", time.Now().Add(400*time.Millisecond)) {
		gsp.Panic()
		t.Fatal("a plain pass fired a schedule older than its 1s window")
	}
	schedulerCatchUp(before, now) // the stalled pass covers the gap
	ok := waitPlaying("schedstall.wav", time.Now().Add(3*time.Second))
	gsp.Panic()
	if !ok {
		t.Fatal("stalled scheduler never recovered the schedule it skipped")
	}
}

// failScheduledLoads makes the next n scheduled loads fail.
func failScheduledLoads(t *testing.T, n int) {
	t.Helper()
	var mu sync.Mutex
	t.Cleanup(func() { scheduledLoad = loadCueSource })
	scheduledLoad = func(cue ctp.Cue, opts gsp.LoadOpts) error {
		mu.Lock()
		defer mu.Unlock()
		if n > 0 {
			n--
			return errors.New("injected load failure")
		}
		return loadCueSource(cue, opts)
	}
}

// A scheduled fire that fails is retried within its window instead of
// being marked fired for the rest of the day.
func TestScheduledFireRetriesAfterFailure(t *testing.T) {
	due := scheduleFixture(t, "schedretry.wav", 1200*time.Millisecond)
	failScheduledLoads(t, 1)
	// Armed just after its second: fires at once on the cold-load path
	// (an earlier arm would prewarm and take the warm slot instead).
	time.Sleep(time.Until(due.Add(150 * time.Millisecond)))
	schedulerTick(time.Now())
	time.Sleep(250 * time.Millisecond)
	if cue, _ := ctp.GetCue("1"); cue.LastResult != ctp.CueResultError {
		t.Fatalf("first attempt result = %v, want error", cue.LastResult)
	}
	ok := waitPlaying("schedretry.wav", due.Add(5*time.Second))
	var cue ctp.Cue
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cue, _ = ctp.GetCue("1"); cue.LastResult == ctp.CueResultOK {
			break // recorded just after the load returns
		}
	}
	gsp.Panic()
	if !ok || cue.LastResult != ctp.CueResultOK {
		t.Fatalf("failed scheduled fire was not retried (playing %v, result %v)", ok, cue.LastResult)
	}
}

// An operator action after a failed scheduled fire cancels its retries.
func TestScheduledRetryYieldsToOperator(t *testing.T) {
	due := scheduleFixture(t, "schedyield.wav", 1200*time.Millisecond)
	failScheduledLoads(t, 1)
	// Armed just after its second: fires at once on the cold-load path
	// (an earlier arm would prewarm and take the warm slot instead).
	time.Sleep(time.Until(due.Add(150 * time.Millisecond)))
	schedulerTick(time.Now())
	time.Sleep(250 * time.Millisecond)
	if cue, _ := ctp.GetCue("1"); cue.LastResult != ctp.CueResultError {
		t.Fatalf("first attempt result = %v, want error", cue.LastResult)
	}
	gsp.Stop() // the operator takes over
	if waitPlaying("schedyield.wav", due.Add(4*time.Second)) {
		gsp.Panic()
		t.Fatal("a retry fired after the operator stopped playback")
	}
}

// Retries give up once the window has passed.
func TestScheduledRetryWindowEnds(t *testing.T) {
	due := scheduleFixture(t, "schedgiveup.wav", 1200*time.Millisecond)
	defer func(w time.Duration) { scheduleRetryWindow = w }(scheduleRetryWindow)
	scheduleRetryWindow = 3 * time.Second
	failScheduledLoads(t, 1000)
	time.Sleep(time.Until(due.Add(150 * time.Millisecond)))
	schedulerTick(time.Now())
	time.Sleep(time.Until(due.Add(scheduleRetryWindow + 1500*time.Millisecond)))
	calls := 0
	scheduledLoad = func(ctp.Cue, gsp.LoadOpts) error { calls++; return nil }
	time.Sleep(2 * scheduleRetryEvery)
	if calls != 0 {
		t.Fatalf("still retrying %v after the scheduled time", scheduleRetryWindow)
	}
}
