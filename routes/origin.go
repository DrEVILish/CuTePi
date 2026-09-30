package routes

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/logs"
)

// SameOrigin is the cross-site guard for the whole HTTP surface.
//
//   - CSRF: a state-changing request (anything but GET/HEAD/OPTIONS) whose
//     Origin (or, lacking one, Referer) names a different host is refused.
//     Browsers attach cached Basic-auth credentials to cross-site form posts,
//     so the operator password alone does not stop a hostile page from
//     POSTing /api/shutdown. Requests with neither header (curl, Companion,
//     scripts) are not browser-driven and pass.
//   - DNS rebinding: the Host header must be one this box answers to (an IP
//     literal, localhost, a dotless name, a name under a local-only suffix
//     such as .local, the machine hostname, or a name in config
//     allowed_hosts). A rebinding page reaches us under its own public
//     domain name and is refused, so it cannot read API responses.
func SameOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !hostAllowed(c.Request.Host) {
			logs.Printf(logs.RTEHostRefused, "refused Host %q: not in config allowed_hosts", c.Request.Host)
			c.String(http.StatusMisdirectedRequest, "Host %q is not allowed. Add it to allowed_hosts in config.json (reverse-proxy domains must be listed) and restart the service.", c.Request.Host)
			c.Abort()
			return
		}
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

// localSuffixes are reserved or non-delegated name suffixes that never
// resolve through public DNS.
var localSuffixes = []string{".local", ".lan", ".home.arpa", ".internal", ".localhost"}

// hostAllowed is the DNS-rebinding allow-list for the Host header.
func hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return true // HTTP/1.0 without Host: not a browser
	}
	if net.ParseIP(host) != nil || host == "localhost" {
		return true
	}
	// Dotless names and local-only suffixes resolve only on the LAN (mDNS,
	// the router's DNS): an attacker's public DNS cannot serve them, so
	// they cannot be rebinding vectors. This keeps mDNS conflict renames
	// (cutepi-2.local) and router names (cutepi.lan) working.
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		if host == name || host == name+".local" {
			return true
		}
	}
	for _, h := range config.AllowedHosts() {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}
