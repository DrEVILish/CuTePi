package ctp

import (
	"bytes"
	"strings"
	"testing"
)

// A show snapshot applied back over a changed show restores it exactly, and
// a cue whose file this unit lacks gets a missing pool entry.
func TestShowSnapshotRoundTrip(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	mustRegisterMedia(t, "snap-a.mp4")
	mustRegisterMedia(t, "snap-b.mp4")
	for _, f := range []string{"snap-a.mp4", "snap-b.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatal(err)
		}
	}
	gid, err := CreateGroup("Snap group", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCueGroup("2", gid); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCue("1", "title", "Opening"); err != nil {
		t.Fatal(err)
	}
	if err := SetGoAdvance(false); err != nil {
		t.Fatal(err)
	}
	if err := SetCue("2"); err != nil {
		t.Fatal(err)
	}
	snap, err := ExportShowSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Change everything, then apply the snapshot.
	if err := UpdateCue("1", "title", "Changed"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCue("2"); err != nil {
		t.Fatal(err)
	}
	if err := SetGoAdvance(true); err != nil {
		t.Fatal(err)
	}
	if err := ApplyShowSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	again, err := ExportShowSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap, again) {
		t.Fatalf("snapshot not restored:\n%s\n%s", snap, again)
	}
	if GetGoAdvance() {
		t.Fatal("GO-advance not replicated")
	}
	if pos, _ := SelectedCuePos(); pos != 2 {
		t.Fatalf("selection = %d, want 2", pos)
	}

	// A file this unit does not have.
	other := bytes.ReplaceAll(snap, []byte(`"snap-b.mp4"`), []byte(`"snap-elsewhere.mp4"`))
	if err := ApplyShowSnapshot(other); err != nil {
		t.Fatal(err)
	}
	var missing bool
	if err := db.Get(&missing, `SELECT missing FROM mediapool WHERE filename = 'snap-elsewhere.mp4'`); err != nil || !missing {
		t.Fatalf("no missing pool entry for the absent file (err %v)", err)
	}
	cue, err := GetCue("2")
	if err != nil || cue.Filename != "snap-elsewhere.mp4" {
		t.Fatalf("cue 2 = %q (%v)", cue.Filename, err)
	}
	if strings.Contains(string(snap), `"last_result"`) || strings.Contains(string(snap), `"media_id"`) {
		t.Fatal("per-unit columns in the snapshot")
	}
}
