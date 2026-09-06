package ctp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/media"
)

func TestMain(m *testing.M) {
	config.SetDbLocation(":memory:")
	if err := InitDB(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func mustRegisterMedia(t *testing.T, filename string) {
	t.Helper()
	if err := RegisterMedia(filename, 1234, media.Metadata{
		Mimetype:   "video/mp4",
		Duration:   10,
		Resolution: "1920x1080",
		Codec:      "h264",
	}, filename); err != nil {
		t.Fatalf("RegisterMedia(%q): %v", filename, err)
	}
}

func TestRegisterAndGetMediapool(t *testing.T) {
	mustRegisterMedia(t, "test-a.mp4")
	mustRegisterMedia(t, "test-b.mp4")

	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	if len(pool.Medias) < 2 {
		t.Fatalf("expected at least 2 media items, got %d", len(pool.Medias))
	}
}

func TestCueNavigation(t *testing.T) {
	mustRegisterMedia(t, "nav-1.mp4")
	mustRegisterMedia(t, "nav-2.mp4")
	mustRegisterMedia(t, "nav-3.mp4")

	if err := AddCue("nav-1.mp4", ""); err != nil {
		t.Fatalf("AddCue 1: %v", err)
	}
	if err := AddCue("nav-2.mp4", ""); err != nil {
		t.Fatalf("AddCue 2: %v", err)
	}
	if err := AddCue("nav-3.mp4", ""); err != nil {
		t.Fatalf("AddCue 3: %v", err)
	}

	if err := SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}

	if err := NextCue(); err != nil {
		t.Fatalf("NextCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 2 {
		t.Fatalf("expected SelectedCuePos=2 after NextCue, got %d", got)
	}

	if err := NextCue(); err != nil {
		t.Fatalf("NextCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 3 {
		t.Fatalf("expected SelectedCuePos=3 after second NextCue, got %d", got)
	}

	// Already at the last cue - NextCue must not advance past cuesheetLength.
	if err := NextCue(); err != nil {
		t.Fatalf("NextCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 3 {
		t.Fatalf("expected SelectedCuePos to stay at 3, got %d", got)
	}

	if err := PrevCue(); err != nil {
		t.Fatalf("PrevCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 2 {
		t.Fatalf("expected SelectedCuePos=2 after PrevCue, got %d", got)
	}

	if err := SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	version := CuesheetVersion()
	if err := SetCue("2"); err != nil {
		t.Fatalf("SetCue for sync: %v", err)
	}
	if CuesheetVersion() <= version {
		t.Fatalf("cue selection change did not advance the sync version")
	}
	if err := PrevCue(); err != nil {
		t.Fatalf("PrevCue: %v", err)
	}
	// Regression test: PrevCue used to have an off-by-one that blocked
	// navigating down to cue 1.
	if got, _ := SelectedCuePos(); got != 1 {
		t.Fatalf("expected SelectedCuePos to stay at 1 (can't go below 1), got %d", got)
	}
}

func TestUpdateCueRejectsUnknownColumn(t *testing.T) {
	mustRegisterMedia(t, "edit-1.mp4")
	if err := AddCue("edit-1.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	// This is the regression test for the SQL-injection-shaped bug: column
	// names must be allow-listed rather than concatenated verbatim.
	if err := UpdateCue("1", "cuePos = 999; --", "x"); err == nil {
		t.Fatalf("expected UpdateCue to reject a non-allow-listed column")
	}

	if err := UpdateCue("1", "title", "New Title"); err != nil {
		t.Fatalf("UpdateCue(title): %v", err)
	}
	cue, err := GetCue("1")
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if cue.Title != "New Title" {
		t.Fatalf("expected title to be updated, got %q", cue.Title)
	}
}

// Regression test: the inline cell-edit form used to be pre-filled with
// the entire stringified Cue struct instead of the value of the specific
// column being edited.
func TestCueColumnValue(t *testing.T) {
	mustRegisterMedia(t, "colval-1.mp4")
	if err := AddCue("colval-1.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := UpdateCue("1", "title", "Column Value Test"); err != nil {
		t.Fatalf("UpdateCue: %v", err)
	}
	cue, err := GetCue("1")
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}

	val, err := CueColumnValue(cue, "title")
	if err != nil {
		t.Fatalf("CueColumnValue(title): %v", err)
	}
	if val != "Column Value Test" {
		t.Fatalf("CueColumnValue(title) = %q, want the current title, not a stringified struct", val)
	}

	// duration is now an editable column; verify it returns formatted time
	val, err = CueColumnValue(cue, "cueDuration")
	if err != nil {
		t.Fatalf("CueColumnValue(cueDuration): %v", err)
	}
	if val != "00:00:10.000" {
		t.Fatalf("CueColumnValue(cueDuration) = %q, want media duration", val)
	}

	if _, err := CueColumnValue(cue, "nonExistentColumn"); err == nil {
		t.Fatalf("expected CueColumnValue to reject a non-editable column")
	}
}

func TestCueDurationUsesTrimWindow(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "duration-trim.mp4")
	if err := AddCue("duration-trim.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := UpdateCue("1", "posStart", "2"); err != nil {
		t.Fatalf("UpdateCue(posStart): %v", err)
	}
	if err := UpdateCue("1", "posEnd", "5"); err != nil {
		t.Fatalf("UpdateCue(posEnd): %v", err)
	}
	cue, err := GetCue("1")
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if cue.CueDurationFmt != "00:00:03.000" {
		t.Fatalf("trimmed duration = %q, want 3 seconds", cue.CueDurationFmt)
	}
}

func TestAddCueAllowsRepeatedMedia(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "repeat-cue.mp4")
	if err := AddCue("repeat-cue.mp4", ""); err != nil {
		t.Fatalf("first AddCue: %v", err)
	}
	if err := AddCue("repeat-cue.mp4", ""); err != nil {
		t.Fatalf("second AddCue: %v", err)
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Cues) != 2 || sheet.Cues[0].Title == sheet.Cues[1].Title {
		t.Fatalf("repeated media should create distinct cues, got %+v", sheet.Cues)
	}
}

// Regression test: mediapool listings must be sorted newest-first, per
// spec ("Should the media table include a date added field? Yes, sort
// newest first").
func TestGetMediapoolSortedNewestFirst(t *testing.T) {
	mustRegisterMedia(t, "order-older.mp4")
	time.Sleep(10 * time.Millisecond)
	mustRegisterMedia(t, "order-newer.mp4")

	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}

	var olderIdx, newerIdx = -1, -1
	for i, m := range pool.Medias {
		switch m.Filename {
		case "order-older.mp4":
			olderIdx = i
		case "order-newer.mp4":
			newerIdx = i
		}
	}
	if olderIdx == -1 || newerIdx == -1 {
		t.Fatalf("expected both files in mediapool, got %+v", pool.Medias)
	}
	if newerIdx > olderIdx {
		t.Fatalf("expected newer file (index %d) to sort before older file (index %d)", newerIdx, olderIdx)
	}
}

func TestRegisterMediaRejectsDuplicateFilename(t *testing.T) {
	mustRegisterMedia(t, "dup.mp4")
	err := RegisterMedia("dup.mp4", 1, media.Metadata{Codec: "h264"}, "dup")
	if err == nil {
		t.Fatalf("expected registering a duplicate filename to fail")
	}
}

func TestThumbnailPendingWorkflow(t *testing.T) {
	mustRegisterMedia(t, "thumb-1.mp4")

	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	var mediaID int
	for _, m := range pool.Medias {
		if m.Filename == "thumb-1.mp4" {
			mediaID = m.Media_id
			if !m.ThumbnailPending {
				t.Fatalf("expected a freshly registered file to have thumbnail_pending=true")
			}
			if !m.WaveformPending {
				t.Fatalf("expected a freshly registered file to have waveform_pending=true")
			}
		}
	}
	if mediaID == 0 {
		t.Fatalf("could not find registered media in pool")
	}

	pending, err := PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if !containsFilename(pending, "thumb-1.mp4") {
		t.Fatalf("expected thumb-1.mp4 to be in the pending list")
	}

	if err := MarkThumbnailDone(mediaID); err != nil {
		t.Fatalf("MarkThumbnailDone: %v", err)
	}
	pending, err = PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if !containsFilename(pending, "thumb-1.mp4") {
		t.Fatalf("expected thumb-1.mp4 to stay pending while waveform analysis is still due")
	}

	if err := StoreWaveform(mediaID, `[0.1,0.2,0.3]`); err != nil {
		t.Fatalf("StoreWaveform: %v", err)
	}
	pending, err = PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if containsFilename(pending, "thumb-1.mp4") {
		t.Fatalf("expected thumb-1.mp4 to no longer be pending after thumbnail + waveform are done")
	}

	// Stored peaks round-trip back through the pool.
	pool, err = GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	for _, m := range pool.Medias {
		if m.Filename == "thumb-1.mp4" {
			if m.Waveform != `[0.1,0.2,0.3]` {
				t.Fatalf("stored waveform = %q, want %q", m.Waveform, `[0.1,0.2,0.3]`)
			}
		}
	}

	if err := RequestThumbnailRefresh("thumb-1.mp4"); err != nil {
		t.Fatalf("RequestThumbnailRefresh: %v", err)
	}

	if err := RequestWaveformAnalysis("thumb-1.mp4"); err != nil {
		t.Fatalf("RequestWaveformAnalysis: %v", err)
	}
	pending, err = PendingThumbnails()
	if err != nil {
		t.Fatalf("PendingThumbnails: %v", err)
	}
	if !containsFilename(pending, "thumb-1.mp4") {
		t.Fatalf("expected thumb-1.mp4 to be pending again after refresh/analyse requests")
	}
	if err := MarkThumbnailDone(mediaID); err != nil {
		t.Fatalf("MarkThumbnailDone: %v", err)
	}
	if err := StoreWaveform(mediaID, ""); err != nil {
		t.Fatalf("StoreWaveform: %v", err)
	}
}

func containsFilename(medias []Media, filename string) bool {
	for _, m := range medias {
		if m.Filename == filename {
			return true
		}
	}
	return false
}

// Time parsing tests
func TestParseTime(t *testing.T) {
	tests := []struct {
		input    string
		expected int // milliseconds
		wantErr  bool
	}{
		// Bare seconds (no colons)
		{"90", 90_000, false},
		{"90.5", 90_500, false},
		{"0", 0, false},
		{"1.5", 1_500, false},

		// mm:ss
		{"1:30", 90_000, false},
		{"1:30.5", 90_500, false},
		{"0:05", 5_000, false},
		{"59:59.999", 3_599_999, false},

		// hh:mm:ss
		{"1:00:00", 3_600_000, false},
		{"0:01:30", 90_000, false},
		{"0:01:30.5", 90_500, false},
		{"1:30:45", 5_445_000, false},
		{"12:34:56.789", 45_296_789, false},

		// Errors
		{"", 0, true},
		{"abc", 0, true},
		{"1:2:3:4", 0, true},
		{"1:xx", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseTime(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseTime(%q) = %d, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseTime(%q) unexpected error: %v", tc.input, err)
				return
			}
			if got != tc.expected {
				t.Errorf("ParseTime(%q) = %d, want %d", tc.input, got, tc.expected)
			}
		})
	}
}

func TestFormatTime(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "00:00:00.000"},
		{1, "00:00:00.001"},
		{999, "00:00:00.999"},
		{1_000, "00:00:01.000"},
		{60_000, "00:01:00.000"},
		{90_000, "00:01:30.000"},
		{90_500, "00:01:30.500"},
		{3_600_000, "01:00:00.000"},
		{3_661_000, "01:01:01.000"},
		{45_296_789, "12:34:56.789"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d", tc.input), func(t *testing.T) {
			got := FormatTime(tc.input)
			if got != tc.expected {
				t.Errorf("FormatTime(%d) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// Round-trip test
func TestParseTimeRoundTrip(t *testing.T) {
	inputs := []string{"90", "90.5", "1:30", "1:30.5", "1:00:00", "12:34:56.789"}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			ms, err := ParseTime(in)
			if err != nil {
				t.Fatalf("ParseTime(%q): %v", in, err)
			}
			out := FormatTime(ms)
			// Parse the formatted output again
			ms2, err := ParseTime(out)
			if err != nil {
				t.Fatalf("FormatTime round-trip failed for %q: %v", out, err)
			}
			if ms != ms2 {
				t.Errorf("round-trip mismatch: %q -> %dms -> %q -> %dms", in, ms, out, ms2)
			}
		})
	}
}

// Move cue tests
func TestMoveCueUpDown(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "move-a.mp4")
	mustRegisterMedia(t, "move-b.mp4")
	mustRegisterMedia(t, "move-c.mp4")

	// Add three cues at positions 1, 2, 3
	if err := AddCue("move-a.mp4", ""); err != nil {
		t.Fatalf("AddCue a: %v", err)
	}
	if err := AddCue("move-b.mp4", ""); err != nil {
		t.Fatalf("AddCue b: %v", err)
	}
	if err := AddCue("move-c.mp4", ""); err != nil {
		t.Fatalf("AddCue c: %v", err)
	}

	// Initial order: a, b, c at positions 1, 2, 3
	sheet, _ := GetCuesheet()
	if sheet.Cues[0].CuePos != 1 || sheet.Cues[1].CuePos != 2 || sheet.Cues[2].CuePos != 3 {
		t.Fatalf("initial order wrong: %+v", sheet.Cues)
	}

	// Move c (pos 3) up -> should be at pos 2, b at 3
	if err := MoveCueUp("3"); err != nil {
		t.Fatalf("MoveCueUp: %v", err)
	}
	sheet, _ = GetCuesheet()
	if sheet.Cues[0].CuePos != 1 || sheet.Cues[1].CuePos != 2 || sheet.Cues[2].CuePos != 3 {
		t.Fatalf("after MoveCueUp(3): %+v", sheet.Cues)
	}
	if sheet.Cues[1].Media.Filename != "move-c.mp4" || sheet.Cues[2].Media.Filename != "move-b.mp4" {
		t.Fatalf("expected c at pos 2, b at 3: %+v", sheet.Cues)
	}

	// Move a (pos 1) up -> should stay at 1 (already at top)
	if err := MoveCueUp("1"); err != nil {
		t.Fatalf("MoveCueUp(1): %v", err)
	}
	sheet, _ = GetCuesheet()
	if sheet.Cues[0].CuePos != 1 {
		t.Fatalf("MoveCueUp(1) should not move: %+v", sheet.Cues)
	}

	// Move b (pos 3) down -> should stay at 3 (already at bottom)
	if err := MoveCueDown("3"); err != nil {
		t.Fatalf("MoveCueDown(3): %v", err)
	}
	sheet, _ = GetCuesheet()
	if sheet.Cues[2].CuePos != 3 {
		t.Fatalf("MoveCueDown(3) should not move: %+v", sheet.Cues)
	}

	// Move c (pos 2) down -> should go to pos 3, b to 2
	if err := MoveCueDown("2"); err != nil {
		t.Fatalf("MoveCueDown(2): %v", err)
	}
	sheet, _ = GetCuesheet()
	if sheet.Cues[1].Media.Filename != "move-b.mp4" || sheet.Cues[2].Media.Filename != "move-c.mp4" {
		t.Fatalf("expected b at pos 2, c at 3: %+v", sheet.Cues)
	}

	// Move c (pos 3) up twice -> should go to pos 1
	if err := MoveCueUp("3"); err != nil {
		t.Fatalf("MoveCueUp(3) again: %v", err)
	}
	if err := MoveCueUp("2"); err != nil {
		t.Fatalf("MoveCueUp(2) again: %v", err)
	}
	sheet, _ = GetCuesheet()
	if sheet.Cues[0].Media.Filename != "move-c.mp4" {
		t.Fatalf("expected c at pos 1 after two MoveCueUp: %+v", sheet.Cues)
	}
}

