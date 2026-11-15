package ctp

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SelectUnit is one selectable entry in sheet order: a cue row or a group
// header (exported — the renderer's GO-bar preview walks the same list, so
// the preview can never disagree with what GO/Space fires). Collapsed groups
// contribute only their header; their members are unreachable.
type SelectUnit struct {
	CuePos  int  // >0 for cues, 0 for group headers
	GroupID int  // >0 for group headers, 0 for cues
	IsGroup bool // GroupID > 0
}

// FlatRow is one rendered (and keyboard-selectable) row of the cuesheet in
// visual order: a group header (Group != nil) or a cue (Cue != nil), at the
// given nesting depth (0 = top level). Shared by the cuesheet renderer and
// the keyboard-selection walk so the two can never disagree.
type FlatRow struct {
	Group *Group
	Cue   *Cue
	Depth int
	// GroupColor is the innermost containing group's colour ("" = none),
	// carried so member rows can draw the folder outline in it.
	GroupColor string
	// Group-boundary flags for the folder outline (§5.4): a cue is the
	// last rendered member of its direct group's run.
	LastInGroup bool
	// SpanDepth: depth of the innermost OPEN group span this row sits in
	// (-1 = none). The folder box draws one vertical line per open span,
	// so nested groups stack their left edges on deeper rows.
	SpanDepth int
	// Folder-box verticals: SpanBase colours the line at the cell's left
	// edge (the outermost open span); SpanShadows stacks one 1px line per
	// further open span, each in ITS OWN group's colour (never the cue's).
	SpanBase    string
	SpanShadows string
}

// groupAncestors returns the chain of parent group ids above gid (nearest
// first), guarded against parent-cycles and dangling parents.
func groupAncestors(gid int, byID map[int]Group) []int {
	var chain []int
	g, ok := byID[gid]
	for ok && g.ParentGroupID != 0 && len(chain) <= len(byID) {
		if chainContains(chain, g.ParentGroupID) {
			break // cycle: stop walking
		}
		chain = append(chain, g.ParentGroupID)
		g, ok = byID[g.ParentGroupID]
	}
	return chain
}

func chainContains(chain []int, id int) bool {
	for _, v := range chain {
		if v == id {
			return true
		}
	}
	return false
}

// cueInCollapsed reports whether the cue belongs to gid's subtree — a direct
// member or a member of a descendant subgroup. Only those rows hide when gid
// collapses; anything else parked inside the span renders at its position.
// Dangling parents (healed at startup) and parent-cycles never match: the
// walk is capped at the group count.
func cueInCollapsed(cue *Cue, gid int, byID map[int]Group) bool {
	for p, n := cue.Parent, 0; p != 0 && n <= len(byID); n++ {
		if p == gid {
			return true
		}
		g, ok := byID[p]
		if !ok {
			return false
		}
		p = g.ParentGroupID
	}
	return false
}

