package routes

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// QA cross-check: dump the route table (method + pattern) to a file so the
// WebUI audit script can match every hx-post/hx-get/href the markup emits
// against a real endpoint. Runs as a normal test; the file is the product.
func TestDumpRouteTable(t *testing.T) {
	if os.Getenv("QA_ROUTE_TABLE") == "" {
		t.Skip("set QA_ROUTE_TABLE=1 to emit the route table for the UI audit")
	}
	r := gin.New()
	api := r.Group("/api")
	Api(api)
	Groups(api)
	Show(api)
	Logs(api)
	type row struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	table := []row{}
	for _, ri := range r.Routes() {
		table = append(table, row{ri.Method, ri.Path})
	}
	f, err := os.Create(os.Getenv("QA_ROUTE_TABLE"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	_ = enc.Encode(table)
}

// The audit companion: every hx-post target referenced in the templates must
// exist in the route table (path params normalised).
func TestTemplateActionsHaveRoutes(t *testing.T) {
	// Collect the gin routes.
	// The route table as main.go mounts it (Api + Groups + Show + Logs).
	r := gin.New()
	api := r.Group("/api")
	Api(api)
	Groups(api)
	Show(api)
	Logs(api)
	routes := map[string]bool{}
	for _, ri := range r.Routes() {
		routes[ri.Method+" "+ri.Path] = true
	}

	// Collect hx-*/action URLs from the templates.
	src := ""
	glob, _ := filepath.Glob("../templates/*.html")
	for _, f := range glob {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src += string(b)
	}
	re := regexp.MustCompile(`hx-(?:post|get|delete)="([^"]+)"`)
	bad := []string{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		u := m[1]
		if u == "" || containsBraces(u) {
			continue // rendered values, not literals
		}
		if actionRouted(routes, u) {
			continue
		}
		bad = append(bad, u)
	}
	for _, u := range bad {
		t.Errorf("template action %q has no matching route", u)
	}
}

// actionRouted matches a literal template action against the route table,
// including gin wildcards (`/api/test/*pattern` also serves the bare
// `/api/test` — the 307→200 trailing-slash rewrite).
func actionRouted(routes map[string]bool, u string) bool {
	if strings.Contains(u, "{{") {
		return true // rendered values, not literals
	}
	methods := []string{"POST ", "GET ", "PUT ", "DELETE "}
	for _, m := range methods {
		base := m + "/api" + stripAPI(u)
		if routes[base] {
			return true
		}
	}
	// A wildcard route also answers the bare literal before its slash.
	for r := range routes {
		parts := strings.SplitN(r, " ", 2)
		if i := strings.Index(parts[1], "/*"); i > 0 && strings.HasPrefix(parts[1], "/api") {
			if "POST /api"+stripAPI(u) == parts[0]+" "+parts[1][:i] {
				return true
			}
		}
	}
	return false
}

func containsBraces(u string) bool {
	return strings.Contains(u, "{{")
}

// stripAPI turns a full "/api/x" template action into the group-relative
// suffix the routes are registered under ("/cue/next").
func stripAPI(u string) string {
	if strings.HasPrefix(u, "/api/") {
		return u[len("/api"):]
	}
	return u
}

var _ = fmt.Sprintf
var _ = httptest.NewRequest
