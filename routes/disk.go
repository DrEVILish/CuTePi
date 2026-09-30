package routes

import (
	"syscall"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// mediaFreeBytes reports the space available to CuTePi on the media volume
// (0, false when it cannot be read).
func mediaFreeBytes() (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(config.MediaLocation(), &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}

// registerDiskRoute exposes the media volume's free space so every upload
// path (modal, pool drop, /upload, show import) can warn before sending more
// than fits (§5.7). known=false means "can't tell" and the client proceeds.
func registerDiskRoute(rg *gin.RouterGroup) {
	rg.GET("/disk", func(c *gin.Context) {
		free, ok := mediaFreeBytes()
		c.JSON(200, gin.H{"freeBytes": free, "known": ok})
	})
}
