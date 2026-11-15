package routes

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/gsp"
)

// registerGLWallRoutes serves the GPU wall's counters (DESIGN §6.1.1):
// frames mixed and frames presented since start, for measuring a cue
// through the wall from outside the process. Read-only.
func registerGLWallRoutes(rg *gin.RouterGroup) {
	rg.GET("/debug/glwall", func(c *gin.Context) {
		mixed, presented, layers, on := gsp.GLWallStats()
		delay, audio := gsp.GLAudioSync()
		c.JSON(http.StatusOK, gin.H{"on": on, "mixed": mixed, "presented": presented, "layers": layers, "pool": gsp.GLPoolStats(),
			"displayDelayMs": float64(delay.Microseconds()) / 1000, "cueAudioOffsetMs": float64(audio.Microseconds()) / 1000})
	})
	// Plane wall presenter (CUTEPI_WALL_SINK=planesink): commits, the
	// intervals between them, and frames on screen per visible layer.
	rg.GET("/debug/planewall", func(c *gin.Context) {
		if c.Query("gc") != "" { // TEMP leak debug
			runtime.GC()
			time.Sleep(200 * time.Millisecond)
			runtime.GC()
			time.Sleep(200 * time.Millisecond)
		}
		if c.Query("retired") != "" {
			buf := make([]byte, 4<<20)
			c.String(http.StatusOK, gsp.RetiredReport()+"\n"+string(buf[:runtime.Stack(buf, true)]))
			return
		}
		st, layers, on := gsp.PlaneWallStats()
		c.JSON(http.StatusOK, gin.H{"on": on, "commits": st.Commits, "fails": st.Fails,
			"intervalMs": gin.H{"p50": st.P50Ms, "p99": st.P99Ms, "max": st.MaxMs}, "layers": layers,
			"pipelinesAlive": gsp.PipelinesAlive()})
	})
}
