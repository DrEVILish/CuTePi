package routes

import (
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/gsp"
)

// GoBar is the top-of-sheet trigger strip (§12.1): what GO fires (the
// selected unit — cue or group) and the unit that follows it.
type GoBar struct {
	HasSel     bool
	SelIsGroup bool
	SelNum     string // cueNum for cues, group cue_num for groups
	SelTitle   string // cue title or group name
	HasNext    bool
	NextNum    string
	NextTitle  string
}

// computeGoBar derives the strip from the shared selection walk, so the
// preview can never disagree with what Space/GO actually fires.
func computeGoBar(sheet *ctp.Cuesheet) GoBar {
	units, err := ctp.SelectUnits()
	if err != nil {
		return GoBar{}
	}
	idx, err := ctp.SelectUnitIndex()
	if err != nil || idx < 0 || idx >= len(units) {
		return GoBar{}
	}
	describe := func(u ctp.SelectUnit) (string, string) {
		if u.IsGroup {
			for _, g := range sheet.Groups {
				if g.GroupID == u.GroupID {
					return g.CueNum, g.Name
				}
			}
			return "", ""
		}
		for _, cue := range sheet.Cues {
			if cue.CuePos == u.CuePos {
				return cue.CueNum, cue.Title
			}
		}
		return "", ""
	}
	var bar GoBar
	bar.HasSel = true
	bar.SelIsGroup = units[idx].IsGroup
	bar.SelNum, bar.SelTitle = describe(units[idx])
	if idx+1 < len(units) {
		bar.HasNext = true
		bar.NextNum, bar.NextTitle = describe(units[idx+1])
	}
	return bar
}

// SheetRow is one rendered row of the cuesheet: a group header at any
// nesting level, or a single cue. Built by folding ctp.FlattenSheet, which
// both the renderer and the keyboard walk share. Depth (0 = top level) drives
// the row indentation via the --depth custom property.
type SheetRow struct {
	Group        *ctp.Group
	Cue          *ctp.Cue
	Depth        int
	Selected     bool
	MemberCount  int    // direct members of this row's group (header rows)
	GroupColor   string // innermost containing group's colour (member rows)
	LastInGroup  bool   // folder outline flags (§5.4)
	SpanDepth    int    // innermost open folder span (folder-box verticals)
	SpanBase     string // outermost vertical's colour (its own group's)
	SpanShadows  string // one stacked vertical per deeper open span
}

// sheetRowsWithSelection folds the sheet and flags the row that the persisted
// selection points at, so the template can highlight it.
func sheetRowsWithSelection(sheet *ctp.Cuesheet) []SheetRow {
	flat := ctp.FlattenSheet(sheet)
	rows := make([]SheetRow, 0, len(flat))
	members := make(map[int]int, len(sheet.Groups))
	for _, cue := range sheet.Cues {
		members[cue.Parent]++
	}
	for _, r := range flat {
		row := SheetRow{Group: r.Group, Cue: r.Cue, Depth: r.Depth,
			GroupColor: r.GroupColor,
			LastInGroup: r.LastInGroup,
			SpanDepth:   r.SpanDepth,
			SpanBase:    r.SpanBase, SpanShadows: r.SpanShadows}
		if r.Group != nil {
			row.MemberCount = members[r.Group.GroupID]
		}
		rows = append(rows, row)
	}
	// Anchor header plus any headers in the multi-selection set (-groupID):
	// both highlight; cue-only consumers (bulk, drag, F8) ignore negatives.
	inSet := make(map[int]bool, len(ctp.SelectedSet()))
	for _, p := range ctp.SelectedSet() {
		inSet[p] = true
	}
	gid, err := ctp.SelectedGroupPos()
	if err != nil {
		return rows
	}
	for i := range rows {
		if rows[i].Group == nil {
			continue
		}
		if rows[i].Group.GroupID == gid || inSet[-rows[i].Group.GroupID] {
			rows[i].Selected = true
		}
	}
	return rows
}

