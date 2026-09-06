package ctp

import (
	"database/sql"
	"log"
	"strconv"
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
	Shuffle       bool   `db:"shuffle"`
	Loop          bool   `db:"loop"`
	FadeMS        int    `db:"fade_ms"`
	DurationMS    int    `db:"duration_ms"`
}

// CreateGroup inserts a new cue group and returns its group_id.
func CreateGroup(name string, parentGroupID int) (int, error) {
	if name == "" {
		name = "New Group"
	}
	res, err := db.Exec(`
		INSERT INTO cue_group (name, parent_group_id) VALUES (?, ?)
	`, name, parentGroupID)
	if err != nil {
		log.Printf("Error creating cue group: %v", err)
		return 0, err
	}
	id, _ := res.LastInsertId()
	// Collapse state is presentation, but the cuesheet render needs it now.
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
	_, err := db.Exec(`
		UPDATE cue_group SET name = ?, parent_group_id = ?, collapse = ?,
			slideshow = ?, shuffle = ?, loop = ?, fade_ms = ?, duration_ms = ?
		WHERE group_id = ?
	`, g.Name, g.ParentGroupID, boolInt(g.Collapse), boolInt(g.Slideshow),
		boolInt(g.Shuffle), boolInt(g.Loop), g.FadeMS, g.DurationMS, g.GroupID)
	if err != nil {
		log.Printf("Error updating cue group: %v", err)
		return err
	}
	bumpCuesheetVersion()
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
	bumpCuesheetVersion()
	return nil
}

// SetCueGroup assigns cue at cuePos to group (0 = top level) and moves it
// right after the group's current last member, keeping membership contiguous
// in flat cuePos order (the render model groups by contiguous runs).
func SetCueGroup(cuePos string, groupID int) error {
	pos, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	if groupID != 0 {
		if _, err := GetGroup(groupID); err != nil {
			return err
		}
	}

	// Remember the group's last member so the cue lands after it (this runs
	// before the reorder; ReorderCues reindexes 1..N and remaps the selection).
	var lastMember sql.NullInt64
	if err := db.Get(&lastMember, `
		SELECT MAX(cuePos) FROM cuesheet WHERE parent = ? AND cuePos != ?
	`, groupID, pos); err != nil {
		return err
	}
	insertAfter := int(lastMember.Int64)

	_, err = db.Exec(`UPDATE cuesheet SET parent = ? WHERE cuePos = ?`, groupID, pos)
	if err != nil {
		log.Printf("Error setting cue group: %v", err)
		return err
	}

	var order []int
	if err := db.Select(&order, `SELECT cuePos FROM cuesheet ORDER BY cuePos`); err != nil {
		return err
	}
	// Rebuild the flat order with pos removed, then reinsert it right after
	// the group's last member (front of the list when the group is empty).
	without := make([]int, 0, len(order)-1)
	for _, p := range order {
		if p != pos {
			without = append(without, p)
		}
	}
	idx := -1
	for i, p := range without {
		if p == insertAfter {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = 0
	}
	reordered := make([]int, 0, len(order))
	reordered = append(reordered, without[:idx]...)
	reordered = append(reordered, pos)
	reordered = append(reordered, without[idx:]...)
	return ReorderCues(reordered)
}

// GroupCuePositions lists a group's member cue positions in sheet order.
func GroupCuePositions(groupID int) ([]int, error) {
	var positions []int
	if err := db.Select(&positions, `
		SELECT cuePos FROM cuesheet WHERE parent = ? ORDER BY cuePos
	`, groupID); err != nil {
		return nil, err
	}
	return positions, nil
}