func TestAddCueAtPositionBumpsExisting(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "pos-a.mp4")
	mustRegisterMedia(t, "pos-b.mp4")
	mustRegisterMedia(t, "pos-c.mp4")

	if err := AddCue("pos-a.mp4", ""); err != nil {
		t.Fatalf("AddCue a: %v", err)
	}
	if err := AddCue("pos-b.mp4", ""); err != nil {
		t.Fatalf("AddCue b: %v", err)
	}
	// Insert at position 1, which should push the existing cues down.
	if err := AddCue("pos-c.mp4", "1"); err != nil {
		t.Fatalf("AddCue c at position 1: %v", err)
	}

	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	byPos := map[int]string{}
	for _, c := range sheet.Cues {
		byPos[c.CuePos] = c.Media.Filename
	}
	if byPos[1] != "pos-c.mp4" {
		t.Fatalf("expected pos-c.mp4 inserted at position 1, got %q", byPos[1])
	}
	if byPos[2] != "pos-a.mp4" {
		t.Fatalf("expected pos-a.mp4 bumped to position 2, got %q", byPos[2])
	}
}

func TestDeleteMediaRemovesFromMediapool(t *testing.T) {
	mustRegisterMedia(t, "del-1.mp4")

	if err := Delete("del-1.mp4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	if containsFilename(pool.Medias, "del-1.mp4") {
		t.Fatalf("expected del-1.mp4 to be removed from the mediapool")
	}
}

