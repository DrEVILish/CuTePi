package ctp

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func TestOverwriteImportRollsBackOnInvalidCue(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	mustRegisterMedia(t, "rollback-existing.mp4")
	if err := AddCue("rollback-existing.mp4", ""); err != nil {
		t.Fatal(err)
	}
	before, selected, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	err = ImportShow([]ExportGroup{{GroupID: 1, Name: "New"}}, []ExportCue{{Filename: "missing.mp4", Title: "bad"}}, 1, true)
	if err == nil {
		t.Fatal("expected import failure")
	}
	after, gotSelected, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || len(after) != 1 || after[0].Filename != before[0].Filename || gotSelected != selected {
		t.Fatalf("sheet changed after failed import: %#v", after)
	}
	groups, err := ExportGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("group survived rollback: %#v", groups)
	}
}

func TestOverwriteImportLiveEndpoint(t *testing.T) {
	if err := ImportShow(nil, []ExportCue{{SourceKind: "endpoint", EndpointURL: "https://example.com", Title: "Live", CueNum: "1"}}, 1, true); err != nil {
		t.Fatal(err)
	}
	cues, selected, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 1 || cues[0].EndpointURL != "https://example.com" || selected != 1 {
		t.Fatalf("bad import: %#v, selected %d", cues, selected)
	}
}

func TestImportRegressionExportOrder(t *testing.T) {
	ClearCueSheet()
	mustRegisterMedia(t, "audit-a.mp4")
	mustRegisterMedia(t, "audit-b.mp4")
	AddCue("audit-a.mp4", "")
	AddCue("audit-b.mp4", "")
	db.Exec("UPDATE cuesheet SET sheet_index = CASE cuePos WHEN 1 THEN 2000 ELSE 1000 END")
	sheet, _ := GetCuesheet()
	exported, _, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Cues[0].Filename != exported[0].Filename {
		t.Fatalf("visible first %q, exported first %q", sheet.Cues[0].Filename, exported[0].Filename)
	}
}
func TestImportRegressionScheduleRoundTrip(t *testing.T) {
	ClearCueSheet()
	mustRegisterMedia(t, "audit-sched.mp4")
	AddCue("audit-sched.mp4", "")
	SetCueSchedule(1, true, 1, 36000)
	cues, selected, _ := ExportCues()
	if err := ImportShow(nil, cues, selected, true); err != nil {
		t.Fatal(err)
	}
	c, _ := GetCue("1")
	if !c.ScheduleEnabled {
		t.Fatal("enabled 10:00 Monday schedule lost after export/import")
	}
}
func TestImportRegressionGroupParentRoundTrip(t *testing.T) {
	ClearCueSheet()
	child, _ := CreateGroup("older child", 0)
	parent, _ := CreateGroup("newer parent", 0)
	g, _ := GetGroup(child)
	g.ParentGroupID = parent
	if err := UpdateGroup(g); err != nil {
		t.Fatal(err)
	}
	groups, _ := ExportGroups()
	if err := ImportShow(groups, nil, 0, true); err != nil {
		t.Fatal(err)
	}
	got, _ := Groups()
	for _, g := range got {
		if g.Name == "older child" && g.ParentGroupID == 0 {
			t.Fatal("older child became top-level after export/import")
		}
	}
}
func TestImportRegressionZeroOpacityRoundTrip(t *testing.T) {
	ClearCueSheet()
	mustRegisterMedia(t, "audit-opacity.mp4")
	AddCue("audit-opacity.mp4", "")
	db.Exec("UPDATE cuesheet SET opacity=0")
	cues, selected, _ := ExportCues()
	if err := ImportShow(nil, cues, selected, true); err != nil {
		t.Fatal(err)
	}
	c, _ := GetCue("1")
	if c.Opacity != 0 {
		t.Fatalf("zero opacity restored as %v", c.Opacity)
	}
}

