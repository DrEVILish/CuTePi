package ctp

import "testing"

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