// Regression test: deleting a media item referenced by a cue must remove
// that cue too (the cuesheet table's ON DELETE CASCADE foreign key), per
// spec ("If a media file is deleted... those cues should be deleted").
func TestDeleteMediaCascadesToCues(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "cascade-1.mp4")
	if err := AddCue("cascade-1.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	if err := Delete("cascade-1.mp4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Cues) != 0 {
		t.Fatalf("expected the cue referencing the deleted media to be gone, got %+v", sheet.Cues)
	}
}

func TestRemoveCueByPosition(t *testing.T) {
	mustRegisterMedia(t, "rm-1.mp4")
	mustRegisterMedia(t, "rm-2.mp4")

	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	if err := AddCue("rm-1.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := AddCue("rm-2.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}

	if err := RemoveCue("1"); err != nil {
		t.Fatalf("RemoveCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 0 {
		t.Fatalf("expected deleted selected cue to clear selection, got %d", got)
	}

	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	for _, c := range sheet.Cues {
		if c.Title == "rm-1.mp4" {
			t.Fatalf("expected removed cue to be absent")
		}
	}
	if len(sheet.Cues) != 1 || sheet.Cues[0].CuePos != 1 || sheet.Cues[0].Title != "rm-2.mp4" {
		t.Fatalf("expected remaining cue to compact to position 1, got %+v", sheet.Cues)
	}
}

func TestCueOrderAndSelectionStayConsistentAfterChanges(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "order-select-a.mp4")
	mustRegisterMedia(t, "order-select-b.mp4")
	mustRegisterMedia(t, "order-select-c.mp4")
	for _, name := range []string{"order-select-a.mp4", "order-select-b.mp4", "order-select-c.mp4"} {
		if err := AddCue(name, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", name, err)
		}
	}
	if err := SetCue("3"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	if err := MoveCueUp("3"); err != nil {
		t.Fatalf("MoveCueUp: %v", err)
	}
	if selected, _ := SelectedCuePos(); selected != 2 {
		t.Fatalf("selected cue after move up = %d, want 2", selected)
	}
	if err := RemoveCue("1"); err != nil {
		t.Fatalf("RemoveCue: %v", err)
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Cues) != 2 || sheet.Cues[0].CuePos != 1 || sheet.Cues[1].CuePos != 2 {
		t.Fatalf("cue positions after delete = %+v, want contiguous 1..2", sheet.Cues)
	}
	if selected, _ := SelectedCuePos(); selected != 1 {
		t.Fatalf("selected cue after deleting earlier cue = %d, want 1", selected)
	}
}

func TestNextCuePos(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "nc-a.mp4")
	mustRegisterMedia(t, "nc-b.mp4")
	mustRegisterMedia(t, "nc-c.mp4")
	for _, f := range []string{"nc-a.mp4", "nc-b.mp4", "nc-c.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue %s: %v", f, err)
		}
	}
	cues, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 3 {
		t.Fatalf("expected 3 cues, got %d", len(cues.Cues))
	}
	posA, posB, posC := cues.Cues[0].CuePos, cues.Cues[1].CuePos, cues.Cues[2].CuePos

	next, err := NextCuePos(posA)
	if err != nil || next != posB {
		t.Fatalf("NextCuePos(%d) = %d, %v; want %d", posA, next, err, posB)
	}
	next, err = NextCuePos(posB)
	if err != nil || next != posC {
		t.Fatalf("NextCuePos(%d) = %d, %v; want %d", posB, next, err, posC)
	}
	next, err = NextCuePos(posC)
	if err != nil || next != 0 {
		t.Fatalf("NextCuePos(last) = %d, %v; want 0 (no next)", next, err)
	}
}

