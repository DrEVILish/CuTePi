package routes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
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

// logged reports whether the log viewer holds an entry with code whose
// message contains want.
func logged(code, want string) bool {
	for _, e := range logs.Recorded() {
		if e.Code == code && strings.Contains(e.Message, want) {
			return true
		}
	}
	return false
}

// audited reports whether the audit trail holds event for cue pos.
func audited(event string, pos int) bool {
	for _, a := range logs.AuditTrail() {
		if a.Event == event && a.Pos == pos {
			return true
		}
	}
	return false
}

// A stalled scheduler has failed: a cue that fell due during the stall is
// not fired late; it is recorded as failed (error result, log, audit).
func TestSchedulerStallFailsMissedCue(t *testing.T) {
	logs.Clear()
	due := scheduleFixture(t, "schedstall.wav", 1500*time.Millisecond)
	before := due.Add(-1500 * time.Millisecond)
	schedulerTick(before) // the last pass before the stall: nothing due yet
	time.Sleep(time.Until(due.Add(1300 * time.Millisecond)))
	now := time.Now()
	schedulerStalled(before, now)
	schedulerTick(now)
	played := waitPlaying("schedstall.wav", time.Now().Add(time.Second))
	gsp.Panic()
	if played {
		t.Fatal("a cue missed during a stall was fired late")
	}
	if cue, _ := ctp.GetCue("1"); cue.LastResult != ctp.CueResultError {
		t.Fatalf("missed cue result = %v, want error", cue.LastResult)
	}
	if !logged(logs.SCHStalled, "1 scheduled cue") || !logged(logs.SCHFailed, "stalled") || !audited("schedule_failed", 1) {
		t.Fatalf("stall not recorded: %+v", logs.Recorded())
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

// A scheduled fire that fails is retried once, at once, and the failed
// attempt is logged.
func TestScheduledFireRetriesOnce(t *testing.T) {
	logs.Clear()
	due := scheduleFixture(t, "schedretry.wav", 1200*time.Millisecond)
	failScheduledLoads(t, 1)
	// Armed just after its second: fires at once on the cold-load path
	// (an earlier arm would prewarm and take the warm slot instead).
	time.Sleep(time.Until(due.Truncate(time.Second).Add(100 * time.Millisecond))) // inside its stored second
	schedulerTick(time.Now())
	ok := waitPlaying("schedretry.wav", due.Add(time.Second+150*time.Millisecond))
	var cue ctp.Cue
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cue, _ = ctp.GetCue("1"); cue.LastResult == ctp.CueResultOK {
			break // recorded just after the load returns
		}
	}
	gsp.Panic()
	if !ok || cue.LastResult != ctp.CueResultOK {
		t.Fatalf("the retry did not play within 1s (playing %v, result %v)", ok, cue.LastResult)
	}
	if !logged(logs.SCHFireFailed, "attempt 1") {
		t.Fatalf("first failure not logged: %+v", logs.Recorded())
	}
}

// An operator action after a failed scheduled fire cancels the retry, and
// the cue is recorded as failed.
func TestScheduledRetryYieldsToOperator(t *testing.T) {
	logs.Clear()
	defer func(d time.Duration) { scheduleRetryDelay = d }(scheduleRetryDelay)
	scheduleRetryDelay = time.Second // room for the operator to act first
	due := scheduleFixture(t, "schedyield.wav", 1200*time.Millisecond)
	failScheduledLoads(t, 1)
	time.Sleep(time.Until(due.Truncate(time.Second).Add(100 * time.Millisecond))) // inside its stored second
	schedulerTick(time.Now())
	time.Sleep(300 * time.Millisecond)
	gsp.Stop() // the operator takes over
	if waitPlaying("schedyield.wav", time.Now().Add(2*time.Second)) {
		gsp.Panic()
		t.Fatal("the retry fired after the operator stopped playback")
	}
	if !logged(logs.SCHFailed, "operator") || !audited("schedule_failed", 1) {
		t.Fatalf("yielded cue not recorded as failed: %+v", logs.Recorded())
	}
}

// A retry that fails too is final: no third attempt, the cue has failed.
func TestScheduledSecondFailureIsFinal(t *testing.T) {
	logs.Clear()
	due := scheduleFixture(t, "schedgiveup.wav", 1200*time.Millisecond)
	var mu sync.Mutex
	calls := 0
	t.Cleanup(func() { scheduledLoad = loadCueSource })
	scheduledLoad = func(ctp.Cue, gsp.LoadOpts) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("injected load failure")
	}
	time.Sleep(time.Until(due.Truncate(time.Second).Add(100 * time.Millisecond))) // inside its stored second
	schedulerTick(time.Now())
	time.Sleep(2 * time.Second)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d load attempts, want 2 (one retry)", n)
	}
	if cue, _ := ctp.GetCue("1"); cue.LastResult != ctp.CueResultError {
		t.Fatalf("result = %v, want error", cue.LastResult)
	}
	if !logged(logs.SCHFireFailed, "attempt 2") || !logged(logs.SCHFailed, "both attempts") || !audited("schedule_failed", 1) {
		t.Fatalf("final failure not recorded: %+v", logs.Recorded())
	}
}
