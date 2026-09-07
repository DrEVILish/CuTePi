package routes

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// MaxBodyBytes caps every request body (uploads, .CTP imports, form posts).
// Without it a single POST can fill the media disk — which also takes down
// SQLite writes — or OOM the in-memory .CTP parse. 2 GiB is far above any
// legit clip for a Pi appliance. Test-only override below.
var maxBodyBytes int64 = 2 << 30

// LimitBody wraps each request body in http.MaxBytesReader. When the cap is
// exceeded the body read fails and the existing handler error paths render
// the rejection (multipart/JSON parse errors surface as 400/422).
func LimitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		c.Next()
	}
}

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
