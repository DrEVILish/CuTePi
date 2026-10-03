package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/gsp"
)

// registerGLWallRoutes serves the GPU wall's counters (DESIGN §6.1.1):
// frames mixed and frames presented since start, for measuring a cue
// through the wall from outside the process. Read-only.
func registerGLWallRoutes(rg *gin.RouterGroup) {
	rg.GET("/debug/glwall", func(c *gin.Context) {
		mixed, presented, layers, on := gsp.GLWallStats()
		c.JSON(http.StatusOK, gin.H{"on": on, "mixed": mixed, "presented": presented, "layers": layers, "pool": gsp.GLPoolStats()})
	})
}
