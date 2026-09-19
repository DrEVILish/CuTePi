package ctp

import (
	"testing"
)

// Collapsing an empty group must hide nothing: neighbours above and below
// keep rendering. Regression: FlattenSheet skipped every row after a
// collapsed header, swallowing unrelated cues until the next header.
func TestCollapsedEmptyGroupKeepsNeighbours(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "cn-a.mp4")
	mustRegisterMedia(t, "cn-b.mp4")
	if err := AddCue("cn-a.mp4", ""); err != nil {
		t.Fatalf("AddCue a: %v", err)
	}
	if err := AddCue("cn-b.mp4", ""); err != nil {
		t.Fatalf("AddCue b: %v", err)
	}
	gid, err := CreateGroup("Empty", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	// Park the empty header between the two cues, then collapse it.
	if err := MoveGroup(gid, 2, 0); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	g, err := GetGroup(gid)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	g.Collapse = true
	if err := UpdateGroup(g); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (cue, header, cue), got %d: %+v", len(rows), rows)
	}
	if rows[0].Cue == nil || rows[1].Group == nil || rows[2].Cue == nil {
		t.Fatalf("wrong row order: %+v", rows)
	}
}

// Collapsing a group hides its members — but a top-level cue parked after
// the last member (before the next header) must still render.
func TestCollapsedGroupHidesOnlyMembers(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "cm-m.mp4")
	mustRegisterMedia(t, "cm-t.mp4")
	gid, err := CreateGroup("Folder", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := AddCue("cm-m.mp4", ""); err != nil {
		t.Fatalf("AddCue m: %v", err)
	}
	mpos, _ := lastCuePos()
	if err := SetCueGroup(itoa(mpos), gid); err != nil {
		t.Fatalf("SetCueGroup: %v", err)
	}
	if err := AddCue("cm-t.mp4", ""); err != nil {
		t.Fatalf("AddCue t: %v", err)
	}
	g, _ := GetGroup(gid)
	g.Collapse = true
	if err := UpdateGroup(g); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (header + top-level cue), got %d: %+v", len(rows), rows)
	}
	if rows[0].Group == nil || rows[1].Cue == nil || rows[1].Cue.Title != "cm-t.mp4" {
		t.Fatalf("wrong rows: %+v", rows)
	}
}

// Shift-extend follows VISUAL order (not cuePos) and includes spanned group
// headers (as -gid). Regression: the old handler ranged over numeric cuePos,
// highlighting the wrong cues after any drag-reorder.
func TestExtendSelectionVisualOrderWithHeader(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "ex-a.mp4")
	mustRegisterMedia(t, "ex-b.mp4")
	mustRegisterMedia(t, "ex-c.mp4")
	for _, f := range []string{"ex-a.mp4", "ex-b.mp4", "ex-c.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
	}
	// Visual [C(3), A(1), B(2)]: drag C before A, then park an empty group
	// header between A and B.
	if err := SheetDrop([]int{3}, 0, "cue", 1, false, false, false, nil, 0); err != nil {
		t.Fatalf("SheetDrop: %v", err)
	}
	gid, err := CreateGroup("Gap", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := MoveGroup(gid, 2, 0); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	// Anchor on C (cuePos 3), extend to B (cuePos 2): visual span is
	// C, A, header, B.
	if err := SetSelection(3, nil); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	if err := ExtendSelection(2, 0); err != nil {
		t.Fatalf("ExtendSelection: %v", err)
	}
	set := SelectedSet()
	want := map[int]bool{1: true, 2: true, -gid: true}
	if len(set) != len(want) {
		t.Fatalf("set = %v, want cues 1,2 + header -%d", set, gid)
	}
	for _, p := range set {
		if !want[p] {
			t.Fatalf("set = %v, want cues 1,2 + header -%d", set, gid)
		}
	}
	if anchor, _ := SelectedCuePos(); anchor != 3 {
		t.Fatalf("anchor = %d, want 3", anchor)
	}
}

// "Add to New Group" seats the folder at the right-clicked cue's slot:
// A B C D, selection {B,C}, at=C → A, header, B, C, D with B,C parented.
func TestBulkGroupNewAtAnchorSlot(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "ng-a.mp4")
	mustRegisterMedia(t, "ng-b.mp4")
	mustRegisterMedia(t, "ng-c.mp4")
	mustRegisterMedia(t, "ng-d.mp4")
	for _, f := range []string{"ng-a.mp4", "ng-b.mp4", "ng-c.mp4", "ng-d.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
	}
	gid, err := BulkGroupNewAt([]int{2, 3}, 3)
	if err != nil {
		t.Fatalf("BulkGroupNewAt: %v", err)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	if len(rows) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Cue == nil || rows[1].Group == nil || rows[1].Group.GroupID != gid ||
		rows[2].Cue == nil || rows[2].Cue.CuePos != 2 ||
		rows[3].Cue == nil || rows[3].Cue.CuePos != 3 ||
		rows[4].Cue == nil {
		t.Fatalf("wrong layout: %+v", rows)
	}
	parents, _ := storedParents()
	if parents[2] != gid || parents[3] != gid {
		t.Fatalf("parents = %v, want 2,3 -> %d", parents, gid)
	}
}

