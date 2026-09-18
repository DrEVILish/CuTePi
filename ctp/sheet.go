package ctp

// Sheet model v2 — two stored facts, each written explicitly by the op that
// changes it, read literally by the renderer:
//
//   - `sheet_index` (cues + group headers): visual order only.
//   - `cuesheet.parent`: group membership only.
//
// Membership is NEVER re-derived: the op that places a cue (drop, insert,
// move, bulk-assign) computes its parent once — via gapOwner below for gap
// placements, or the drop target for joins — and stores it. The renderer
// (FlattenSheet) reads both facts literally and performs zero writes, so the
// DB and the UI cannot disagree. cuePos stays the cue's stable identity
// (selection, gsp, audit); sheet_index is the playback/render order. Group
// headers carry their own sheet_index, so an empty group is draggable.

import (
	"fmt"
	"log"
)

// SheetItem is one row of the visual sequence.
type SheetItem struct {
	Kind    string // "cue" | "group"
	CuePos  int    // Kind == cue
	GroupID int    // Kind == group
}

const sheetIndexStep = 1000.0

// loadSheetSequence reads the merged visual order from the DB.
func loadSheetSequence() ([]SheetItem, error) {
	var cueRows []struct {
		CuePos     int     `db:"cuePos"`
		SheetIndex float64 `db:"sheet_index"`
	}
	if err := db.Select(&cueRows, `SELECT cuePos, sheet_index FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return nil, err
	}
	var groupRows []struct {
		GroupID    int     `db:"group_id"`
		SheetIndex float64 `db:"sheet_index"`
	}
	if err := db.Select(&groupRows, `SELECT group_id, sheet_index FROM cue_group ORDER BY sheet_index, group_id`); err != nil {
		return nil, err
	}
	seq := make([]SheetItem, 0, len(cueRows)+len(groupRows))
	ci, gi := 0, 0
	for ci < len(cueRows) || gi < len(groupRows) {
		if gi >= len(groupRows) || (ci < len(cueRows) && cueRows[ci].SheetIndex <= groupRows[gi].SheetIndex) {
			seq = append(seq, SheetItem{Kind: "cue", CuePos: cueRows[ci].CuePos})
			ci++
		} else {
			seq = append(seq, SheetItem{Kind: "group", GroupID: groupRows[gi].GroupID})
			gi++
		}
	}
	return seq, nil
}

// groupDepth returns a group's nesting depth via its ancestors.
func groupDepth(groupID int, byID map[int]Group) int {
	return len(groupAncestors(groupID, byID))
}

// groupNesting loads every group plus each group's depth in the declared
// parent_group_id tree. Pure read, shared by gap ownership and rendering.
func groupNesting() (byID map[int]Group, depths map[int]int, err error) {
	var groupRows []Group
	if err := db.Select(&groupRows, `SELECT * FROM cue_group`); err != nil {
		return nil, nil, err
	}
	byID = map[int]Group{}
	for _, g := range groupRows {
		byID[g.GroupID] = g
	}
	depths = map[int]int{}
	var depthOf func(int, int) int
	depthOf = func(id int, guard int) int {
		if d, ok := depths[id]; ok {
			return d
		}
		g, ok := byID[id]
		if !ok || guard > len(byID) {
			return 0
		}
		d := 0
		if g.ParentGroupID != 0 {
			d = depthOf(g.ParentGroupID, guard+1) + 1
		}
		depths[id] = d
		return d
	}
	for id := range byID {
		depths[id] = depthOf(id, 0)
	}
	return byID, depths, nil
}

// gapOwner answers the one membership question a gap placement asks: which
// group owns a cue inserted at index i of seq? The innermost header
// preceding i whose span contains i — a header's span runs to the next
// header of the same or shallower depth. Pure (no writes); the caller stores
// the answer. Index len(seq) (append at end) is valid.
func gapOwner(seq []SheetItem, i int) (int, error) {
	_, depths, err := groupNesting()
	if err != nil {
		return 0, err
	}
	type span struct {
		groupID int
		depth   int
	}
	var stack []span
	if i > len(seq) {
		i = len(seq)
	}
	for _, item := range seq[:i] {
		if item.Kind != "group" {
			continue
		}
		d := depths[item.GroupID]
		for len(stack) > 0 && stack[len(stack)-1].depth >= d {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, span{item.GroupID, d})
	}
	if len(stack) == 0 {
		return 0, nil
	}
	return stack[len(stack)-1].groupID, nil
}

// storedParents reads the current membership map (cuePos -> parent).
func storedParents() (map[int]int, error) {
	var rows []struct {
		CuePos int `db:"cuePos"`
		Parent int `db:"parent"`
	}
	if err := db.Select(&rows, `SELECT cuePos, parent FROM cuesheet`); err != nil {
		return nil, err
	}
	out := make(map[int]int, len(rows))
	for _, r := range rows {
		out[r.CuePos] = r.Parent
	}
	return out, nil
}

// applyOrder persists exactly what it is given, in one transaction:
// sheet_index = i*1000 for every row, cuesheet.parent = parents[cuePos].
// parents must cover every cue in seq. Membership is stored, never derived.
func applyOrder(seq []SheetItem, parents map[int]int) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, item := range seq {
		if item.Kind == "group" {
			if _, err := tx.Exec(`UPDATE cue_group SET sheet_index = ? WHERE group_id = ?`,
				float64(i)*sheetIndexStep, item.GroupID); err != nil {
				return err
			}
			continue
		}
		p, ok := parents[item.CuePos]
		if !ok {
			return fmt.Errorf("applyOrder: no parent for cue %d", item.CuePos)
		}
		if _, err := tx.Exec(`UPDATE cuesheet SET sheet_index = ?, parent = ? WHERE cuePos = ?`,
			float64(i)*sheetIndexStep, p, item.CuePos); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// Adjacency is directional: a group's block opens at its header and runs
// downward, so a cue counts as inside the run only through the correct side.
// belongsBefore: the row above the cue is part of gid's block (its header, a
// stored member, or a row of its subtree). belongsAfter: the row below keeps
// the cue inside the run — a stored member or a subgroup header — but never
// gid's own header, which OPENS the block: a cue parked directly above its
// group's header sits outside the folder and leaves it.
func belongsBefore(it SheetItem, gid int, parents map[int]int, descendants map[int]map[int]bool) bool {
	if it.Kind == "group" {
		return it.GroupID == gid || descendants[gid][it.GroupID]
	}
	p, ok := parents[it.CuePos]
	if !ok {
		return false
	}
	if p == gid {
		return true
	}
	return descendants[gid][p]
}

func belongsAfter(it SheetItem, gid int, parents map[int]int, descendants map[int]map[int]bool) bool {
	if it.Kind == "group" {
		return it.GroupID != gid && descendants[gid][it.GroupID]
	}
	return parents[it.CuePos] == gid
}

// healSeparated enforces the contiguity invariant after full-order replaces:
// a cue stored with parent G≠0 that ends up with no adjacent belonging row
// leaves the group (parent→0) — it was separated from its run. A group's
// lone member never leaves (the group travels with its only cue). Runs to
// fixpoint over scope only; untouched cues are never modified.
func healSeparated(seq []SheetItem, scope []int) error {
	_, _, err := groupNesting()
	if err != nil {
		return err
	}
	desc := groupDescendants()
	inScope := map[int]bool{}
	for _, p := range scope {
		inScope[p] = true
	}
	for {
		parents, err := storedParents()
		if err != nil {
			return err
		}
		counts := map[int]int{}
		for _, p := range parents {
			if p != 0 {
				counts[p]++
			}
		}
		// Sequence positions of every cue in the CURRENT order.
		posOf := map[int]int{}
		for i, it := range seq {
			if it.Kind == "cue" {
				posOf[it.CuePos] = i
			}
		}
		changed := false
		for cuePos := range inScope {
			p, ok := parents[cuePos]
			if !ok || p == 0 || counts[p] <= 1 {
				continue
			}
			i, ok := posOf[cuePos]
			if !ok {
				continue
			}
			adjacent := false
			if i > 0 && belongsBefore(seq[i-1], p, parents, desc) {
				adjacent = true
			}
			if i+1 < len(seq) && belongsAfter(seq[i+1], p, parents, desc) {
				adjacent = true
			}
			if !adjacent {
				if _, err := db.Exec(`UPDATE cuesheet SET parent = 0 WHERE cuePos = ?`, cuePos); err != nil {
					return err
				}
				changed = true
			}
		}
		if !changed {
			return nil
		}
		bumpCuesheetVersion()
	}
}

// HealSheet repairs dangling state from older builds at startup (called next
// to MarkMissingFiles): parents pointing at deleted groups go to 0, and rows
// missing an index are appended at the end. It never judges membership —
// only broken references.
func HealSheet() error {
	var groups []int
	if err := db.Select(&groups, `SELECT group_id FROM cue_group`); err != nil {
		return err
	}
	have := map[int]bool{}
	for _, g := range groups {
		have[g] = true
	}
	var bad []int
	if err := db.Select(&bad, `SELECT cuePos FROM cuesheet WHERE parent != 0`); err != nil {
		return err
	}
	for _, cuePos := range bad {
		var p int
		if err := db.Get(&p, `SELECT parent FROM cuesheet WHERE cuePos = ?`, cuePos); err != nil {
			return err
		}
		if !have[p] {
			if _, err := db.Exec(`UPDATE cuesheet SET parent = 0 WHERE cuePos = ?`, cuePos); err != nil {
				return err
			}
			log.Printf("HealSheet: cue %d pointed at missing group %d, released to top level", cuePos, p)
		}
	}
	var maxIdx float64
	if err := db.Get(&maxIdx, `SELECT COALESCE(MAX(sheet_index), 0) FROM (
		SELECT sheet_index FROM cuesheet
		UNION ALL
		SELECT sheet_index FROM cue_group
	)`); err != nil {
		return err
	}
	next := maxIdx + sheetIndexStep
	var unindexed []int
	if err := db.Select(&unindexed, `SELECT cuePos FROM cuesheet WHERE sheet_index IS NULL OR sheet_index = 0`); err != nil {
		return err
	}
	for _, cuePos := range unindexed {
		if _, err := db.Exec(`UPDATE cuesheet SET sheet_index = ? WHERE cuePos = ?`, next, cuePos); err != nil {
			return err
		}
		next += sheetIndexStep
	}
	var unindexedGroups []int
	if err := db.Select(&unindexedGroups, `SELECT group_id FROM cue_group WHERE sheet_index IS NULL OR sheet_index = 0`); err != nil {
		return err
	}
	for _, gid := range unindexedGroups {
		if _, err := db.Exec(`UPDATE cue_group SET sheet_index = ? WHERE group_id = ?`, next, gid); err != nil {
			return err
		}
		next += sheetIndexStep
	}
	if len(unindexed)+len(unindexedGroups) > 0 {
		bumpCuesheetVersion()
	}
	return nil
}

// SheetDrop performs one literal drag: move the dragged cue(s) or group
// block to the gap before the target row (or to the end of the sheet).
// join=true means the drop landed ON a group header: the dragged rows are
// placed inside that group's span (after its last row) instead. Membership
// then follows from the sequence — the line is the truth.
// joinFirst (with join) puts the rows right after the header instead, i.e.
// as the group's FIRST cues (§5.4: drop on the lower half of an expanded
// header). forceTop (cue drops only) pins the dropped cues top-level
// (parent=0) even when the gap sits inside a group span: the "drop BETWEEN
// the groups" gesture. Without it a gap drop re-derives (a stale explicit-top
// mark on a moved cue is cleared, so joining works after gap-dropping).
// SheetDrop places the dragged rows at the exact gap the drop line showed,
// with the band's membership (§5.4): explicit parent (nil keeps the legacy
// derived semantics for grid-internal callers), after = slot after that cue
// (member-slot drops), else the beforeKind/beforeID gap. parent > 0 joins
// the group (expands it); for a dragged group parent == 0 detaches it to
// top level and parent > 0 nests it under that group.
func SheetDrop(cues []int, groupID int, beforeKind string, beforeID int, join bool, forceTop bool, joinFirst bool, parent *int, after int) error {
	seq, err := loadSheetSequence()
	if err != nil {
		return err
	}

	// Collect the dragged slice: cues in the given order (block), or the
	// group's whole block — its header, descendant subgroup headers (stored
	// parent_group_id chain) and every cue stored with a parent in that set,
	// in sequence order. Membership is stored, so the block is read from the
	// DB, not inferred from spans.
	isGroup := groupID != 0
	dragSet := map[string]bool{}
	var dragged []SheetItem
	if isGroup {
		var groupRows []Group
		if err := db.Select(&groupRows, `SELECT * FROM cue_group`); err != nil {
			return err
		}
		byID := map[int]Group{}
		for _, g := range groupRows {
			byID[g.GroupID] = g
		}
		if _, ok := byID[groupID]; !ok {
			return fmt.Errorf("group %d not found", groupID)
		}
		inBlock := map[int]bool{groupID: true}
		for id, g := range byID {
			for at := g.ParentGroupID; at != 0; {
				if at == groupID {
					inBlock[id] = true
					break
				}
				pg, ok := byID[at]
				if !ok {
					break
				}
				at = pg.ParentGroupID
			}
		}
		parents, err := storedParents()
		if err != nil {
			return err
		}
		for _, item := range seq {
			if item.Kind == "group" {
				if inBlock[item.GroupID] {
					dragged = append(dragged, item)
					dragSet["g"+fmt.Sprint(item.GroupID)] = true
				}
				continue
			}
			if inBlock[parents[item.CuePos]] {
				dragged = append(dragged, item)
				dragSet["c"+fmt.Sprint(item.CuePos)] = true
			}
		}
	} else {
		for _, p := range cues {
			dragged = append(dragged, SheetItem{Kind: "cue", CuePos: p})
			dragSet["c"+fmt.Sprint(p)] = true
		}
	}
	if len(dragged) == 0 {
		return nil
	}

	// Remove the dragged rows from the sequence.
	rest := make([]SheetItem, 0, len(seq))
	for _, item := range seq {
		key := "c" + fmt.Sprint(item.CuePos)
		if item.Kind == "group" {
			key = "g" + fmt.Sprint(item.GroupID)
		}
		if !dragSet[key] {
			rest = append(rest, item)
		}
	}

	// Find the insertion index for the gap "before the target row".
	at := len(rest) // end of sheet
	if beforeKind == "cue" {
		for i, item := range rest {
			if item.Kind == "cue" && item.CuePos == beforeID {
				at = i
				break
			}
		}
	} else if beforeKind == "group" {
		for i, item := range rest {
			if item.Kind == "group" && item.GroupID == beforeID {
				at = i
				break
			}
		}
	}

	// JOIN: place inside the target group's span, after its last current member.
	// joinFirst instead drops right after the header (first cue in group).
	// Either way the target expands: a joined cue must land visibly, never
	// vanish into a closed folder.
	// Explicit contract (client drops §5.4): parent set → that group wins
	// the membership outright; after > 0 (or beforeKind cue) pins the slot.
	if parent != nil && *parent != 0 {
		target := *parent
		if isGroup {
			if err := ValidateGroupParent(groupID, target); err != nil {
				return err
			}
		}
		if err := ExpandGroup(target); err != nil {
			return err
		}
	}
	if parent != nil && *parent == 0 && isGroup {
		if _, err := db.Exec(`UPDATE cue_group SET parent_group_id = 0 WHERE group_id = ?`, groupID); err != nil {
			return err
		}
	}
	if join && beforeKind == "group" {
		if err := ExpandGroup(beforeID); err != nil {
			return err
		}
	}
	if join && beforeKind == "group" && joinFirst {
		for i, item := range rest {
			if item.Kind == "group" && item.GroupID == beforeID {
				at = i + 1
				break
			}
		}
		if isGroup {
			if err := ValidateGroupParent(groupID, beforeID); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE cue_group SET parent_group_id = ? WHERE group_id = ?`, beforeID, groupID); err != nil {
				return err
			}
		}
	} else if join && beforeKind == "group" {
		// Find the last cue in 'rest' that currently has parent == beforeID.
		// This ensures new members are added to the end of the group's
		// existing member block, not at the end of the group's visual span
		// (which extends to the next group header or end of sheet).
		lastMemberIdx := -1
		for i, item := range rest {
			if item.Kind == "cue" {
				var p int
				if err := db.Get(&p, `SELECT parent FROM cuesheet WHERE cuePos = ?`, item.CuePos); err == nil && p == beforeID {
					lastMemberIdx = i
				}
			}
		}
		if lastMemberIdx >= 0 {
			at = lastMemberIdx + 1
		} else {
			// Group is empty: insert right after the header.
			for i, item := range rest {
				if item.Kind == "group" && item.GroupID == beforeID {
					at = i + 1
					break
				}
			}
		}
		// Group dragged into another group: nest it.
		if isGroup {
			if err := ValidateGroupParent(groupID, beforeID); err != nil {
				return err
			}
			if _, err := db.Exec(`UPDATE cue_group SET parent_group_id = ? WHERE group_id = ?`, beforeID, groupID); err != nil {
				return err
			}
		}
	}

	// Member-slot drops: after a specific cue wins over the end-of-group
	// default. Not reached by group moves after cuePos anchoring.
	if after > 0 {
		for i, item := range rest {
			if item.Kind == "cue" && item.CuePos == after {
				at = i + 1
				break
			}
		}
	}

	// Splice.
	out := make([]SheetItem, 0, len(rest)+len(dragged))
	out = append(out, rest[:at]...)
	out = append(out, dragged...)
	out = append(out, rest[at:]...)

	// Membership, stored explicitly with the move. A group block keeps its
	// internal membership (members travel); only cue drops set parents:
	// forceTop pins top-level, joins take the target, gap drops take the
	// owner of their new gap.
	parents, err := storedParents()
	if err != nil {
		return err
	}
	if !isGroup {
		// New sequence index of each dragged cue.
		posOf := map[int]int{}
		for i, item := range out {
			if item.Kind == "cue" {
				posOf[item.CuePos] = i
			}
		}
		if parent != nil {
			// Explicit band membership (§5.4): header joins, member-slot
			// joins and top-level gap drops all send it. No gap inference.
			for _, item := range dragged {
				if item.Kind == "cue" {
					parents[item.CuePos] = *parent
				}
			}
		} else {
			for _, item := range dragged {
				if item.Kind != "cue" {
					continue
				}
				switch {
				case forceTop:
					parents[item.CuePos] = 0
				case join && beforeKind == "group":
					parents[item.CuePos] = beforeID
				default:
					owner, err := gapOwner(out, posOf[item.CuePos])
					if err != nil {
						return err
					}
					parents[item.CuePos] = owner
				}
			}
		}
	}
	if err := applyOrder(out, parents); err != nil {
		return err
	}
	log.Printf("sheet: dropped %d row(s) (group=%v) before %s/%d join=%v forceTop=%v parent=%v after=%d", len(dragged), isGroup, beforeKind, beforeID, join, forceTop, parent, after)
	return nil
}

