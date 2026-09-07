package ctp

import (
	"database/sql"
	"fmt"
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
	} else {
		idx++ // land AFTER the group's last member, not before it
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

// normalizeGroupMembership maintains the render invariant: every group's
// members are contiguous in flat cuePos order. buildSheetRows clusters by
// contiguous parent runs, so without this a drag-reorder or move that lands
// a cue inside/away from a group would render the group as two same-named
// folders sharing one identity, or leave a stray indented row behind.
//
// Membership follows placement, and the caller says which cues moved:
//   - a moved cue placed between two members of one group joins it (dropping
//     into a folder's span), whatever its previous group;
//   - a moved member that no longer touches its own group leaves it — unless
//     it is that group's only member (a lone member IS the group; dragging
//     it around relocates the group rather than deleting it);
//   - bystander runs are only touched by the structural pass: a run
//     sandwiched between two runs of one group joins it, and a group run
//     separated from its group leaves (edge fragment first, when the group
//     was split in two).
//
// Runs to fixpoint, since joins can cascade ([G 0 0 G] needs two rounds).
func normalizeGroupMembership(moved ...int) error {
	// Phase 1: operator intent, applied to the moved cues' NEW positions.
	// Joins run to fixpoint before any leave: in a swap, the cue placed
	// between two members of a group must join before the displaced cue is
	// judged "separated" (its neighbour may just have joined).
	join := func() (bool, error) {
		changed := false
		for _, mpos := range moved {
			if mpos <= 0 {
				continue
			}
			var p int
			if err := db.Get(&p, `SELECT parent FROM cuesheet WHERE cuePos = ?`, mpos); err != nil {
				return false, err
			}
			prevP, nextP := -1, -1 // -1 = no neighbour on that side
			var v int
			if err := db.Get(&v, `SELECT parent FROM cuesheet WHERE cuePos < ? ORDER BY cuePos DESC LIMIT 1`, mpos); err == nil {
				prevP = v
			}
			if err := db.Get(&v, `SELECT parent FROM cuesheet WHERE cuePos > ? ORDER BY cuePos LIMIT 1`, mpos); err == nil {
				nextP = v
			}
			if prevP > 0 && prevP == nextP && p != prevP {
				if _, err := db.Exec(`UPDATE cuesheet SET parent = ? WHERE cuePos = ?`, prevP, mpos); err != nil {
					return false, err
				}
				changed = true
			}
		}
		return changed, nil
	}
	for {
		c, err := join()
		if err != nil {
			return err
		}
		if !c {
			break
		}
	}
	for _, mpos := range moved {
		if mpos <= 0 {
			continue
		}
		var p int
		if err := db.Get(&p, `SELECT parent FROM cuesheet WHERE cuePos = ?`, mpos); err != nil {
			return err
		}
		prevP, nextP := -1, -1
		var v int
		if err := db.Get(&v, `SELECT parent FROM cuesheet WHERE cuePos < ? ORDER BY cuePos DESC LIMIT 1`, mpos); err == nil {
			prevP = v
		}
		if err := db.Get(&v, `SELECT parent FROM cuesheet WHERE cuePos > ? ORDER BY cuePos LIMIT 1`, mpos); err == nil {
			nextP = v
		}
		// Separated from the own group — but a lone member stays: the group
		// travels with its only cue.
		if p != 0 && prevP != p && nextP != p {
			var n int
			if err := db.Get(&n, `SELECT COUNT(*) FROM cuesheet WHERE parent = ?`, p); err != nil {
				return err
			}
			if n >= 2 {
				if _, err := db.Exec(`UPDATE cuesheet SET parent = 0 WHERE cuePos = ?`, mpos); err != nil {
					return err
				}
			}
		}
	}

	// Phase 2: structural cleanup to fixpoint (covers bystanders and any
	// legacy non-contiguous data).
	for round := 0; ; round++ {
		if round > 100 {
			return fmt.Errorf("normalizeGroupMembership: did not converge")
		}
		var rows []struct {
			CuePos int `db:"cuePos"`
			Parent int `db:"parent"`
		}
		if err := db.Select(&rows, `SELECT cuePos, parent FROM cuesheet ORDER BY cuePos`); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		// Contiguous parent runs.
		type run struct {
			start, end int // row indexes, inclusive
			parent     int
		}
		var runs []run
		for i := range rows {
			if i == 0 || rows[i].Parent != rows[i-1].Parent {
				runs = append(runs, run{i, i, rows[i].Parent})
				continue
			}
			runs[len(runs)-1].end = i
		}

		changed := false
		apply := func(from, to, parent int) {
			for i := from; i <= to; i++ {
				if _, err := db.Exec(`UPDATE cuesheet SET parent = ? WHERE cuePos = ?`, parent, rows[i].CuePos); err != nil {
					log.Printf("normalizeGroupMembership: update cue %d: %v", rows[i].CuePos, err)
				}
			}
			changed = true
		}

		// Rule 1 (join) first: a run sandwiched between two runs of one group
		// belongs to that group, whatever its current parent — the operator
		// placed it inside the group's span. (Joining merges runs, so this
		// takes priority over the leave rule below.)
		for idx, r := range runs {
			if idx == 0 || idx+1 == len(runs) {
				continue
			}
			prev, next := &runs[idx-1], &runs[idx+1]
			if prev.parent != 0 && prev.parent == next.parent && r.parent != prev.parent {
				apply(r.start, r.end, prev.parent)
				break
			}
		}
		// Rule 2 (leave): a group run separated from the rest of its group.
		// When the group is split in two, the EDGE fragment is taken to be
		// the one that was dragged away (dropping a cue at the sheet's head
		// or tail); a fragment stranded mid-sheet between other groups also
		// leaves, since it can neither join its neighbours nor teleport back.
		if !changed {
			pickIdx := -1
			for idx, r := range runs {
				if r.parent == 0 {
					continue
				}
				displaced := false
				for j, other := range runs {
					if j != idx && other.parent == r.parent {
						displaced = true
						break
					}
				}
				if !displaced {
					continue
				}
				if idx == 0 || idx == len(runs)-1 {
					pickIdx = idx // edge fragment wins immediately
					break
				}
				if pickIdx == -1 {
					pickIdx = idx // first mid-sheet candidate as fallback
				}
			}
			if pickIdx != -1 {
				apply(runs[pickIdx].start, runs[pickIdx].end, 0)
			}
		}
		if !changed {
			return nil
		}
	}
}