func TestLoopCountColumnAndDefaults(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "loop.mp4")
	if err := AddCue("loop.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	cues, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues.Cues))
	}
	cue := cues.Cues[0]
	// New cues default loop off, hold off, infinite loop count 0.
	if cue.Loop || cue.Hold || cue.LoopCount != 0 {
		t.Fatalf("new cue defaults loop=%v hold=%v loop_count=%d, want off/off/0", cue.Loop, cue.Hold, cue.LoopCount)
	}
	pos := strconv.Itoa(cue.CuePos)

	for col, val := range map[string]string{"loop": "true", "loop_count": "5"} {
		if err := UpdateCue(pos, col, val); err != nil {
			t.Fatalf("UpdateCue %s: %v", col, err)
		}
	}
	c, err := GetCue(pos)
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if !c.Loop || c.LoopCount != 5 {
		t.Fatalf("loop=%v loop_count=%d, want true/5", c.Loop, c.LoopCount)
	}
	if err := UpdateCue(pos, "loop_count", "0"); err != nil {
		t.Fatalf("UpdateCue loop_count to 0: %v", err)
	}
	c, err = GetCue(pos)
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if c.LoopCount != 0 {
		t.Fatalf("loop_count = %d, want 0", c.LoopCount)
	}
	if err := UpdateCue(pos, "loop_count", "-1"); err == nil {
		t.Fatalf("UpdateCue loop_count -1 should be rejected")
	}
}