// MoveSheetCue swaps a cue with the neighbouring cue in visual order
// (move up/down). Each swapped cue takes the owner of its new slot, so
// crossing a group header changes membership and staying inside keeps it.
func MoveSheetCue(cuePos int, dir int) error {
	seq, err := loadSheetSequence()
	if err != nil {
		return err
	}
	// Cue rows only, in visual order.
	var cueIdxs []int
	for i, item := range seq {
		if item.Kind == "cue" {
			cueIdxs = append(cueIdxs, i)
		}
	}
	for i, idx := range cueIdxs {
		if seq[idx].CuePos != cuePos {
			continue
		}
		j := i + dir
		if j < 0 || j >= len(cueIdxs) {
			return nil // clamped at the ends
		}
		a, b := cueIdxs[i], cueIdxs[j]
		otherCuePos := seq[b].CuePos
		seq[a], seq[b] = seq[b], seq[a]

		parents, err := storedParents()
		if err != nil {
			return err
		}
		for _, mv := range []struct {
			pos int
			at  int
		}{{cuePos, b}, {otherCuePos, a}} {
			owner, err := gapOwner(seq, mv.at)
			if err != nil {
				return err
			}
			parents[mv.pos] = owner
		}
		if err := applyOrder(seq, parents); err != nil {
			return err
		}
		// If the moved cue was selected, move selection to the other cue
		// so the selection stays on the same visual row.
		if sel, _ := SelectedCuePos(); sel == cuePos {
			_ = setSelectedCuePos(otherCuePos)
		}
		return nil
	}
	return nil
}

// NextSheetCue / PrevSheetCue walk cues in VISUAL order (the auto-continue
// chain and arrow navigation follow what the operator sees).
func nextSheetCue(cuePos int, dir int) (int, error) {
	var rows []struct {
		CuePos     int     `db:"cuePos"`
		SheetIndex float64 `db:"sheet_index"`
	}
	if err := db.Select(&rows, `SELECT cuePos, sheet_index FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return 0, err
	}
	for i, r := range rows {
		if r.CuePos != cuePos {
			continue
		}
		j := i + dir
		if j < 0 || j >= len(rows) {
			return 0, nil
		}
		return rows[j].CuePos, nil
	}
	return 0, nil
}
