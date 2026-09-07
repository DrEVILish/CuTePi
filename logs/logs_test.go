package logs

import (
	"regexp"
	"testing"
)

// allCodes lists every exported code constant. Keep this in sync with the
// constants in logs.go. It lets the test verify uniqueness and format without
// reflection over consts.
func allCodes() []string {
	return []string{
		GSPPauseErr, GSPToggleErr, GSPStopErr, GSPPipeDebug, GSPPipeStopped, GSPPadAdded,
		RTEPlay, RTEPlayAlias, RTEPause, RTEToggle, RTEFadeOut, RTEPanic,
		RTEClear, RTEStop, RTETest, RTEDirect, RTELoad,
		RTEAddCue, RTEDelete, RTEDeleteBusy, RTECueNext, RTECuePrev, RTECuePlay,
		RTEUp, RTEDown, RTEUse, RTEEdit, RTEUpdate, RTERemove,
		RTERestart, RTEShutdown,
		YDLRequest, YDLResolve, YDLDownload, YDLProbe, YDLRegister, YDLRender, YDLFailed,
		NETListErr, NETInfoList,
		AUDAudit,
	}
}

var codeRe = regexp.MustCompile(`^[A-Z]{3}-E[0-9]{3}$`)

func TestCodesUniqueAndWellFormed(t *testing.T) {
	codes := allCodes()
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if !codeRe.MatchString(c) {
			t.Errorf("code %q is not well-formed (want <SUBSYS>-E<NNN>)", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
	}
}

func TestRecordedFilterAndClear(t *testing.T) {
	old := level
	defer func() { level = old }()

	level = LevelDebug
	Clear()
	PrintfDebug("DBG-E900", "debug line")
	Printf("DBG-E901", "info line")
	PrintfWarn("DBG-E902", "warn line")
	Emit(AuditEvent{Event: "cue_start", Pos: 1, Title: "intro"})

	got := Recorded()
	if len(got) != 4 {
		t.Fatalf("at debug level want 4 recorded entries, got %d", len(got))
	}
	if got[0].Level != "debug" || got[1].Level != "info" || got[2].Level != "warn" || !got[3].Audit {
		t.Fatalf("debug-level order/kind wrong: %+v", got)
	}

	level = LevelInfo
	Clear()
	Printf("DBG-E901", "info line")
	PrintfWarn("DBG-E902", "warn line")
	Emit(AuditEvent{Event: "cue_start", Pos: 1, Title: "intro"})
	got = Recorded()
	want := []string{"info", "warn"}
	if len(got) != 3 {
		t.Fatalf("at info level want 3 recorded entries (audit always kept), got %d", len(got))
	}
	for i, e := range got[:2] {
		if e.Level != want[i] {
			t.Errorf("entry %d level = %q, want %q", i, e.Level, want[i])
		}
	}
	if !got[2].Audit {
		t.Errorf("audit entry should always be present regardless of level")
	}

	if body := AuditTrail(); len(body) != 2 {
		t.Fatalf("AuditTrail want 2 events (one per Emit so far), got %d", len(body))
	}

	Clear()
	if got := Recorded(); len(got) != 0 {
		t.Fatalf("after Clear want empty viewer buffer, got %d", len(got))
	}
	if body := AuditTrail(); len(body) != 2 {
		t.Fatalf("Clear must not touch the audit trail (second Emit added one), got %d", len(body))
	}
}

// TestAuditTrailCapped is the runnable check for the audit capacity: after
// more than auditCapacity events the trail holds at the cap and keeps the
// NEWEST entries (a runaway trail used to grow for the whole process
// lifetime and get copied on every export).
func TestAuditTrailCapped(t *testing.T) {
	mu.Lock()
	old := audit
	audit = nil
	mu.Unlock()
	defer func() {
		mu.Lock()
		audit = old
		mu.Unlock()
	}()

	for i := 0; i < auditCapacity+500; i++ {
		Emit(AuditEvent{Event: "cue_start", Pos: i, Title: "x"})
	}
	trail := AuditTrail()
	if len(trail) != auditCapacity {
		t.Fatalf("audit trail = %d entries, want capped at %d", len(trail), auditCapacity)
	}
	if last := trail[len(trail)-1]; last.Pos != auditCapacity+499 {
		t.Fatalf("newest entry lost: last pos = %d, want %d", last.Pos, auditCapacity+499)
	}
	if first := trail[0]; first.Pos != 500 {
		t.Fatalf("oldest kept entry = %d, want 500 (oldest rolled off)", first.Pos)
	}
}
