package routes

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// AuthMiddleware enforces the optional operator password (config.json
// auth_password, editable in Settings). Empty = disabled, the default for a
// trusted show LAN. HTTP Basic keeps every client working with zero protocol
// changes — htmx/fetch requests and the WebSocket handshake all carry the
// browser's cached credentials on same-origin requests.
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		pw := config.AuthPassword()
		if pw == "" {
			c.Next()
			return
		}
		_, supplied, ok := c.Request.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(supplied), []byte(pw)) != 1 {
			c.Header("WWW-Authenticate", `Basic realm="CuTePi", charset="UTF-8"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}