// Groups implements cue-group routes: CRUD, membership, collapse, and the
// slideshow/playlist trigger. Group membership never changes playback order -
// it only rides the flat cuePos order.
func Groups(rg *gin.RouterGroup) {
	rg.POST("/group/add", func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		parent, _ := strconv.Atoi(c.PostForm("parentGroupID"))
		id, err := ctp.CreateGroup(name, parent)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		// The new group is the selection, so the operator renames it
		// (double-click) right away; the context-menu creation flow relies
		// on this discoverable inline edit.
		_ = ctp.SetSelectedGroup(id)
		renderCuesheet(c)
	})

	rg.DELETE("/group/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		if err := ctp.DeleteGroupWithCues(id); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	rg.PUT("/group/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		if name := strings.TrimSpace(c.PostForm("name")); name != "" {
			g.Name = name
		}
		if col := strings.TrimSpace(c.PostForm("color")); strings.HasPrefix(col, "#") || col == "" {
			g.Color = col
		}
		if cn := strings.TrimSpace(c.PostForm("cueNum")); len(cn) <= 24 {
			g.CueNum = cn
		}
		// Collapse only changes when the field is PRESENT: the inspector
		// has no collapse control any more, and every instant-save PUT
		// omits it — writing it unconditionally would uncollapse the group.
		if _, present := c.GetPostForm("collapse"); present {
			g.Collapse = c.PostForm("collapse") == "true" || c.PostForm("collapse") == "on"
		}
		g.Slideshow = c.PostForm("slideshow") == "true" || c.PostForm("slideshow") == "on"
		g.Shuffle = c.PostForm("shuffle") == "true" || c.PostForm("shuffle") == "on"
		g.Loop = c.PostForm("loop") == "true" || c.PostForm("loop") == "on"
		// The inspector's "Inside group" picker (absent in older callers —
		// they keep the current parent). Cycle/self-parent attempts are
		// rejected by UpdateGroup's validation.
		if raw, present := c.GetPostForm("parentGroupID"); present {
			if p, perr := strconv.Atoi(strings.TrimSpace(raw)); perr == nil {
				g.ParentGroupID = p
			}
		}
		// The group inspector submits hold/fade in SECONDS (its labels read
		// "(s)") but the persisted fields are milliseconds; ×1000 here so a
		// "5" is a 5s hold, not 5ms.
		if f, err := strconv.Atoi(c.PostForm("fadeSecs")); err == nil {
			g.FadeMS = f * 1000
		}
		if d, err := strconv.Atoi(c.PostForm("durationSecs")); err == nil {
			g.DurationMS = d * 1000
		}
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Inline group-name edit from the cuesheet (dblclick on the group name).
	// Same pattern as the cue row's POST/PUT edit/:col pair.
	rg.POST("/group/:id/edit/name", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		c.HTML(http.StatusOK, "cueeditcol.html", gin.H{
			"val":    g.Name,
			"action": "/api/group/" + c.Param("id") + "/name",
		})
	})
	rg.PUT("/group/:id/name", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		g.Name = strings.TrimSpace(c.PostForm("val"))
		if g.Name == "" {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "a group name is required"})
			return
		}
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Move a group block (its member cues, in order) so it starts before the
	// cue at beforePos (0 = end of sheet). Used by group-header drag reorder.
	rg.POST("/group/:id/move", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		before, _ := strconv.Atoi(c.PostForm("beforePos"))
		beforeGroup, _ := strconv.Atoi(c.PostForm("beforeGroup"))
		if err := ctp.MoveGroup(id, before, beforeGroup); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Group colour from the context menu ("" = None, clears the colour).
	// A dedicated endpoint so the generic PUT /group/:id form-save's missing
	// checkboxes cannot wipe slideshow state.
	rg.POST("/group/:id/color", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		g.Color = strings.TrimSpace(c.PostForm("color"))
		if g.Color != "" && !strings.HasPrefix(g.Color, "#") {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "invalid colour"})
			return
		}
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	rg.POST("/group/:id/collapse", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		g.Collapse = !g.Collapse
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Select a group like a cue row: persists the selection (as the negative
	// group id) so other clients and reloads mirror it.
	rg.POST("/group/:id/select", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		if _, gerr := ctp.GetGroup(id); gerr != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		// Same modifiers as cue rows (§12.4): shift extends the visible
		// range to this header, cmd toggles its membership.
		if c.Query("extend") == "1" {
			if err := ctp.ExtendSelection(0, id); err == nil {
				renderCuesheet(c)
				return
			}
		} else if c.Query("toggle") == "1" {
			anchor, _ := ctp.SelectedCuePos()
			set := ctp.SelectedSet()
			out := set[:0]
			had := false
			for _, p := range set {
				if p == -id {
					had = true
					continue
				}
				out = append(out, p)
			}
			if !had && anchor != -id {
				out = append(out, -id)
			}
			if anchor == 0 {
				anchor = -id
			}
			if err := ctp.SetGroupSelection(anchor, out); err == nil {
				renderCuesheet(c)
				return
			}
		}
		if err := ctp.SetSelectedGroup(id); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Arrow-key group open/close for the selected group: harmless no-ops when
	// the selection is a cue or the group is already in the asked state.
	rg.POST("/group/selected/open", func(c *gin.Context) {
		openSelGroup(c, true)
	})
	rg.POST("/group/selected/close", func(c *gin.Context) {
		openSelGroup(c, false)
	})

	// Inline group cue-number edit from the cuesheet (dblclick the number).
	rg.POST("/group/:id/edit/cueNum", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		c.HTML(http.StatusOK, "cueeditcol.html", gin.H{
			"val":    g.CueNum,
			"action": "/api/group/" + c.Param("id") + "/cue_num",
		})
	})
	rg.PUT("/group/:id/cue_num", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		if g.CueNum = strings.TrimSpace(c.PostForm("val")); len(g.CueNum) > 24 {
			g.CueNum = g.CueNum[:24]
		}
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Group inspector: renders the group's settings partial (shared with
	// GET /api/cue/inspector when the persisted selection is a group).
	rg.GET("/group/:id/inspector", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		renderGroupInspector(c, id)
	})

	// POST /api/group/:id/play triggers the group's playlist action: for a
	// slideshow group, shuffled/looped/faded timed playback of its member
	// cues; otherwise it just plays the first member (cues are a list - a
	// plain group has no implicit queue).
	rg.POST("/group/:id/play", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		g, err := ctp.GetGroup(id)
		if err != nil {
			c.String(http.StatusNotFound, "group not found")
			return
		}
		playGroup(g)
		c.Status(http.StatusOK)
	})

	// Assign a cue to a group (0 = top level). Default: the cue lands at the
	// group's end; first=1 makes it the group's FIRST cue (§5.4: drop on the
	// lower half of an expanded header).
	rg.POST("/cue/:cuePos/group", func(c *gin.Context) {
		groupID, err := strconv.Atoi(c.PostForm("groupId"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid groupId")
			return
		}
		assign := ctp.SetCueGroup
		if c.PostForm("first") == "1" {
			assign = ctp.SetCueGroupFirst
		}
		if err := assign(c.Param("cuePos"), groupID); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		// Joining opens the group so the cue lands visibly (groupID 0 =
		// release to top level: nothing to open).
		if groupID != 0 {
			if err := ctp.ExpandGroup(groupID); err != nil {
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
				return
			}
		}
		renderCuesheet(c)
	})
}

// playGroup triggers a group's playlist action: a slideshow group runs its
// runner, a plain group plays its first member (cues are a list — a plain
// group has no implicit queue). Shared by the group route and the GO path.
func playGroup(g ctp.Group) {
	if g.Slideshow {
		go goSafe(func() { slideshowRunner(g) })
		return
	}
	playFirstGroupMember(g.GroupID)
}

// defaultSlideshowHold is the per-image hold when the group's duration_ms is
// unset (0). Cluster-scoped; a real production value would come per group.
const defaultSlideshowHold = 5 * time.Second

// slideshowRunner plays a group's member cues in flat cuePos order (its
// rendered order). Slideshow groups shuffle and loop; non-slideshow groups
// just play their first member. Each image holds for the group's duration,
// optional fade-out between images. The loop aborts whenever the operator
// plays/stops anything else (cue-position identity guard, like auto-continue).
// ponytail: single goroutine; per-group generation counters would be needed to
// disambiguate "same slideshow replayed" from "still that original run".
// groupOption is one entry in the group inspector's "Inside group" picker:
// every OTHER group, depth-indented, with the picker group's own descendants
// disabled (choosing one would create a cycle).
type groupOption struct {
	GroupID        int
	Name           string
	Indent         string
	IsDescendantOf bool
}

// renderGroupInspector renders the groupinspector partial for one group.
// cue.H → groupinspector.html expects .Group and .MemberCount plus the cue
// palette for the colour dropdown.
func renderGroupInspector(c *gin.Context, groupID int) {
	g, err := ctp.GetGroup(groupID)
	if err != nil {
		c.String(http.StatusNotFound, "group not found")
		return
	}
	members, err := ctp.GroupCuePositions(groupID)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}
	all, err := ctp.Groups()
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}
	// Depth-indent by walking each group's ancestors; the picker group's own
	// subtree is disabled in the dropdown (cycle prevention at the UI level;
	// ValidateGroupParent enforces it server-side too).
	byID := make(map[int]ctp.Group, len(all))
	for _, og := range all {
		byID[og.GroupID] = og
	}
	descendants := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		for _, og := range all {
			if og.ParentGroupID == id && !descendants[og.GroupID] {
				descendants[og.GroupID] = true
				walk(og.GroupID)
			}
		}
	}
	walk(groupID)
	opts := make([]groupOption, 0, len(all))
	for _, og := range all {
		if og.GroupID == groupID {
			continue
		}
		depth := 0
		for at := og.ParentGroupID; at != 0 && depth <= len(all); {
			p, ok := byID[at]
			if !ok || p.ParentGroupID == at {
				break
			}
			at = p.ParentGroupID
			depth++
		}
		indent := ""
		for i := 0; i < depth; i++ {
			indent += "· "
		}
		opts = append(opts, groupOption{
			GroupID:        og.GroupID,
			Name:           og.Name,
			Indent:         indent,
			IsDescendantOf: descendants[og.GroupID],
		})
	}
	c.HTML(http.StatusOK, "groupinspector.html", gin.H{
		"Group":       g,
		"MemberCount": len(members),
		"Palette":     cuePalette,
		"OtherGroups": opts,
	})
}

