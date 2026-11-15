package ctp

import "testing"

// A malformed persisted selection must fail export rather than silently
// replacing the operator's selection with zero in a successful archive.
func TestShowExportsRejectMalformedSelection(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	mustRegisterMedia(t, "snapshot-selection.mp4")
	if err := AddCue("snapshot-selection.mp4", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, stateKeySelectedCue, "broken"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedCue); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := ExportCues(); err == nil {
		t.Fatal("cue export accepted a malformed persisted selection")
	}
	if _, err := ExportShow(); err == nil {
		t.Fatal("show export accepted a malformed persisted selection")
	}
}