// FlattenSheet folds the MERGED visual sequence (cues and group headers by
// sheet_index) into rendered rows: a group header (Group != nil) or a cue
// (Cue != nil) at its nesting depth. A header OPENS a span; the next header
// of the same or shallower depth closes it. A cue renders inside the
// innermost open span only when its STORED parent equals that span's group —
// otherwise as a top-level gap row at its position. Read-only: never writes.
// Collapsing hides a group's subtree members only (header stays); unrelated
// rows parked inside the span keep rendering. Empty groups render wherever
// their header sits — every group position is real, nothing trails.
func FlattenSheet(cs *Cuesheet) []FlatRow {
	byID := make(map[int]Group, len(cs.Groups))
	for _, g := range cs.Groups {
		byID[g.GroupID] = g
	}
	hidden := make(map[int]bool, len(cs.Groups))
	depthOf := make(map[int]int, len(cs.Groups))
	for gid := range byID {
		chain := groupAncestors(gid, byID)
		depthOf[gid] = len(chain)
		for _, anc := range chain {
			if byID[anc].Collapse {
				hidden[gid] = true
				break
			}
		}
	}

	groups := make([]Group, len(cs.Groups))
	copy(groups, cs.Groups)
	sort.Slice(groups, func(i, j int) bool { return groups[i].SheetIndex < groups[j].SheetIndex })

	type item struct {
		kind string // "cue" | "group"
		cue  *Cue
		g    *Group
	}
	items := make([]item, 0, len(cs.Cues)+len(cs.Groups))
	gi := 0
	for _, cue := range cs.Cues {
		for gi < len(groups) && groups[gi].SheetIndex <= cue.SheetIndex {
			g := groups[gi]
			items = append(items, item{kind: "group", g: &g})
			gi++
		}
		c := cue
		items = append(items, item{kind: "cue", cue: &c})
	}
	for ; gi < len(groups); gi++ {
		g := groups[gi]
		items = append(items, item{kind: "group", g: &g})
	}

	rows := make([]FlatRow, 0, len(items))
	type span struct {
		groupID  int
		depth    int
		lastRow  int // index in rows of the last emitted row of this span
		skipping bool
	}
	var stack []span
	closeTo := func(depth int) {
		for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.lastRow >= 0 && top.lastRow < len(rows) {
				rows[top.lastRow].LastInGroup = true // bottom edge of the folder outline
			}
		}
	}
	// The innermost open (non-gapped) span's depth: one folder-box line is
	// drawn per open span, so a row's verticals run from depth 0 to this.
	// Gapped spans (stray rows parked inside them) draw nothing until the
	// span resumes (§6.4).
	spanDepth := func() int {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].lastRow != -1 {
				return stack[i].depth
			}
		}
		return -1
	}
	// The innermost open (non-gapped) span's depth: one folder-box line is
	// drawn per open span, so a row's verticals run from depth 0 to this.
	// Gapped spans (stray rows parked inside them) draw nothing until the
	// span resumes (§6.4).
	// spanPaint answers "what colour is each vertical on this row?": one
	// line per OPEN span, coloured by THAT span's group (uncoloured groups
	// fall back to the theme's success tone — never the cue's colour).
	// Returns the outermost line's colour and a box-shadow stack for the
	// deeper lines (1.1rem per level). Empty when no span is open.
	spanPaint := func() (string, string) {
		type paint struct {
			depth int
			color string
		}
		var open []paint
		for _, s := range stack {
			if s.lastRow == -1 {
				continue // gapped span: its box closes and resumes around strays
			}
			color := "var(--ctp-success, #42ddb2)"
			if g, ok := byID[s.groupID]; ok && g.Color != "" {
				color = g.Color
			}
			open = append(open, paint{depth: s.depth, color: color})
		}
		if len(open) == 0 {
			return "", ""
		}
		base := open[0].color
		var sb strings.Builder
		for k := 1; k < len(open); k++ {
			if k > 1 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%.1frem 0 0 %s", float64(open[k].depth)*1.1, open[k].color)
		}
		return base, sb.String()
	}
	for it := range items {
		if items[it].kind == "group" {
			g := items[it].g
			d := depthOf[g.GroupID]
			closeTo(d)
			// Header visible unless a PROPER ancestor is collapsed.
			if hidden[g.GroupID] {
				stack = append(stack, span{groupID: g.GroupID, depth: d, lastRow: -1, skipping: true})
				continue
			}
			rows = append(rows, FlatRow{Group: g, Depth: d, SpanDepth: d})
			// A collapsed group's own members are hidden too (not just
			// descendants of collapsed ancestors): the header stays, its span
			// skips. Without this, collapsing changed state but rendered
			// nothing — the button and arrow keys looked dead.
			stack = append(stack, span{groupID: g.GroupID, depth: d, lastRow: len(rows) - 1, skipping: g.Collapse})
			// Verticals for the header row: outer open spans plus its own.
			rows[len(rows)-1].SpanBase, rows[len(rows)-1].SpanShadows = spanPaint()
			continue
		}
		// A cue: member of the innermost open span.
		if len(stack) == 0 {
			rows = append(rows, FlatRow{Cue: items[it].cue, Depth: 0, SpanDepth: -1})
			continue
		}
		// The cue's STORED parent may belong to an outer open span (its
		// visual position sits after a nested subgroup): surface that span
		// by closing the inner spans first, or the cue leaks out of its
		// folder outline as a depth-0 stray (regression: the outline closed
		// around the subgroup and members after it drew no side borders).
		if items[it].cue.Parent != stack[len(stack)-1].groupID {
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].groupID == items[it].cue.Parent {
					closeTo(stack[i].depth + 1) // close strictly deeper spans; the parent stays open
					break
				}
			}
		}
		top := stack[len(stack)-1]
		if top.skipping && cueInCollapsed(items[it].cue, top.groupID, byID) {
			continue // member of a collapsed group's subtree: not rendered
		}
		// Literal membership (§6.4): the cue renders inside this span only
		// when its STORED parent is this span's group. Anything else —
		// top-level cues and strays whose group sits elsewhere — renders as
		// a plain top-level row at its position; the folder outline closes
		// above it and resumes after it. The renderer never rewrites.
		if items[it].cue.Parent != top.groupID {
			if top.lastRow >= 0 && top.lastRow < len(rows) {
				rows[top.lastRow].LastInGroup = true
			}
			// Gapping the innermost span first: the stray row's verticals
			// only cover spans that stay open (the closed one resumes after).
			top.lastRow = -1
			stack[len(stack)-1] = top
			base, shadows := spanPaint()
			rows = append(rows, FlatRow{Cue: items[it].cue, Depth: 0, SpanDepth: spanDepth(),
				SpanBase: base, SpanShadows: shadows})
			continue
		}
		// The innermost open span's colour draws the member's folder
		// outline (QLab-style); empty when the group has no colour.
		groupColor := ""
		if g, ok := byID[top.groupID]; ok {
			groupColor = g.Color
		}
		cue := items[it].cue
		// Members sit one level deeper than their group header (§6.4):
		// top-depth + 1 so the CSS indent formula puts the first level at
		// 1.5rem, +1.1rem per nesting level after.
		base, shadows := spanPaint()
		rows = append(rows, FlatRow{Cue: cue, Depth: top.depth + 1, GroupColor: groupColor,
			SpanDepth: spanDepth(), SpanBase: base, SpanShadows: shadows})
		// Every open span contains this row: nested subgroup headers and
		// member rows extend each enclosing span's visual last row, so the
		// folder outline and the selected block outline close at the span's
		// true end (not at the last direct member before a nested header).
		for i := range stack {
			if stack[i].lastRow >= 0 {
				stack[i].lastRow = len(rows) - 1
			}
		}
		top.lastRow = len(rows) - 1
		stack[len(stack)-1] = top
	}
	closeTo(-1) // close everything at the sequence end
	return rows
}