// Shift+arrows grow and shrink the selection around a fixed anchor, in
// visual order: down twice from A selects A,B,C; up once drops C again;
// stepping onto the anchor clears the set.
func TestExtendStepGrowsAndShrinks(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "es-a.mp4")
	mustRegisterMedia(t, "es-b.mp4")
	mustRegisterMedia(t, "es-c.mp4")
	for _, f := range []string{"es-a.mp4", "es-b.mp4", "es-c.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
	}
	if err := SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	if err := ExtendStep(1); err != nil {
		t.Fatalf("ExtendStep down: %v", err)
	}
	if err := ExtendStep(1); err != nil {
		t.Fatalf("ExtendStep down: %v", err)
	}
	if set := SelectedSet(); len(set) != 2 || set[0] != 2 || set[1] != 3 {
		t.Fatalf("set after 2x down = %v, want [2 3]", set)
	}
	if err := ExtendStep(-1); err != nil {
		t.Fatalf("ExtendStep up: %v", err)
	}
	if set := SelectedSet(); len(set) != 1 || set[0] != 2 {
		t.Fatalf("set after up = %v, want [2]", set)
	}
	if err := ExtendStep(-1); err != nil {
		t.Fatalf("ExtendStep up onto anchor: %v", err)
	}
	if set := SelectedSet(); len(set) != 0 {
		t.Fatalf("set on anchor = %v, want []", set)
	}
	if anchor, _ := SelectedCuePos(); anchor != 1 {
		t.Fatalf("anchor = %d, want 1", anchor)
	}
}

