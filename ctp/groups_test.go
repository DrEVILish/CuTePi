package ctp

import (
	"strconv"
	"testing"
)

func lastCuePos() (int, error) {
	var m int
	err := db.Get(&m, `SELECT MAX(cuePos) FROM cuesheet`)
	return m, err
}

func TestCueGroupsCRUDAndMembership(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)

	// Create a nested pair + one cue for each level plus one top-level cue.
	outer, err := CreateGroup("Folder A", 0)
	if err != nil {
		t.Fatalf("CreateGroup A: %v", err)
	}
	inner, err := CreateGroup("Folder A/1", outer)
	if err != nil {
		t.Fatalf("CreateGroup inner: %v", err)
	}

	mustRegisterMedia(t, "g-a.mp4")
	mustRegisterMedia(t, "g-b.mp4")
	mustRegisterMedia(t, "g-c.mp4")
	mustRegisterMedia(t, "g-top.mp4")
	positions := map[string]int{}
	for _, f := range []string{"g-a.mp4", "g-b.mp4", "g-c.mp4", "g-top.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
		pos, err := lastCuePos()
		if err != nil {
			t.Fatalf("lastCuePos: %v", err)
		}
		positions[f] = pos
	}

	// Assign g-a and g-b to the inner group; g-c to the outer group.
	if err := SetCueGroup(itoa(positions["g-a.mp4"]), inner); err != nil {
		t.Fatalf("SetCueGroup a: %v", err)
	}
	if err := SetCueGroup(itoa(positions["g-b.mp4"]), inner); err != nil {
		t.Fatalf("SetCueGroup b: %v", err)
	}
	if err := SetCueGroup(itoa(positions["g-c.mp4"]), outer); err != nil {
		t.Fatalf("SetCueGroup c: %v", err)
	}

	groups, err := Groups()
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if groups[0].Name != "Folder A" || groups[1].Name != "Folder A/1" {
		t.Fatalf("group names wrong: %+v", groups)
	}

	// Members: inner has a and b, outer has c.
	innerMembers, err := GroupCuePositions(inner)
	if err != nil {
		t.Fatalf("inner members: %v", err)
	}
	if len(innerMembers) != 2 {
		t.Fatalf("inner group want 2 members, got %d", len(innerMembers))
	}
	outerMembers, _ := GroupCuePositions(outer)
	if len(outerMembers) != 1 {
		t.Fatalf("outer group want 1 member, got %d", len(outerMembers))
	}

	// Slideshow settings round-trip.
	target := groups[0]
	target.Slideshow = true
	target.Shuffle = true
	target.Loop = true
	target.FadeMS = 500
	target.DurationMS = 3000
	if err := UpdateGroup(target); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	got, err := GetGroup(target.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if !got.Slideshow || !got.Shuffle || !got.Loop || got.FadeMS != 500 || got.DurationMS != 3000 {
		t.Fatalf("slideshow settings not persisted: %+v", got)
	}

	// Deleting a group releases its cues to the top level.
	if err := DeleteGroup(inner); err != nil {
		t.Fatalf("DeleteGroup inner: %v", err)
	}
	released, err := GroupCuePositions(inner)
	if err != nil {
		t.Fatalf("released members: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("deleted group still has members: %+v", released)
	}
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	for _, cue := range sheet.Cues {
		if cue.Parent == inner {
			t.Fatalf("cue still inside deleted group: %+v", cue)
		}
	}
}

func TestCueGroupsRenderFlatOrder(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	group, err := CreateGroup("Only Folder", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	mustRegisterMedia(t, "gf-1.mp4")
	mustRegisterMedia(t, "gf-2.mp4")
	if err := AddCue("gf-1.mp4", ""); err != nil {
		t.Fatalf("AddCue 1: %v", err)
	}
	p1, _ := lastCuePos()
	if err := AddCue("gf-2.mp4", ""); err != nil {
		t.Fatalf("AddCue 2: %v", err)
	}
	p2, _ := lastCuePos()
	if err := SetCueGroup(itoa(p1), group); err != nil {
		t.Fatalf("SetCueGroup 1: %v", err)
	}
	if err := SetCueGroup(itoa(p2), group); err != nil {
		t.Fatalf("SetCueGroup 2: %v", err)
	}

	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Groups) != 1 || sheet.Groups[0].GroupID != group {
		t.Fatalf("cuesheet groups wrong: %+v", sheet.Groups)
	}
	members, _ := GroupCuePositions(group)
	if len(members) != 2 {
		t.Fatalf("group members wrong: %+v", members)
	}
	// Flat order is preserved by the reindex (contiguous run).
	if members[1] != members[0]+1 {
		t.Fatalf("group members not contiguous: %+v", members)
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}