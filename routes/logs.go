package routes

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/logs"
)

// Logs implements the Web UI log viewer: a JSON feed of the recent buffered
// records, a runtime level switch, and a clear. The viewer only reads; it is
// not a terminal - the audit trail is exported via the .CTP export instead.
func Logs(rg *gin.RouterGroup) {
	rg.GET("/logs", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"level":      logs.CurrentLevel().String(),
			"recordedAt": time.Now().Format(time.RFC3339),
			"entries":    logs.Recorded(),
		})
	})

	rg.POST("/logs/level", func(c *gin.Context) {
		lvl, ok := logs.ParseLevel(c.PostForm("level"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid level"})
			return
		}
		logs.SetLevel(lvl)
		c.Status(http.StatusOK)
	})

	rg.DELETE("/logs", func(c *gin.Context) {
		logs.Clear()
		c.Status(http.StatusOK)
	})
}