func TestMarkMissingFilesAndRelink(t *testing.T) {
	dir := t.TempDir()
	config.SetDirsForTesting(dir)
	if err := os.WriteFile(filepath.Join(dir, "exists.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing fixture media: %v", err)
	}
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "exists.mp4")
	mustRegisterMedia(t, "gone.mp4")

	MarkMissingFiles()
	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	missing := map[string]bool{}
	for _, m := range pool.Medias {
		missing[m.Filename] = m.Missing
	}
	if missing["exists.mp4"] {
		t.Fatalf("exists.mp4 flagged missing, want present")
	}
	if !missing["gone.mp4"] {
		t.Fatalf("gone.mp4 not flagged missing, want missing")
	}

	if err := AddCue("gone.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(sheet.Cues))
	}
	pos := strconv.Itoa(sheet.Cues[0].CuePos)

	if err := ReplaceCueMedia(pos, "exists.mp4"); err != nil {
		t.Fatalf("ReplaceCueMedia: %v", err)
	}
	cue, err := GetCue(pos)
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if cue.Filename != "exists.mp4" {
		t.Fatalf("relinked cue filename = %q, want exists.mp4", cue.Filename)
	}
	if cue.Title != "gone.mp4" {
		t.Fatalf("relink must keep the old title, got %q", cue.Title)
	}
	if err := ReplaceCueMedia(pos, "no-such.mp4"); err == nil {
		t.Fatalf("re-linking to an unknown filename should fail")
	}
}