// openSelGroup opens (true) or closes (false) the currently selected group;
// no-ops on cues (the state simply stays, and a fresh cuesheet renders so the
// swap never receives an empty body).
func openSelGroup(c *gin.Context, open bool) {
	gid, gerr := ctp.SelectedGroupPos()
	if gerr != nil || gid == 0 {
		renderCuesheet(c)
		return
	}
	g, err := ctp.GetGroup(gid)
	if err != nil {
		renderCuesheet(c)
		return
	}
	// Apply the requested state only when it differs from the current one; a
	// fresh cuesheet always renders so the swap has content.
	if g.Collapse == open {
		g.Collapse = !open
		if err := ctp.UpdateGroup(g); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
	}
	renderCuesheet(c)
}

func slideshowRunner(g ctp.Group) {
	// Only image groups are slideshows, but the runner itself is kind-agnostic.
	if !g.Slideshow {
		playFirstGroupMember(g.GroupID)
		return
	}

	// Snapshot the subtree in rendered (sheet) order: the group's own members
	// plus any nested subgroups'. Image (and video) cues become the slides;
	// AUDIO cues become the soundtrack playlist played underneath.
	subtree, err := ctp.GroupSubtreeCues(g.GroupID)
	if err != nil || len(subtree) == 0 {
		return
	}
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		return
	}
	byPos := make(map[int]ctp.Cue, len(sheet.Cues))
	for _, cue := range sheet.Cues {
		byPos[cue.CuePos] = cue
	}
	var members, music []ctp.Cue
	for _, pos := range subtree {
		cue, ok := byPos[pos]
		if !ok {
			continue
		}
		if strings.HasPrefix(cue.Mimetype, "audio/") {
			music = append(music, cue)
		} else {
			members = append(members, cue)
		}
	}
	if len(members) == 0 {
		return
	}
	if g.Shuffle {
		rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
		rand.Shuffle(len(music), func(i, j int) { music[i], music[j] = music[j], music[i] })
	}

	// The soundtrack runs on its own pipeline for the whole slideshow; it is
	// stopped here, and by any operator intervention (Stop/Panic/new load
	// hook into the same stop).
	var stopMusic func()
	if len(music) > 0 {
		files := make([]string, 0, len(music))
		for _, m := range music {
			files = append(files, m.Filename)
		}
		stopMusic = gsp.BackgroundPlaylist(files, music[0].Volume)
	}
	defer func() {
		if stopMusic != nil {
			stopMusic()
		}
	}()

	hold := time.Duration(g.DurationMS) * time.Millisecond
	if hold <= 0 {
		hold = defaultSlideshowHold
	}
	fade := time.Duration(g.FadeMS) * time.Millisecond

	for {
		for _, cue := range members {
			if err := loadAndPlayCueKeep(cue, strings.HasPrefix(cue.Mimetype, "image/")); err != nil {
				log.Printf("slideshow: loading %q failed: %v", cue.Filename, err)
				return
			}
			_ = ctp.SetCue(strconv.Itoa(cue.CuePos)) // highlight = current image
			// Images show for the group hold; video slides play their own
			// duration (rate/trim-corrected): a 30s clip is not a 5s slide.
			ms := hold.Milliseconds()
			if !strings.HasPrefix(cue.Mimetype, "image/") {
				if eff := ctp.EffectiveCueDuration(cue); eff > 0 {
					ms = int64(eff)
				}
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)

			// Operator intervention aborts the slideshow (same identity guard
			// shape as auto-continue).
			if gsp.CurrentCuePos() != cue.CuePos {
				return
			}
			if fade > 0 {
				gsp.FadeAndStop(int(fade / time.Millisecond))
				time.Sleep(fade)
				// No identity check here: FadeAndStop ends the cue itself,
				// so the end hook clears cuePos regardless — checking
				// CurrentCuePos here aborted every slideshow after slide 1.
			}
		}
		if !g.Loop {
			return
		}
	}
}

func playFirstGroupMember(groupID int) {
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		return
	}
	// sheet.Cues is already visual order. Include nested subgroups: the
	// first cue a GO should play is the visually-first descendant member.
	inScope := map[int]bool{groupID: true}
	for _, g := range sheet.Groups {
		for a := g.ParentGroupID; a != 0; {
			if a == groupID {
				inScope[g.GroupID] = true
				break
			}
			parent := 0
			for _, h := range sheet.Groups {
				if h.GroupID == a {
					parent = h.ParentGroupID
					break
				}
			}
			a = parent
		}
	}
	for _, cue := range sheet.Cues {
		if inScope[cue.Parent] {
			_ = loadAndPlayCue(cue)
			_ = ctp.SetCue(strconv.Itoa(cue.CuePos))
			return
		}
	}
}