// A join drop into a collapsed group expands it, so the dropped cue lands
// visibly instead of vanishing into a closed folder.
func TestJoinDropExpandsCollapsedGroup(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "jd-m.mp4")
	gid, err := CreateGroup("Folder", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := AddCue("jd-m.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	mpos, _ := lastCuePos()
	g, _ := GetGroup(gid)
	g.Collapse = true
	if err := UpdateGroup(g); err != nil {
		t.Fatalf("UpdateGroup: %v", err)
	}
	if err := SheetDrop([]int{mpos}, 0, "group", gid, true, false, false, nil, 0); err != nil {
		t.Fatalf("SheetDrop join: %v", err)
	}
	g, _ = GetGroup(gid)
	if g.Collapse {
		t.Fatalf("group still collapsed after join drop")
	}
	parents, _ := storedParents()
	if parents[mpos] != gid {
		t.Fatalf("cue parent = %d, want %d", parents[mpos], gid)
	}
}

// The §5.4 drop contract: an explicit parent joins, parent 0 stays top
// level, an after-slot lands the cue at the exact member gap — and a group
// delete takes its member cues with it.
func TestSheetDropExplicitMembership(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "xd-a.mp4")
	mustRegisterMedia(t, "xd-b.mp4")
	mustRegisterMedia(t, "xd-c.mp4")
	for _, f := range []string{"xd-a.mp4", "xd-b.mp4", "xd-c.mp4"} {
		if err := AddCue(f, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", f, err)
		}
	}
	gid, err := CreateGroup("Drop Target", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	// 1. Explicit join: cue 3 into the group (header anchor, end of block).
	gidInt := gid
	if err := SheetDrop([]int{3}, 0, "group", gid, true, false, false, &gidInt, 0); err != nil {
		t.Fatalf("join drop: %v", err)
	}
	if p := cueParent(t, 3); p != gid {
		t.Fatalf("cue parent = %d, want %d", p, gid)
	}
	// 2. Member-slot drop: cue 2 joins right after cue 3.
	if err := SheetDrop([]int{2}, 0, "cue", 3, false, false, false, &gidInt, 3); err != nil {
		t.Fatalf("slot drop: %v", err)
	}
	if p := cueParent(t, 2); p != gid {
		t.Fatalf("slot drop parent = %d, want %d", p, gid)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	if len(rows) < 4 || rows[1].Group == nil || rows[2].Cue == nil || rows[2].Cue.CuePos != 3 ||
		rows[3].Cue == nil || rows[3].Cue.CuePos != 2 {
		t.Fatalf("slot drop order wrong: %+v", rows)
	}
	// 3. Top-level gap drop below the block: parent 0 wins.
	zero := 0
	if err := SheetDrop([]int{3}, 0, "cue", 1, false, false, false, &zero, 0); err != nil {
		t.Fatalf("top-level drop: %v", err)
	}
	if p := cueParent(t, 3); p != 0 {
		t.Fatalf("cue re-parented into %d, want top level", p)
	}
	// 4. Deleting the folder removes its member cue with it. ReorderCues
	// renumbers inside the delete, so probe by title, not cuePos.
	if err := DeleteGroupWithCues(gid); err != nil {
		t.Fatalf("DeleteGroupWithCues: %v", err)
	}
	var gone int
	if err := db.Get(&gone, `SELECT COUNT(*) FROM cuesheet WHERE title = 'xd-b.mp4'`); err != nil || gone != 0 {
		t.Fatalf("member cue survived folder delete: %d", gone)
	}
}

// End-of-sheet blank below a final expanded group is top-level (§5.4):
// an explicit parent 0 must never re-inherit the last block.
func TestSheetDropEndTopLevel(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	_ = setSelectedCuePos(0)
	mustRegisterMedia(t, "xe-a.mp4")
	mustRegisterMedia(t, "xe-b.mp4")
	if err := AddCue("xe-a.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := AddCue("xe-b.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	gid, err := CreateGroup("End Block", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	gidInt := gid
	// Cue 2 joins the group.
	if err := SheetDrop([]int{2}, 0, "cue", 1, false, false, false, &gidInt, 0); err != nil {
		t.Fatalf("join drop: %v", err)
	}
	// Blank-space intent: top-level, end of sheet.
	zero := 0
	if err := SheetDrop([]int{1}, 0, "end", 0, false, false, false, &zero, 0); err != nil {
		t.Fatalf("end drop: %v", err)
	}
	if p := cueParent(t, 1); p != 0 {
		t.Fatalf("end-of-sheet drop parent = %d, want 0", p)
	}
}

func cueParent(t *testing.T, pos int) int {
	t.Helper()
	var p int
	if err := db.Get(&p, `SELECT parent FROM cuesheet WHERE cuePos = ?`, pos); err != nil {
		t.Fatalf("cue %d missing: %v", pos, err)
	}
	return p
}

// Member cues sit one level deeper than their header (§6.4): a top-level
// group's members render with --depth: 1 so the name indent is visible.
func TestMemberDepthIndents(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "md-a.mp4")
	if err := AddCue("md-a.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	gid, err := CreateGroup("Depth Block", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	gidInt := gid
	if err := SheetDrop([]int{1}, 0, "group", gid, true, false, false, &gidInt, 0); err != nil {
		t.Fatalf("join: %v", err)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	var headerDepth, memberDepth int
	for _, r := range rows {
		if r.Group != nil {
			headerDepth = r.Depth
		} else if r.Cue != nil && r.Cue.Parent == gid {
			memberDepth = r.Depth
		}
	}
	if headerDepth != 0 || memberDepth != 1 {
		t.Fatalf("depths: header %d, member %d, want 0 / 1", headerDepth, memberDepth)
	}
}

// The folder outline must keep enclosing every literal member even when its
// visual position sits after a nested subgroup's header: the previously
// drawn mode closed the parent span around the subgroup, so members after
// it rendered as depth-0 strays with no side borders.
func TestOutlineEnclosesMemberAfterNestedSubgroup(t *testing.T) {
	db.Exec(`DELETE FROM cue_group`)
	if err := ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}
	mustRegisterMedia(t, "sub-after.mp4")
	for i := 0; i < 4; i++ {
		if err := AddCue("sub-after.mp4", ""); err != nil {
			t.Fatalf("AddCue %d: %v", i, err)
		}
	}
	gid, err := CreateGroup("G1", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	sub, err := CreateGroup("Sub", gid)
	if err != nil {
		t.Fatalf("CreateGroup sub: %v", err)
	}
	// c1,c2 -> G1; c3 -> Sub; c4 -> G1 again (ordered after the subgroup).
	for _, p := range []int{1, 2} {
		if err := SetCueGroup(itoa(p), gid); err != nil {
			t.Fatalf("SetCueGroup c%d: %v", p, err)
		}
	}
	if err := SetCueGroup(itoa(3), sub); err != nil {
		t.Fatalf("SetCueGroup c3: %v", err)
	}
	if err := SetCueGroup(itoa(4), gid); err != nil {
		t.Fatalf("SetCueGroup c4: %v", err)
	}
	// Push c4's sheet_index past the Sub header (gap-indexed layout).
	if _, err := db.Exec(`UPDATE cuesheet SET sheet_index = 6000 WHERE cuePos = 4`); err != nil {
		t.Fatalf("bump c4 index: %v", err)
	}
	cs, err := GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	rows := FlattenSheet(&cs)
	lastG1 := -1
	for i, r := range rows {
		if r.Cue != nil && r.Cue.Parent == gid {
			if r.Depth == 0 {
				t.Fatalf("cue %d (member of G1) rendered at depth 0 — escaped the folder outline", r.Cue.CuePos)
			}
			lastG1 = i
		}
	}
	if lastG1 < 0 {
		t.Fatal("no G1 member rows rendered")
	}
	if !rows[lastG1].LastInGroup {
		t.Fatal("last G1 member after the subgroup is not LastInGroup — folder outline never closes under it")
	}
}