func TestNewCueColumnsRoundTrip(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "cols.mp4")
	if err := AddCue("cols.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	cues, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 1 {
		t.Fatalf("expected 1 cue, got %d", len(cues.Cues))
	}
	pos := strconv.Itoa(cues.Cues[0].CuePos)

	cases := map[string][2]string{
		"loop":       {"true", "true"},
		"autoContinue": {"true", "true"},
		"color":      {"#ff00aa", "#ff00aa"},
		"fadeAction": {"all", "all"},
		"fadeOut":    {"2.5", "2.5"}, // 2.5 s stored as ms
		"volume":     {"0.6", "0.6"}, // per-cue master gain in dB; 0 = 0dB default
	}
	for col, vals := range cases {
		if err := UpdateCue(pos, col, vals[0]); err != nil {
			t.Fatalf("UpdateCue %s: %v", col, err)
		}
		c, err := GetCue(pos)
		if err != nil {
			t.Fatalf("GetCue: %v", err)
		}
		switch col {
		case "loop":
			if !c.Loop {
				t.Fatalf("loop not stored")
			}
		case "autoContinue":
			if !c.AutoContinue {
				t.Fatalf("autoContinue not stored")
			}
		case "color":
			if c.Color != vals[1] {
				t.Fatalf("color = %q, want %q", c.Color, vals[1])
			}
		case "fadeAction":
			if c.FadeAction != vals[1] {
				t.Fatalf("fadeAction = %q, want %q", c.FadeAction, vals[1])
			}
		case "fadeOut":
			if c.FadeOut != 2500 {
				t.Fatalf("fadeOut = %d, want 2500", c.FadeOut)
			}
		case "volume":
			if c.Volume != 0.6 {
				t.Fatalf("volume = %v, want 0.6", c.Volume)
			}
		}
	}
}

