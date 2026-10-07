package routes

// Active Cues pane (DESIGN §6.1.2): every running cue in stack order, top
// first, with a Stop (cut) and a Fade out (the ESC fade time) for that cue
// alone. The pane re-renders on each WebSocket sync while it is open.

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// activeCue is one row of the pane.
type activeCue struct {
	gsp.Voice
	Num       string // cue number ("" for a direct play)
	Name      string // cue title, else the file
	Color     string
	Elapsed   string
	Remaining string
	Pct       int
}

func activeCuesData() gin.H {
	voices := gsp.Voices()
	rows := make([]activeCue, 0, len(voices))
	for _, v := range voices {
		r := activeCue{Voice: v, Name: v.Title, Elapsed: formatClock(v.Position)}
		if v.CuePos > 0 {
			if cue, err := ctp.GetCue(strconv.Itoa(v.CuePos)); err == nil {
				r.Num, r.Color = cue.CueNum, cue.Color
				if cue.Title != "" {
					r.Name = cue.Title
				}
			}
		}
		if v.Duration > 0 {
			r.Remaining = formatClock(v.Duration - v.Position)
			r.Pct = ProgressPct(int(v.Position*1000), int(v.Duration*1000))
		}
		rows = append(rows, r)
	}
	return gin.H{"Cues": rows, "EscFadeMs": ctp.GetEscFadeMs()}
}

// runningVoice reports the running cue at cuePos, on any layer.
func runningVoice(cuePos int) (gsp.Voice, bool) {
	if cuePos <= 0 || !slices.Contains(gsp.RunningCues(), cuePos) {
		return gsp.Voice{}, false
	}
	for _, v := range gsp.Voices() {
		if v.CuePos == cuePos {
			return v, true
		}
	}
	return gsp.Voice{}, false
}

func registerActiveCuesRoutes(rg *gin.RouterGroup) {
	rg.GET("/activecues", func(c *gin.Context) {
		c.HTML(http.StatusOK, "activecues.html", activeCuesData())
	})
	stop := func(c *gin.Context, fadeMs int) {
		pos, err := strconv.Atoi(c.Param("cuePos"))
		if err != nil || pos <= 0 {
			respondError(c, http.StatusBadRequest, "invalid cue position")
			return
		}
		if fadeMs > 0 && !gsp.Layered() && gsp.CurrentCuePos() == pos {
			// One picture (no display layers): the cue fades out the way
			// ESC fades it, then stops.
			loads := gsp.Loads()
			goSafe(func() {
				gsp.FadeAndStop(fadeMs)
				if gsp.Loads() == loads {
					gsp.StopCue(pos, 0)
				}
			})
			c.HTML(http.StatusOK, "activecues.html", activeCuesData())
			return
		}
		if !gsp.StopCue(pos, fadeMs) {
			respondError(c, http.StatusNotFound, "that cue is not running")
			return
		}
		c.HTML(http.StatusOK, "activecues.html", activeCuesData())
	}
	rg.POST("/activecues/:cuePos/stop", func(c *gin.Context) {
		logs.Printf(logs.RTEStop, "Active Cues: stop cue %s", c.Param("cuePos"))
		stop(c, 0)
	})
	rg.POST("/activecues/:cuePos/fade", func(c *gin.Context) {
		logs.Printf(logs.RTEFadeOut, "Active Cues: fade out cue %s", c.Param("cuePos"))
		stop(c, ctp.GetEscFadeMs())
	})
}
