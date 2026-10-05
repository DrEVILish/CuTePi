package routes

import (
	"net/http"
	"net/url"
	"strings"

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
		if !sameOriginRequest(c.Request) {
			c.String(http.StatusForbidden, "cross-origin request refused")
			c.Abort()
			return
		}
		c.Next()
	}
}

// sameOriginRequest reports whether r's Origin/Referer (when present) names
// r's own Host. Also used by the WebSocket upgrader's CheckOrigin.
func sameOriginRequest(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
		if src == "" {
			return true // not a browser-initiated cross-site request
		}
	}
	if src == "null" {
		return false // sandboxed iframe / opaque origin
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
