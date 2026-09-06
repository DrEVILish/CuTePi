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

// SheetRow is one cluster of the rendered cuesheet: either a group header
// plus its contiguous member cues, or a single ungrouped cue. Built in Go so
// the template stays a simple nested range (closing a group mid-range is not
// expressible in html/template).
type SheetRow struct {
	Group *ctp.Group
	Cues  []ctp.Cue // single-cue clusters for ungrouped cues
}

// buildSheetRows folds the flat cuesheet (cuePos order) into contiguous
// group clusters. Membership is a presentation layer: a cue's parent group is
// rendered as a header whenever the parent changes; ungrouped cues are their
// own single-cue rows.
func buildSheetRows(cuesheet *ctp.Cuesheet) []SheetRow {
	groupByID := make(map[int]*ctp.Group, len(cuesheet.Groups))
	for i := range cuesheet.Groups {
		g := cuesheet.Groups[i]
		groupByID[g.GroupID] = &g
	}

	var rows []SheetRow
	var current *ctp.Group
	var cluster []ctp.Cue
	seen := make(map[int]bool, len(cuesheet.Groups))
	flush := func() {
		if len(cluster) > 0 {
			if current != nil {
				seen[current.GroupID] = true
			}
			rows = append(rows, SheetRow{Group: current, Cues: cluster})
			cluster = nil
		}
	}
	for _, cue := range cuesheet.Cues {
		var parent *ctp.Group
		if cue.Parent != 0 {
			parent = groupByID[cue.Parent]
		}
		if parent != current {
			flush()
			// Header for the newly-entered group (or nil for ungrouped).
			current = parent
		}
		cluster = append(cluster, cue)
	}
	flush()
	// Empty groups have no cue positions to ride in the flat order, so they
	// trail the sheet (still discoverable and editable).
	for _, g := range cuesheet.Groups {
		if !seen[g.GroupID] {
			rows = append(rows, SheetRow{Group: &g, Cues: nil})
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
		if _, err := ctp.CreateGroup(name, parent); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	rg.DELETE("/group/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid group id")
			return
		}
		if err := ctp.DeleteGroup(id); err != nil {
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
		g.Collapse = c.PostForm("collapse") == "true" || c.PostForm("collapse") == "on"
		g.Slideshow = c.PostForm("slideshow") == "true" || c.PostForm("slideshow") == "on"
		g.Shuffle = c.PostForm("shuffle") == "true" || c.PostForm("shuffle") == "on"
		g.Loop = c.PostForm("loop") == "true" || c.PostForm("loop") == "on"
		if f, err := strconv.Atoi(c.PostForm("fadeMs")); err == nil {
			g.FadeMS = f
		}
		if d, err := strconv.Atoi(c.PostForm("durationMs")); err == nil {
			g.DurationMS = d
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

	rg.GET("/group/:id/inspector", func(c *gin.Context) {
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
		members, err := ctp.GroupCuePositions(id)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.HTML(http.StatusOK, "groupinspector.html", gin.H{"Group": g, "MemberCount": len(members)})
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
		go slideshowRunner(g)
		c.Status(http.StatusOK)
	})

	// Assign a cue to a group (0 = top level) and move it to the group's end.
	rg.POST("/cue/:cuePos/group", func(c *gin.Context) {
		groupID, err := strconv.Atoi(c.PostForm("groupId"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid groupId")
			return
		}
		if err := ctp.SetCueGroup(c.Param("cuePos"), groupID); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})
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
func slideshowRunner(g ctp.Group) {
	// Only image groups are slideshows, but the runner itself is kind-agnostic.
	if !g.Slideshow {
		playFirstGroupMember(g.GroupID)
		return
	}

	// Snapshot the members in their rendered (sheet) order.
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		return
	}
	var members []ctp.Cue
	for _, cue := range sheet.Cues {
		if cue.Parent == g.GroupID {
			members = append(members, cue)
		}
	}
	if len(members) == 0 {
		return
	}
	if g.Shuffle {
		rand.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	}

	hold := time.Duration(g.DurationMS) * time.Millisecond
	if hold <= 0 {
		hold = defaultSlideshowHold
	}
	fade := time.Duration(g.FadeMS) * time.Millisecond

	for {
		for _, cue := range members {
			if err := loadAndPlayCue(cue); err != nil {
				log.Printf("slideshow: loading %q failed: %v", cue.Filename, err)
				return
			}
			_ = ctp.SetCue(strconv.Itoa(cue.CuePos)) // highlight = current image
			time.Sleep(hold)

			// Operator intervention aborts the slideshow (same identity guard
			// shape as auto-continue).
			if gsp.CurrentCuePos() != cue.CuePos {
				return
			}
			if fade > 0 {
				gsp.FadeAndStop(int(fade / time.Millisecond))
				time.Sleep(fade)
				if gsp.CurrentCuePos() != cue.CuePos {
					return
				}
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
	for _, cue := range sheet.Cues {
		if cue.Parent == groupID {
			_ = loadAndPlayCue(cue)
			_ = ctp.SetCue(strconv.Itoa(cue.CuePos))
			return
		}
	}
}