func TestShowRoundTripPreservesMergedOrderAndSelection(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		ClearCueSheet()
		for _, f := range []string{"round-a.mp4", "round-b.mp4", "round-c.mp4"} {
			mustRegisterMedia(t, f)
			if err := AddCue(f, ""); err != nil {
				t.Fatal(err)
			}
		}
		gid, err := CreateGroup("Empty folder", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := SheetDrop([]int{3}, 0, "cue", 1, false, false, false, nil, 0); err != nil {
			t.Fatal(err)
		}
		if err := MoveGroup(gid, 2, 0); err != nil {
			t.Fatal(err)
		}
		if err := SetCue("3"); err != nil {
			t.Fatal(err)
		}
		before, _ := loadSheetSequence()
		labels := func(seq []SheetItem) []string {
			var out []string
			for _, i := range seq {
				if i.Kind == "group" {
					g, _ := GetGroup(i.GroupID)
					out = append(out, "group:"+g.Name)
				} else {
					c, _ := GetCue(strconv.Itoa(i.CuePos))
					out = append(out, c.Filename)
				}
			}
			return out
		}
		want := labels(before)
		cues, selected, err := ExportCues()
		if err != nil {
			t.Fatal(err)
		}
		groups, err := ExportGroups()
		if err != nil {
			t.Fatal(err)
		}
		if appendMode {
			ClearCueSheet()
			mustRegisterMedia(t, "existing.mp4")
			AddCue("existing.mp4", "")
		}
		if err := ImportShow(groups, cues, selected, !appendMode); err != nil {
			t.Fatal(err)
		}
		after, _ := loadSheetSequence()
		got := labels(after)
		if appendMode {
			if len(got) == 0 || got[0] != "existing.mp4" {
				t.Fatalf("existing prefix lost: %v", got)
			}
			got = got[1:]
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("restored sequence %v, want %v", got, want)
		}
		pos, _ := SelectedCuePos()
		c, err := GetCue(strconv.Itoa(pos))
		if err != nil || c.Filename != "round-c.mp4" {
			t.Fatalf("restored selection %d %+v %v", pos, c, err)
		}
	}
}
func TestOldManifestDefaultsAndExplicitZeroOpacity(t *testing.T) {
	ClearCueSheet()
	mustRegisterMedia(t, "old-default.mp4")
	var c ExportCue
	if err := json.Unmarshal([]byte("{\"filename\":\"old-default.mp4\",\"title\":\"old\"}"), &c); err != nil {
		t.Fatal(err)
	}
	if err := ImportShow(nil, []ExportCue{c}, 1, true); err != nil {
		t.Fatal(err)
	}
	got, _ := GetCue("1")
	if got.Opacity != 100 {
		t.Fatalf("old default opacity %v", got.Opacity)
	}
	if err := json.Unmarshal([]byte("{\"filename\":\"old-default.mp4\",\"title\":\"zero\",\"opacity\":0}"), &c); err != nil {
		t.Fatal(err)
	}
	if err := ImportShow(nil, []ExportCue{c}, 1, true); err != nil {
		t.Fatal(err)
	}
	got, _ = GetCue("1")
	if got.Opacity != 0 {
		t.Fatalf("explicit zero opacity %v", got.Opacity)
	}
}

func TestShowRoundTripGroupSelectionAndBlankNumberLayerLink(t *testing.T) {
	ClearCueSheet()
	for _, f := range []string{"link-base.mp4", "link-overlay.mp4"} {
		mustRegisterMedia(t, f)
		AddCue(f, "")
	}
	// Cue numbers are operator labels, so an empty target number must not erase identity.
	if _, err := db.Exec("UPDATE cuesheet SET cueNum='' WHERE cuePos=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE cuesheet SET layer='under',layer_under=(SELECT cue_id FROM cuesheet WHERE cuePos=1) WHERE cuePos=2"); err != nil {
		t.Fatal(err)
	}
	gid, err := CreateGroup("Selected folder", 0)
	if err != nil {
		t.Fatal(err)
	}
	SetSelectedGroup(gid)
	cues, selected, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := ExportGroups()
	if err := ImportShow(groups, cues, selected, true); err != nil {
		t.Fatal(err)
	}
	group, err := SelectedGroupPos()
	if err != nil || group == 0 {
		t.Fatalf("group selection lost: %d %v", group, err)
	}
	base, _ := GetCue("1")
	overlay, _ := GetCue("2")
	if overlay.LayerUnder != base.Cue_id {
		t.Fatalf("layer target %d, want %d", overlay.LayerUnder, base.Cue_id)
	}
}
func TestShowRoundTripPreservesTiedIndices(t *testing.T) {
	ClearCueSheet()
	mustRegisterMedia(t, "tied.mp4")
	AddCue("tied.mp4", "")
	gid, _ := CreateGroup("Tied", 0)
	db.Exec("UPDATE cue_group SET sheet_index=(SELECT sheet_index FROM cuesheet WHERE cuePos=1) WHERE group_id=?", gid)
	before, _ := loadSheetSequence()
	cues, selected, _ := ExportCues()
	groups, _ := ExportGroups()
	if err := ImportShow(groups, cues, selected, true); err != nil {
		t.Fatal(err)
	}
	after, _ := loadSheetSequence()
	if len(after) != len(before) || after[0].Kind != before[0].Kind {
		t.Fatalf("tie order %v, was %v", after, before)
	}
}

func TestShowRoundTripSelectionAcrossPositionGap(t *testing.T) {
	ClearCueSheet()
	for _, f := range []string{"gap-one.mp4", "gap-two.mp4", "gap-three.mp4"} {
		mustRegisterMedia(t, f)
		AddCue(f, "")
	}
	if _, err := db.Exec("DELETE FROM cuesheet WHERE cuePos=2"); err != nil {
		t.Fatal(err)
	}
	if err := SetCue("3"); err != nil {
		t.Fatal(err)
	}
	cues, selected, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportShow(nil, cues, selected, true); err != nil {
		t.Fatal(err)
	}
	pos, _ := SelectedCuePos()
	cue, err := GetCue(strconv.Itoa(pos))
	if err != nil || cue.Filename != "gap-three.mp4" {
		t.Fatalf("gap selection became %d %+v %v", pos, cue, err)
	}
}
