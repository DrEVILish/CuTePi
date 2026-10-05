package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/logs"
)

// registerLiveRoutes serves live-page cues (DESIGN §12.14): a TimerPi
// display, or any http(s) page, rendered on the wall. Live pages are not
// pool items; the cuesheet's clock button adds one straight to the sheet,
// and the cue inspector edits its URL.
func registerLiveRoutes(rg *gin.RouterGroup) {
	rg.POST("/cue/live", func(c *gin.Context) {
		pos, err := ctp.AddLiveCue(c.PostForm("title"), c.PostForm("url"))
		if err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		logs.Printf(logs.RTEAddCue, "add live cue position=%d", pos)
		awardsSelectionSync()
		renderCuesheet(c)
	})
}