// SelectUnits returns the sheet's selectable entries in order: every rendered
// row (group headers at any nesting level, plus their visible cues). Users of
// collapsed groups are skipped entirely.
func SelectUnits() ([]SelectUnit, error) {
	cs, err := GetCuesheet()
	if err != nil {
		return nil, err
	}
	return SheetUnits(&cs), nil
}

// SheetUnits is SelectUnits for a sheet the caller already loaded (a render
// reads the sheet once, not once per helper).
func SheetUnits(cs *Cuesheet) []SelectUnit {
	rows := FlattenSheet(cs)
	units := make([]SelectUnit, 0, len(rows))
	for _, r := range rows {
		if r.Group != nil {
			units = append(units, SelectUnit{GroupID: r.Group.GroupID, IsGroup: true})
			continue
		}
		units = append(units, SelectUnit{CuePos: r.Cue.CuePos})
	}
	return units
}

// SelectionStepAt returns the current selection expressed on SelectUnits'
// index space (or -1 when nothing is selected).
func SelectUnitIndex() (int, error) {
	units, err := SelectUnits()
	if err != nil {
		return -1, err
	}
	return UnitIndex(units), nil
}

// UnitIndex is the current selection's index in units (-1 when nothing in
// them is selected).
func UnitIndex(units []SelectUnit) int {
	var selCue, selGroup int
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?;`, stateKeySelectedCue); err == nil {
		selInt, perr := strconv.Atoi(strings.TrimSpace(val))
		if perr == nil {
			if selInt < 0 {
				selGroup = -selInt
			} else {
				selCue = selInt
			}
		}
	}
	for i, u := range units {
		if selGroup != 0 && u.GroupID == selGroup {
			return i
		}
		if selGroup == 0 && u.CuePos == selCue {
			return i
		}
	}
	return -1
}

// ExtendSelection grows the anchor+set selection to cover every VISIBLE unit
// between the anchor and the target (SelectUnits order: visual, collapsed
// members excluded, headers included as -groupID). The anchor keeps its
// identity (cue pos or -groupID); everything else in the span joins the set.
// No anchor (or anchor/target off-sheet, e.g. after a re-render) falls back
// to a plain single-select of the target.
func ExtendSelection(targetCue, targetGroup int) error {
	units, err := SelectUnits()
	if err != nil {
		return err
	}
	ti := -1
	for i, u := range units {
		if targetGroup != 0 && u.IsGroup && u.GroupID == targetGroup {
			ti = i
			break
		}
		if targetGroup == 0 && !u.IsGroup && u.CuePos == targetCue {
			ti = i
			break
		}
	}
	ai, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	if ai < 0 || ti < 0 {
		if targetGroup != 0 {
			return SetSelectedGroup(targetGroup)
		}
		return SetCue(strconv.Itoa(targetCue))
	}
	var anchor int
	if sel, _ := SelectedCuePos(); sel != 0 {
		anchor = sel
	} else if gid, _ := SelectedGroupPos(); gid != 0 {
		anchor = -gid
	}
	lo, hi := ai, ti
	if lo > hi {
		lo, hi = hi, lo
	}
	set := make([]int, 0, hi-lo)
	for _, u := range units[lo : hi+1] {
		var id int
		if u.IsGroup {
			id = -u.GroupID
		} else {
			id = u.CuePos
		}
		if id != anchor {
			set = append(set, id)
		}
	}
	// Keep selection members outside the span (matches the old numeric-range
	// behaviour of accumulating, not replacing). Stale entries (cues/groups
	// deleted since they were selected, e.g. past a bulk delete) are pruned:
	// a dead position must not sneak back into a later bulk op.
	have := make(map[int]bool, len(set))
	for _, p := range set {
		have[p] = true
	}
	live, _ := selectedSetPruned()
	for _, p := range live {
		if !have[p] {
			set = append(set, p)
		}
	}
	return SetGroupSelection(anchor, set)
}

// selectedSetPruned returns the stored multi-selection minus entries whose
// row no longer exists (deleted cues/groups). Dead positions otherwise hide
// in the set until a later ExtendSelection/Bulk call resurrects them.
func selectedSetPruned() ([]int, error) {
	set := SelectedSet()
	if len(set) == 0 {
		return set, nil
	}
	out := make([]int, 0, len(set))
	for _, p := range set {
		if p < 0 {
			var g int
			if err := db.Get(&g, `SELECT group_id FROM cue_group WHERE group_id = ?`, -p); err == nil {
				out = append(out, p)
			}
			continue
		}
		var c int
		if err := db.Get(&c, `SELECT cuePos FROM cuesheet WHERE cuePos = ?`, p); err == nil {
			out = append(out, p)
		}
	}
	if len(out) != len(set) {
		_ = setSelectedSet(out)
	}
	return out, nil
}

// ExtendStep grows/shrinks the multi-selection one visible unit for
// Shift+arrows (dir +1 down, -1 up). The head is the selected unit farthest
// along dir; it steps one further — or back toward the anchor, shrinking the
// span. Stepping onto the anchor clears the set. No anchor: plain step.
func ExtendStep(dir int) error {
	units, err := SelectUnits()
	if err != nil {
		return err
	}
	ai, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	if ai < 0 {
		return SelectStep(dir)
	}
	idOf := func(u SelectUnit) int {
		if u.IsGroup {
			return -u.GroupID
		}
		return u.CuePos
	}
	at := make(map[int]int, len(units))
	for i, u := range units {
		at[idOf(u)] = i
	}
	var anchor int
	if sel, _ := SelectedCuePos(); sel != 0 {
		anchor = sel
	} else if gid, _ := SelectedGroupPos(); gid != 0 {
		anchor = -gid
	}
	// The head (focus end) isn't stored: take the selected unit farthest
	// from the anchor, preferring the stepping side on ties. Stepping back
	// toward the anchor shrinks the span; stepping onto it clears the set.
	head := ai
	best := -1
	for _, p := range SelectedSet() {
		i, ok := at[p]
		if !ok {
			continue
		}
		d := i - ai
		if d < 0 {
			d = -d
		}
		if d > best || d == best && (dir > 0 && i > head || dir < 0 && i < head) {
			best, head = d, i
		}
	}
	head += dir
	if head < 0 || head >= len(units) {
		return nil
	}
	if head == ai {
		return SetGroupSelection(anchor, nil)
	}
	lo, hi := ai, head
	if lo > hi {
		lo, hi = hi, lo
	}
	set := make([]int, 0, hi-lo)
	for _, u := range units[lo : hi+1] {
		if id := idOf(u); id != anchor {
			set = append(set, id)
		}
	}
	return SetGroupSelection(anchor, set)
}

// ExpandGroup uncollapses a group so a joined cue lands visibly instead of
// vanishing into a closed folder. No-op when already open.
func ExpandGroup(groupID int) error {
	_, err := db.Exec(`UPDATE cue_group SET collapse = 0 WHERE group_id = ?`, groupID)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// SelectStep moves the selection one unit forward/backward through
// SelectUnits()' list (groups and cues alike); clamps at the ends.
func SelectStep(dir int) (err error) {
	units, err := SelectUnits()
	if err != nil || len(units) == 0 {
		return err
	}
	idx, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	idx += dir
	if idx < 0 || idx >= len(units) {
		return nil
	}
	u := units[idx]
	if u.IsGroup {
		return setSelectedGroupPos(u.GroupID)
	}
	return setSelectedCuePos(u.CuePos)
}

// routes.autoContinueFrom), so only the query for that next cue remains.
// NextCuePos returns the next existing cue position strictly after endingPos
// in sheet order, or 0 (and nil) when endingPos is the last cue.
func NextCuePos(endingPos int) (int, error) {
	return nextSheetCue(endingPos, 1)
}
