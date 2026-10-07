package routes

import (
	"net/http"

	"CuTePi/ws"
	"github.com/gin-gonic/gin"
)

// SameOrigin is the cross-site guard for the whole HTTP surface.
//
//   - CSRF: a state-changing request (anything but GET/HEAD/OPTIONS) whose
//     Origin (or, lacking one, Referer) names a different host is refused.
//     Browsers attach cached Basic-auth credentials to cross-site form posts,
//     so the operator password alone does not stop a hostile page from
//     POSTing /api/shutdown. Requests with neither header (curl, Companion,
//     scripts) are not browser-driven and pass.
//
// The Host header is not restricted: the server answers under any name, so
// any reverse proxy in front of it works without configuration.
func SameOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if !ws.SameOrigin(c.Request) {
			c.String(http.StatusForbidden, "cross-origin request refused")
			c.Abort()
			return
		}
		c.Next()
	}
}
