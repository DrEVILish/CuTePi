package ctp

import (
	"strconv"
	"fmt"
	"log"
)

// Group is a cue_group row: a visual folder that holds cues (via
// cuesheet.parent = group_id) and, for image groups, slideshow settings.
// Folder membership is a presentation layer - global order stays the flat
// cuePos, so groups never change playback order by themselves.
type Group struct {
	GroupID       int    `db:"group_id"`
	Name          string `db:"name"`
	ParentGroupID int    `db:"parent_group_id"`
	Collapse      bool   `db:"collapse"`
	Slideshow     bool   `db:"slideshow"`
	AwardsMode    bool   `db:"awards_mode"`
	Shuffle       bool   `db:"shuffle"`
	Loop          bool   `db:"loop"`
	FadeMS        int    `db:"fade_ms"`
	DurationMS    int    `db:"duration_ms"`
	CueNum        string `db:"cue_num"`
	Color         string `db:"color"`
	AnchorPos     int    `db:"anchor_pos"` // legacy: pre-sheet_index empty-group anchor
	SheetIndex    float64 `db:"sheet_index"` // header position in the visual sequence (§4)
}

// CreateGroup inserts a new cue group and returns its group_id.
func CreateGroup(name string, parentGroupID int) (int, error) {
	if err := ValidateGroupParent(0, parentGroupID); err != nil {
		return 0, err
	}
	if name == "" {
		name = "New Group"
	}
	// Place the new group at the end of the visual sequence so it doesn't
	// truncate existing groups' spans (which end at the next same-or-shallower
	// depth header). Use max existing sheet_index + step.
	var maxIdx float64
	_ = db.Get(&maxIdx, `SELECT COALESCE(MAX(sheet_index), 0) FROM (
		SELECT sheet_index FROM cuesheet
		UNION ALL
		SELECT sheet_index FROM cue_group
	)`)
	res, err := db.Exec(`
		INSERT INTO cue_group (name, parent_group_id, sheet_index) VALUES (?, ?, ?)
	`, name, parentGroupID, maxIdx+sheetIndexStep)
	if err != nil {
		log.Printf("Error creating cue group: %v", err)
		return 0, err
	}
	id, _ := res.LastInsertId()
	bumpCuesheetVersion()
	return int(id), nil
}

// Groups returns every cue group ordered by id (the order header rows follow
// flat cuePos when they first appear).
func Groups() ([]Group, error) {
	var groups []Group
	if err := db.Select(&groups, `SELECT * FROM cue_group ORDER BY group_id`); err != nil {
		log.Printf("Error listing cue groups: %v", err)
		return nil, err
	}
	return groups, nil
}

// GetGroup returns a single cue group.
func GetGroup(id int) (Group, error) {
	var g Group
	if err := db.Get(&g, `SELECT * FROM cue_group WHERE group_id = ?`, id); err != nil {
		return Group{}, err
	}
	return g, nil
}

