package routes

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// UseMiddleware installs the server's request middleware in its required
// order (main.go and the tests that check how they combine share it):
//   - SameOrigin first: a hostile page never gets a Basic-auth prompt or a
//     response to read (CSRF);
//   - AuthMiddleware: the optional operator password, on every route
//     including the WebSocket handshake;
//   - CachePolicy: no-store everywhere except images (private);
//   - LimitBody: per-route request body caps;
//   - LinkMiddleware: linked units' mirroring and forwarding (§6.8c).
func UseMiddleware(r *gin.Engine) {
	r.Use(SameOrigin())
	r.Use(AuthMiddleware())
	r.Use(CachePolicy())
	r.Use(LimitBody())
	r.Use(LinkMiddleware())
	r.NoRoute(notFound)
}

// maxBodyBytes caps the bodies of the routes that carry media (uploads,
// .CTP imports): without it a single POST can fill the media disk — which
// also takes down SQLite writes. 2 GiB is far above any legit clip for a Pi
// appliance. Every other route (forms, JSON control calls such as
// /upload/check) gets maxSmallBodyBytes: an oversized body there is only
// parsing and allocation work for nothing. Test-only overrides.
var (
	maxBodyBytes      int64 = 2 << 30
	maxSmallBodyBytes int64 = 1 << 20
)

// largeBodyPaths are the routes that accept media-sized bodies.
var largeBodyPaths = map[string]bool{
	"/upload":          true,
	"/upload/":         true,
	"/api/show/import": true,
}

// bodyLimit is the body cap for a request path.
func bodyLimit(path string) int64 {
	if largeBodyPaths[path] {
		return maxBodyBytes
	}
	return maxSmallBodyBytes
}

// LimitBody wraps each request body in http.MaxBytesReader at its route's
// cap. A declared Content-Length over the cap is refused (413) before any
// of the body is read; otherwise the body read fails at the cap and the
// existing handler error paths render the rejection (multipart/JSON parse
// errors surface as 400/422).
func LimitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := bodyLimit(c.Request.URL.Path)
		if c.Request.ContentLength > limit {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
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
		// A paired unit proves itself with the pairing secret, a failover
		// page with a token signed with it (§6.8c); the pairing handshake's
		// answer with its nonce, and the other peer routes refuse
		// anything without the secret.
		if id := peerIDFromKey(c.Request); id != "" {
			c.Set(ctxLinkPeer, id)
			c.Next()
			return
		}
		pw := config.AuthPassword()
		if pw == "" || pageTokenOK(c.Request) || c.Request.URL.Path == "/api/peer/pair/accept" ||
			(strings.HasPrefix(c.Request.URL.Path, "/api/peer/") && c.Request.URL.Path != "/api/peer/pair/request") {
			c.Next()
			return
		}
		_, supplied, ok := c.Request.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(supplied), []byte(pw)) != 1 {
			c.Header("WWW-Authenticate", `Basic realm="CuTePi", charset="UTF-8"`)
			c.Header("Cache-Control", "no-store")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}
