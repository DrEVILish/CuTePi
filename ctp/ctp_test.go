package ctp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	// Already at the last cue - NextCue must not advance past the last cue.
	if err := NextCue(); err != nil {
		t.Fatalf("NextCue: %v", err)
	}
	if got, _ := SelectedCuePos(); got != 3 {
		t.Fatalf("expected SelectedCuePos to stay at 3, got %d", got)
	}

	if err := SelectStep(-1); err != nil {
		t.Fatalf("SelectStep(-1): %v", err)
	}
	if got, _ := SelectedCuePos(); got != 2 {
		t.Fatalf("expected SelectedCuePos=2 after SelectStep(-1), got %d", got)
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
	if err := SelectStep(-1); err != nil {
		t.Fatalf("SelectStep(-1): %v", err)
	}
	// Regression test: stepping back used to have an off-by-one that blocked
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

// Re-uploading a registered filename must replace the probe metadata (and
// re-queue thumbnail/waveform work) instead of failing on the UNIQUE
// constraint — the operator's "replace a file" flow. media_id must stay
// stable so every cue FK'd to it survives.
func TestRegisterMediaReuploadReplacesProbe(t *testing.T) {
	mustRegisterMedia(t, "dup.mp4")

	var before int
	if err := db.Get(&before, `SELECT media_id FROM mediapool WHERE filename = 'dup.mp4'`); err != nil {
		t.Fatalf("reading media_id: %v", err)
	}

	if err := RegisterMedia("dup.mp4", 999, media.Metadata{
		Mimetype:   "video/mp4",
		Duration:   42,
		Resolution: "640x360",
		Codec:      "hevc",
	}, "dup"); err != nil {
		t.Fatalf("re-registering dup.mp4: %v", err)
	}

	var row struct {
		Media_id   int     `db:"media_id"`
		Size       int64   `db:"size"`
		Duration   float64 `db:"duration"`
		Resolution string  `db:"resolution"`
		ThumbPend  bool    `db:"thumbnail_pending"`
		WavePend   bool    `db:"waveform_pending"`
	}
	if err := db.Get(&row, `SELECT media_id, size, duration, resolution, thumbnail_pending, waveform_pending FROM mediapool WHERE filename = 'dup.mp4'`); err != nil {
		t.Fatalf("reading replaced row: %v", err)
	}
	if row.Media_id != before {
		t.Errorf("media_id changed on re-upload: %d -> %d (cues would lose their FK)", before, row.Media_id)
	}
	if row.Size != 999 || row.Duration != 42 || row.Resolution != "640x360" {
		t.Errorf("probe metadata not replaced: %+v", row)
	}
	if !row.ThumbPend || !row.WavePend {
		t.Errorf("background work not re-queued: thumb=%v wave=%v", row.ThumbPend, row.WavePend)
	}

	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM mediapool WHERE filename = 'dup.mp4'`); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 row for dup.mp4, got %d", count)
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

// Renaming media must reconcile both the pool row and every cue that
// references the old filename, and must refuse a collision.
func TestRenameMedia(t *testing.T) {
	mustRegisterMedia(t, "rename-old.mp4")
	if err := AddCue("rename-old.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	if err := RenameMedia("rename-old.mp4", "rename-new.mp4"); err != nil {
		t.Fatalf("RenameMedia: %v", err)
	}
	pool, err := GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	if containsFilename(pool.Medias, "rename-old.mp4") || !containsFilename(pool.Medias, "rename-new.mp4") {
		t.Fatalf("pool not renamed: %+v", pool.Medias)
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	found := false
	for _, c := range sheet.Cues {
		if c.Filename == "rename-new.mp4" {
			found = true
		}
		if c.Filename == "rename-old.mp4" {
			t.Fatal("cue still references the old filename")
		}
	}
	if !found {
		t.Fatal("no cue references the renamed file")
	}

	mustRegisterMedia(t, "rename-taken.mp4")
	if err := RenameMedia("rename-new.mp4", "rename-taken.mp4"); err == nil {
		t.Fatal("renaming onto an existing pool filename must fail")
	}
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

		// Unit forms: the operator's "1:05.000", "1m5s" and "65" all mean 65 s
		{"1:05.000", 65_000, false},
		{"1m5s", 65_000, false},
		{"65", 65_000, false},
		{"1m 5.5s", 65_500, false},
		{"10s", 10_000, false},
		{"500ms", 500, false},
		{"1h2m3s", 3_723_000, false},
		{"2min", 120_000, false},
		{"1M5S", 65_000, false},
		{"5s1m", 0, true},
		{"1m1m", 0, true},
		{"1x", 0, true},
		{"m5", 0, true},
		{"-1s", 0, true},

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

// Deleting media must not leave it pinned as a test pattern or set as the
// panic holding image.
func TestDeleteMediaClearsPinAndHoldingImage(t *testing.T) {
	mustRegisterMedia(t, "del-pin.png")
	mustRegisterMedia(t, "del-keep.png")
	if err := AddTestPattern("del-pin.png"); err != nil {
		t.Fatal(err)
	}
	if err := AddTestPattern("del-keep.png"); err != nil {
		t.Fatal(err)
	}
	if err := SetPanicHoldImage("del-pin.png"); err != nil {
		t.Fatal(err)
	}
	if err := Delete("del-pin.png"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, p := range TestPatterns() {
		if p == "del-pin.png" {
			t.Fatalf("deleted media still pinned as a test pattern: %v", TestPatterns())
		}
	}
	found := false
	for _, p := range TestPatterns() {
		found = found || p == "del-keep.png"
	}
	if !found {
		t.Fatalf("unrelated pin was removed: %v", TestPatterns())
	}
	if got := GetPanicHoldImage(); got != "" {
		t.Fatalf("holding image still %q after its media was deleted", got)
	}
	_ = SetPanicHoldImage("")
	_ = RemoveTestPattern("del-keep.png")
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

// After a drag the visual (sheet_index) order differs from cuePos order;
// removing a cue re-indexes to 1..N and used to trip UNIQUE(cuePos).
func TestRemoveCueAfterReorderKeepsUniquePositions(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ro-a.mp4", "ro-b.mp4", "ro-c.mp4", "ro-d.mp4"} {
		mustRegisterMedia(t, n)
		if err := AddCue(n, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Visual order becomes c, a, b, d while cuePos stays a=1 b=2 c=3 d=4.
	if _, err := db.Exec(`UPDATE cuesheet SET sheet_index = CASE cuePos WHEN 3 THEN 1000 WHEN 1 THEN 2000 WHEN 2 THEN 3000 ELSE 4000 END`); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCue("4"); err != nil {
		t.Fatalf("RemoveCue after reorder: %v", err)
	}
	var names []string
	if err := db.Select(&names, `SELECT m.filename FROM cuesheet c JOIN mediapool m ON m.media_id = c.media_id ORDER BY c.cuePos`); err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 || names[0] != "ro-c.mp4" || names[1] != "ro-a.mp4" || names[2] != "ro-b.mp4" {
		t.Fatalf("expected positions to follow visual order c,a,b; got %v", names)
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
	// Selection follows its cue: cue c (cuePos 3) reindexes 3->2 — not 1.
	// (The old code subtracted 1 on top of the reindex map.)
	if selected, _ := SelectedCuePos(); selected != 2 {
		t.Fatalf("selected cue after deleting earlier cue = %d, want 2", selected)
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
		"loop":         {"true", "true"},
		"autoContinue": {"true", "true"},
		"color":        {"#ff00aa", "#ff00aa"},
		"fadeAction":   {"all", "all"},
		"fadeOut":      {"2.5", "2.5"}, // 2.5 s stored as ms
		"volume":       {"0.6", "0.6"}, // per-cue master gain in dB; 0 = 0dB default
		"fadeCurve":    {"log", "log"}, // column is fade_curve; the API name must map
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
		case "fadeCurve":
			if c.FadeCurve != vals[1] {
				t.Fatalf("fadeCurve = %q, want %q", c.FadeCurve, vals[1])
			}
		}
	}
	if err := UpdateCue(pos, "fadeCurve", "bogus"); err == nil {
		t.Fatalf("unknown fade curve accepted")
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

// TestReorderCuesBeyondThousand is the runnable check for the reindex
// collision: the old fixed +1000 bump collided with live cuePos values under
// the UNIQUE constraint once a show exceeded 1000 cues. The offset is now N
// (the number of cues), which is always disjoint from the compact 1..N range,
// so reordering must succeed at any show size.
func TestReorderCuesBeyondThousand(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	const n = 1005
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("reorder-%d.mp4", i)
		mustRegisterMedia(t, names[i])
		if err := AddCue(names[i], ""); err != nil {
			t.Fatalf("AddCue(%d): %v", i, err)
		}
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	order := make([]int, n)
	for i, c := range sheet.Cues {
		order[i] = c.CuePos
	}
	// Reverse every position; the pre-fix broken bump died here.
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	if _, err := ReorderCues(order); err != nil {
		t.Fatalf("ReorderCues(%d cues): %v", n, err)
	}
	got, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet after reorder: %v", err)
	}
	if len(got.Cues) != n {
		t.Fatalf("after reorder: %d cues, want %d", len(got.Cues), n)
	}
	// Positions reindexed 1..N and the new first row is the old last one.
	if got.Cues[0].Title != names[n-1] {
		t.Fatalf("first cue after reversal = %q, want %q", got.Cues[0].Title, names[n-1])
	}
	if got.Cues[n-1].CuePos != n {
		t.Fatalf("last cuePos = %d, want %d (1..N reindex)", got.Cues[n-1].CuePos, n)
	}
}

// §12.5: auto-numbering appends 1, 2, 3…, an insert between numbered cues
// takes the midpoint, and the next append returns to a whole number.
func TestAutoNumberAppendsIntegers(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	if err := SetAutoNumber(true); err != nil {
		t.Fatalf("SetAutoNumber: %v", err)
	}
	mustRegisterMedia(t, "autonum.mp4")
	for i := 0; i < 3; i++ {
		if err := AddCue("autonum.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	nums := func() []string {
		sheet, err := GetCuesheet()
		if err != nil {
			t.Fatalf("GetCuesheet: %v", err)
		}
		var out []string
		for _, c := range sheet.Cues {
			out = append(out, c.CueNum)
		}
		return out
	}
	if got := strings.Join(nums(), ","); got != "1,2,3" {
		t.Fatalf("appended numbers = %s, want 1,2,3", got)
	}
	if err := AddCue("autonum.mp4", "2"); err != nil {
		t.Fatalf("insert AddCue: %v", err)
	}
	if got := strings.Join(nums(), ","); got != "1,1.5,2,3" {
		t.Fatalf("after insert = %s, want 1,1.5,2,3", got)
	}
	if err := AddCue("autonum.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if got := strings.Join(nums(), ","); got != "1,1.5,2,3,4" {
		t.Fatalf("after append = %s, want …,4", got)
	}
	// Renumber rewrites the whole sequence 1, 2, 3… (default step 1).
	if err := RenumberSheet(); err != nil {
		t.Fatalf("RenumberSheet: %v", err)
	}
	if got := strings.Join(nums(), ","); got != "1,2,3,4,5" {
		t.Fatalf("after renumber = %s, want 1,2,3,4,5", got)
	}
	// A configured step applies to renumbering and to the next append.
	if err := SetCueNumStep(10); err != nil {
		t.Fatal(err)
	}
	defer SetCueNumStep(DefaultCueNumStep)
	if err := RenumberSheet(); err != nil {
		t.Fatal(err)
	}
	if err := AddCue("autonum.mp4", ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(nums(), ","); got != "10,20,30,40,50,60" {
		t.Fatalf("step 10 = %s, want 10,20,30,40,50,60", got)
	}
}

func TestCueNumbersStayUnique(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"un-a.mp4", "un-b.mp4"} {
		mustRegisterMedia(t, n)
		if err := AddCue(n, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := UpdateCue("1", "cueNum", "10"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCue("2", "cueNum", "10"); !errors.Is(err, ErrDuplicateCueNum) {
		t.Fatalf("duplicate cue number: err = %v, want ErrDuplicateCueNum", err)
	}
	if err := UpdateCue("2", "cueNum", "10.0"); !errors.Is(err, ErrDuplicateCueNum) {
		t.Fatalf("10.0 is the same number as 10: err = %v", err)
	}
	if err := UpdateCueFields("2", map[string]string{"cueNum": "10"}); !errors.Is(err, ErrDuplicateCueNum) {
		t.Fatalf("batched duplicate: err = %v", err)
	}
	if err := UpdateCue("1", "cueNum", "10"); err != nil {
		t.Fatalf("re-saving a cue's own number must pass: %v", err)
	}

	gid, err := CreateGroup("un-group", 0)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := GetGroup(gid)
	g.CueNum = "10"
	if err := UpdateGroup(g); !errors.Is(err, ErrDuplicateCueNum) {
		t.Fatalf("group taking a cue's number: err = %v", err)
	}
	g.CueNum = "20"
	if err := UpdateGroup(g); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCue("2", "cueNum", "20"); !errors.Is(err, ErrDuplicateCueNum) {
		t.Fatalf("cue taking a group's number: err = %v", err)
	}
	g.Name = "renamed"
	if err := UpdateGroup(g); err != nil {
		t.Fatalf("saving a group with its own number must pass: %v", err)
	}

	if got, err := freeCueNum(db, "10"); err != nil || got != "21" {
		t.Fatalf("freeCueNum(10) = %q, %v; want the next number above everything, 21", got, err)
	}
	if got, err := freeCueNum(db, "7"); err != nil || got != "7" {
		t.Fatalf("freeCueNum(7) = %q, %v; an unused number is kept", got, err)
	}
	_ = DeleteGroup(gid)
}

func TestSortSheetByCueNumber(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"so-a.mp4", "so-b.mp4", "so-c.mp4", "so-d.mp4"} {
		mustRegisterMedia(t, n)
		if err := AddCue(n, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, pn := range [][2]string{{"1", "30"}, {"2", "7"}, {"3", "B"}, {"4", "10"}} {
		if err := UpdateCue(pn[0], "cueNum", pn[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := SortSheetByCueNumber(); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := db.Select(&got, `SELECT cueNum FROM cuesheet ORDER BY sheet_index`); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "7,10,30,B" {
		t.Fatalf("sorted = %v, want 7,10,30,B (numbers by value, text after)", got)
	}
}

// MediaHasAlpha reads the import metadata: the recorded flag, an older
// import's alpha pixel format (not GIF, always BGRA), and false for no
// metadata, broken metadata or an unknown file.
func TestMediaHasAlpha(t *testing.T) {
	reg := func(name string, info *media.MediaInfo) {
		t.Helper()
		if err := RegisterMedia(name, 1, media.Metadata{Mimetype: "video/mp4", Duration: 1, Resolution: "1920x1080", Codec: "x", Info: info}, name); err != nil {
			t.Fatalf("RegisterMedia(%q): %v", name, err)
		}
	}
	reg("alpha-flag.mkv", &media.MediaInfo{Video: &media.MediaVideoInfo{Codec: "vp9", PixFmt: "yuv420p", Alpha: true}})
	reg("alpha-oldpix.mov", &media.MediaInfo{Video: &media.MediaVideoInfo{Codec: "prores", PixFmt: "yuva444p10le"}})
	reg("old.gif", &media.MediaInfo{Video: &media.MediaVideoInfo{Codec: "gif", PixFmt: "bgra"}})
	reg("opaque.mp4", &media.MediaInfo{Video: &media.MediaVideoInfo{Codec: "h264", PixFmt: "yuv420p"}})
	reg("nometa.mp4", nil)
	reg("broken.mp4", &media.MediaInfo{Video: &media.MediaVideoInfo{PixFmt: "rgba"}})
	if err := UpdateMediaMeta("broken.mp4", "{not json"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"alpha-flag.mkv": true, "alpha-oldpix.mov": true, "old.gif": false,
		"opaque.mp4": false, "nometa.mp4": false, "broken.mp4": false, "unknown.mov": false} {
		if got := MediaHasAlpha(name); got != want {
			t.Errorf("MediaHasAlpha(%q) = %v, want %v", name, got, want)
		}
	}
}