// UpdateGroup persists every editable group field in one call.
func UpdateGroup(g Group) error {
	if err := ValidateGroupParent(g.GroupID, g.ParentGroupID); err != nil {
		return err
	}
	_, err := db.Exec(`
		UPDATE cue_group SET name = ?, parent_group_id = ?, collapse = ?,
			slideshow = ?, awards_mode = ?, shuffle = ?, loop = ?, fade_ms = ?, duration_ms = ?,
			cue_num = ?, color = ?
		WHERE group_id = ?
	`, g.Name, g.ParentGroupID, boolInt(g.Collapse), boolInt(g.Slideshow),
		boolInt(g.AwardsMode), boolInt(g.Shuffle), boolInt(g.Loop), g.FadeMS, g.DurationMS,
		g.CueNum, g.Color, g.GroupID)
	if err != nil {
		log.Printf("Error updating cue group: %v", err)
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// DeleteGroupWithCues deletes a group block (§5.4): its member cues, any
// nested subgroups and every cue inside them — folders disappear WITH
// their contents, QLab-style.
func DeleteGroupWithCues(id int) error {
	var all []Group
	if err := db.Select(&all, `SELECT * FROM cue_group`); err != nil {
		return err
	}
	ids := []int{}
	var collect func(int)
	collect = func(gid int) {
		ids = append(ids, gid)
		for _, g := range all {
			if g.ParentGroupID == gid {
				collect(g.GroupID)
			}
		}
	}
	collect(id)
	// Member cues first, then the headers.
	for _, gid := range ids {
		if _, err := db.Exec(`DELETE FROM cuesheet WHERE parent = ?`, gid); err != nil {
			return err
		}
	}
	for _, gid := range ids {
		if _, err := db.Exec(`DELETE FROM cue_group WHERE group_id = ?`, gid); err != nil {
			return err
		}
	}
	// A deleted group must not stay selected (dangling inspector refetch),
	// and its id must not linger in the multi-selection set (selectedSet
	// would resurrect it into a later bulk/extend action).
	if sel, err := SelectedGroupPos(); err == nil && sel == id {
		_ = setSelectedCuePos(0)
	}
	_ = setSelectedSet(nil)
	bumpCuesheetVersion()
	// Reindex to the compact sequence after the removals (bulk rule).
	var byIndex []int
	if err := db.Select(&byIndex, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err == nil && len(byIndex) > 0 {
		_, _ = ReorderCues(byIndex)
	}
	return nil
}

// DeleteGroup removes a cue group and releases its member cues back to the
// top level (presentation layer only - playback order is untouched).
func DeleteGroup(id int) error {
	if _, err := db.Exec(`DELETE FROM cue_group WHERE group_id = ?`, id); err != nil {
		log.Printf("Error deleting cue group: %v", err)
		return err
	}
	if _, err := db.Exec(`UPDATE cuesheet SET parent = 0 WHERE parent = ?`, id); err != nil {
		log.Printf("Error releasing group members: %v", err)
		return err
	}
	// Release nested subgroups too, or they keep a dangling parent_group_id
	// pointing at the deleted group.
	if _, err := db.Exec(`UPDATE cue_group SET parent_group_id = 0 WHERE parent_group_id = ?`, id); err != nil {
		log.Printf("Error releasing nested subgroups: %v", err)
		return err
	}
	// A deleted group must not stay selected: the persisted selection would
	// keep the inspector refetching /api/group/<id>/inspector forever (404
	// toasts on every cuesheet re-render). Reset to no-selection.
	if sel, err := SelectedGroupPos(); err == nil && sel == id {
		_ = setSelectedCuePos(0)
	}
	bumpCuesheetVersion()
	return nil
}

// MoveGroup relocates a group's block (header + whole span, nested rows
// included) before the target row or group header. The dragged block moves
// as one; membership follows the sequence.
func MoveGroup(groupID int, beforeCuePos int, beforeGroup int) error {
	if beforeGroup != 0 {
		return SheetDrop(nil, groupID, "group", beforeGroup, false, false, false, nil, 0)
	}
	return SheetDrop(nil, groupID, "cue", beforeCuePos, false, false, false, nil, 0)
}

// ValidateGroupParent rejects nesting that would corrupt the sheet: a group
// cannot be its own ancestor, and the tree is capped so indentation stays
// legible. 0 is a valid parent (top level).
func ValidateGroupParent(groupID, parentID int) error {
	if parentID == 0 {
		return nil
	}
	if parentID == groupID {
		return fmt.Errorf("a group cannot be its own parent")
	}
	depth := 0
	seen := map[int]bool{groupID: true}
	// Walk newParent's ancestors: reaching groupID would create a cycle.
	for at := parentID; at != 0; {
		if seen[at] {
			return fmt.Errorf("group nesting would create a cycle")
		}
		seen[at] = true
		g, err := GetGroup(at)
		if err != nil {
			return fmt.Errorf("parent group %d not found", at)
		}
		at = g.ParentGroupID
		depth++
		if depth > 8 {
			return fmt.Errorf("group nesting is limited to 8 levels")
		}
	}
	return nil
}


// groupDescendants maps every group id to the set of its PROPER descendants
// (children, grandchildren, ...), cycle-proof.
func groupDescendants() map[int]map[int]bool {
	var groups []Group
	_ = db.Select(&groups, `SELECT group_id, parent_group_id FROM cue_group`)
	children := make(map[int][]int, len(groups))
	for _, g := range groups {
		children[g.ParentGroupID] = append(children[g.ParentGroupID], g.GroupID)
	}
	out := make(map[int]map[int]bool, len(groups))
	var walk func(int, map[int]bool)
	walk = func(id int, seen map[int]bool) {
		for _, ch := range children[id] {
			if seen[ch] {
				continue
			}
			seen[ch] = true
			walk(ch, seen)
		}
	}
	for _, g := range groups {
		seen := make(map[int]bool)
		walk(g.GroupID, seen)
		out[g.GroupID] = seen
	}
	return out
}

// GroupCuePositions lists a group's direct member cue positions in sheet
// (visual) order.
func GroupCuePositions(groupID int) ([]int, error) {
	var positions []int
	if err := db.Select(&positions, `
		SELECT c.cuePos FROM cuesheet c
		JOIN cue_group g ON c.parent = g.group_id
		WHERE c.parent = ?
		ORDER BY c.sheet_index, c.cuePos
	`, groupID); err != nil {
		return nil, err
	}
	return positions, nil
}

// GroupSubtreeCues lists every cue position inside a group's subtree (the
// group's own members plus all descendant groups' members) in visual order.
// Slideshows and group-relative operations work on the subtree.
func GroupSubtreeCues(groupID int) ([]int, error) {
	desc := groupDescendants()
	ids := []int{groupID}
	for k := range desc[groupID] {
		ids = append(ids, k)
	}
	ph := ""
	args := []interface{}{}
	for i, id := range ids {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args = append(args, id)
	}
	var positions []int
	if err := db.Select(&positions, `
		SELECT cuePos FROM cuesheet WHERE parent IN (`+ph+`)
			ORDER BY sheet_index, cuePos`, args...); err != nil {
		return nil, err
	}
	return positions, nil
}

// ReparentCue is the legacy single-cue membership setter (context menu /
// older clients): place the cue at the end of the target group's span.
// Membership in the sequence model is positional, so "join" = move.
func ReparentCue(newPos int, parentGroupID int) error {
		return SheetDrop([]int{newPos}, 0, "group", parentGroupID, true, false, false, nil, 0)
}

// SetCueGroup assigns cue at cuePos to group (0 = top level) — the join
// gesture; the cue lands after the group's span end.
func SetCueGroup(cuePos string, groupID int) error {
	return setCueGroup(cuePos, groupID, false)
}

// SetCueGroupFirst is the expanded-header drop (§5.4): the cue becomes the
// group's FIRST member (right after the header).
func SetCueGroupFirst(cuePos string, groupID int) error {
	return setCueGroup(cuePos, groupID, true)
}

func setCueGroup(cuePos string, groupID int, first bool) error {
	pos, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	if groupID != 0 {
		if _, err := GetGroup(groupID); err != nil {
			return err
		}
		return SheetDrop([]int{pos}, 0, "group", groupID, true, false, first, nil, 0)
	}
	// Top level: drop before the first header (or at the end).
	beforeKind, beforeID := "", 0
	if seq, err := loadSheetSequence(); err == nil {
		for _, item := range seq {
			if item.Kind == "group" {
				beforeKind, beforeID = "group", item.GroupID
				break
			}
		}
	}
	return SheetDrop([]int{pos}, 0, beforeKind, beforeID, false, false, false, nil, 0)
}
