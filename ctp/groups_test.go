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

// TestGroupMembershipStaysContiguous is the runnable check for the
// contiguity invariant: any position mutation re-aligns membership so every
// group renders as ONE contiguous run — a cue placed into a group's span
// joins it, a member dragged away from the group leaves it, a lone member
// survives, a mid-group insert joins and a boundary insert stays top-level.
func TestGroupMembershipStaysContiguous(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "contig.mp4")

	gid, err := CreateGroup("Contig", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	titles := []string{"contig.mp4", "contig.mp4 (2)", "contig.mp4 (3)", "contig.mp4 (4)"}
	for range titles {
		if err := AddCue("contig.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	parentByTitle := func() map[string]int {
		t.Helper()
		sheet, err := GetCuesheet()
		if err != nil {
			t.Fatalf("GetCuesheet: %v", err)
		}
		m := map[string]int{}
		for _, cue := range sheet.Cues {
			m[cue.Title] = cue.Parent
		}
		return m
	}
	posByTitle := func() map[string]int {
		t.Helper()
		sheet, err := GetCuesheet()
		if err != nil {
			t.Fatalf("GetCuesheet: %v", err)
		}
		m := map[string]int{}
		for _, cue := range sheet.Cues {
			m[cue.Title] = cue.CuePos
		}
		return m
	}

	if err := SetCueGroup(itoa(posByTitle()[titles[0]]), gid); err != nil {
		t.Fatalf("SetCueGroup A: %v", err)
	}
	if err := SetCueGroup(itoa(posByTitle()[titles[1]]), gid); err != nil {
		t.Fatalf("SetCueGroup B: %v", err)
	}
	// Sheet: A(gid), B(gid), C, D top-level.

	// Move the top-level cue up into the group's span: it joins.
	if err := MoveCueUp(itoa(posByTitle()[titles[2]])); err != nil {
		t.Fatalf("MoveCueUp: %v", err)
	}
	if p := parentByTitle()[titles[2]]; p != gid {
		t.Errorf("cue moved between members: parent = %d, want %d", p, gid)
	}

	// Insert at the sheet head (before the group's run): stays top-level.
	if err := AddCue("contig.mp4", "1"); err != nil {
		t.Fatalf("AddCue at sheet head: %v", err)
	}
	if p := parentByTitle()["contig.mp4 (5)"]; p != 0 {
		t.Errorf("cue added before the group: parent = %d, want 0", p)
	}

	// Insert between two members of the run (the second member's slot): the
	// new cue joins.
	sheet, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	gidSlots := []int{}
	for _, cue := range sheet.Cues {
		if cue.Parent == gid {
			gidSlots = append(gidSlots, cue.CuePos)
		}
	}
	if len(gidSlots) < 2 {
		t.Fatalf("need 2+ group members for the mid-insert check, have %d", len(gidSlots))
	}
	if err := AddCue("contig.mp4", itoa(gidSlots[1])); err != nil {
		t.Fatalf("AddCue mid-group: %v", err)
	}
	if p := parentByTitle()["contig.mp4 (6)"]; p != gid {
		t.Errorf("cue added mid-group: parent = %d, want %d", p, gid)
	}

	// A group's lone member survives a no-op reorder untouched.
	gid2, err := CreateGroup("Lone", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := SetCueGroup(itoa(posByTitle()[titles[3]]), gid2); err != nil {
		t.Fatalf("SetCueGroup D: %v", err)
	}
	sheet, err = GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	order := make([]int, 0, len(sheet.Cues))
	for _, cue := range sheet.Cues {
		order = append(order, cue.CuePos)
	}
	if err := ReorderCues(order); err != nil {
		t.Fatalf("ReorderCues: %v", err)
	}
	if p := parentByTitle()[titles[3]]; p != gid2 {
		t.Errorf("lone group member: parent = %d, want %d (must not dissolve)", p, gid2)
	}

	// Drag the head member of the group's run to the sheet head: it is now
	// separated from the rest of its group, so it leaves (the group keeps its
	// remaining members as one contiguous run). Note the top-level cue that
	// sat before the group must NOT get absorbed into the gap.
	sheet, _ = GetCuesheet()
	var runHeadPos int
	var runHeadTitle string
	for _, cue := range sheet.Cues {
		if cue.Parent == gid {
			runHeadPos, runHeadTitle = cue.CuePos, cue.Title
			break
		}
	}
	membersBefore := 0
	for _, p := range parentByTitle() {
		if p == gid {
			membersBefore++
		}
	}
	var leaveOrder []int
	leaveOrder = append(leaveOrder, runHeadPos)
	for _, cue := range sheet.Cues {
		if cue.CuePos != runHeadPos {
			leaveOrder = append(leaveOrder, cue.CuePos)
		}
	}
	if err := ReorderCues(leaveOrder); err != nil {
		t.Fatalf("ReorderCues: %v", err)
	}
	if p := parentByTitle()[runHeadTitle]; p != 0 {
		t.Errorf("member dragged to the sheet head (%q): parent = %d, want 0 (left the group)", runHeadTitle, p)
	}
	remaining := 0
	for title, p := range parentByTitle() {
		if title != runHeadTitle && p == gid {
			remaining++
		}
	}
	if remaining != membersBefore-1 {
		t.Errorf("remaining group members: %d, want %d (group must stay intact)", remaining, membersBefore-1)
	}
}