func TestExportImportCues(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "exp-a.mp4")
	mustRegisterMedia(t, "exp-b.mp4")
	for _, f := range []string{"exp-a.mp4", "exp-b.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
	}
	// Set some worth-persisting settings.
	if err := UpdateCue("1", "hold", "1"); err != nil {
		t.Fatalf("UpdateCue hold: %v", err)
	}
	if err := UpdateCue("2", "volume", "0.6"); err != nil {
		t.Fatalf("UpdateCue volume: %v", err)
	}
	_ = SetCue("1")

	cues, selected, err := ExportCues()
	if err != nil {
		t.Fatalf("ExportCues: %v", err)
	}
	if len(cues) != 2 || selected != 1 {
		t.Fatalf("ExportCues: len=%d selected=%d", len(cues), selected)
	}
	if !cues[0].Hold || cues[0].Title != "exp-a.mp4" {
		t.Fatalf("cue 0 wrong: %+v", cues[0])
	}
	if cues[1].Volume != 0.6 {
		t.Fatalf("cue 1 wrong: %+v", cues[1])
	}

	// Import: append adds two more cues after the two.
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	for _, c := range cues {
		if _, err := AddCueFull(c); err != nil {
			t.Fatalf("AddCueFull: %v", err)
		}
	}
	count, err := CueCount()
	if err != nil || count != 2 {
		t.Fatalf("CueCount after import = %v, %v", count, err)
	}
	cues2, _, _ := ExportCues()
	if len(cues2) != 2 || cues2[0].Hold != true || cues2[1].Volume != 0.6 {
		t.Fatalf("round-trip settings lost: %+v", cues2)
	}
}

func TestMediaRegistered(t *testing.T) {
	ok, err := MediaRegistered("nonexistent-file.mp4")
	if err != nil {
		t.Fatalf("MediaRegistered: %v", err)
	}
	if ok {
		t.Fatal("MediaRegistered returned true for an unregistered file")
	}
	mustRegisterMedia(t, "registered-test.mp4")
	ok, err = MediaRegistered("registered-test.mp4")
	if err != nil {
		t.Fatalf("MediaRegistered: %v", err)
	}
	if !ok {
		t.Fatal("MediaRegistered returned false after registration")
	}
}
