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

// cueOpts must stamp WarmPreroll on every cue: the gsp warm slot compares
// the full opts struct at activation, so an unstamped fire would mismatch a
// Warm-armed slot and silently rebuild cold. Pin the warm-armed == fired
// equality for a representative cue, plus the WarmPreroll field itself.
func TestCueOptsWarmPrerollStamp(t *testing.T) {
	setupTestDB(t)
	if err := ctp.RegisterMedia("warm-opts.wav", 100, media.Metadata{Mimetype: "audio/wav", Duration: 10}, "warm-opts.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("warm-opts.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	opts := cueOpts(cue, false)
	if !opts.WarmPreroll {
		t.Fatalf("cueOpts dropped WarmPreroll; the warm slot would never activate")
	}

	// warm-armed (cueOpts) vs firing (same cueOpts) must be exactly equal.
	if cueOpts(cue, false) != opts {
		t.Fatalf("warm-arm != fire for the same cue: opts unequal")
	}

	// keepBackground (slideshow path) is a DIFFERENT decision — must compare
	// unequal so a warm slot armed without it never mis-activates mid-slide.
	if cueOpts(cue, true) == opts {
		t.Errorf("KeepBackground does not participate in cueOpts equality")
	}
}

// The scheduler's warm path end to end: prewarm goroutine arms the slot
// inside the look-ahead window, the exact-second timer activates it, and a
// slot that loses the race (slow preroll, InstallWarm miss) still fires via
// the plain-fire fallback. Media is audio (headless-env-decodable) so the
// fakesink path isn't required.
func TestSchedulerWarmFire(t *testing.T) {
	setupTestDB(t)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "schedwarm.wav"), buildTinyWav(2), 0o644); err != nil {
		t.Fatalf("write wav fixture: %v", err)
	}
	if err := ctp.RegisterMedia("schedwarm.wav", 100, media.Metadata{Mimetype: "audio/wav", Duration: 2}, "schedwarm.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("schedwarm.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	scheduleFired = make(map[string]int64) // fresh idempotence ledger
	if err := ctp.SetShowMode(true); err != nil {
		t.Fatalf("SetShowMode: %v", err)
	}
	due := time.Now().Add(700 * time.Millisecond)
	day := int(due.Weekday())
	if day == 0 {
		day = 7
	}
	if err := ctp.SetCueSchedule(1, true, day, due.Hour()*3600+due.Minute()*60+due.Second()); err != nil {
		t.Fatalf("SetCueSchedule: %v", err)
	}
	go RunScheduler()
	defer ctp.SetShowMode(false)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		schedulerTick(time.Now()) // drive the arm/fire machinery directly
		if gsp.CurrentPlaying() == "schedwarm.wav" {
			if took := time.Since(due); took > 1500*time.Millisecond {
				t.Errorf("scheduled cue fired %v late — exact-timer path regressed", took.Round(time.Millisecond))
			}
			gsp.Panic()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	gsp.Panic()
	t.Fatal("scheduled warm cue never played")
}
