package routes

import (
	"encoding/json"
	"strings"
	"testing"
)

// A dropped-in name.css must surface everywhere with no code changes:
// discovery, the settings select (index body) and the JSON endpoint.
func TestThemeDiscoveryAndEndpoint(t *testing.T) {
	byName := map[string]Theme{}
	for _, th := range Themes() {
		byName[th.Name] = th
	}
	for name, label := range map[string]string{
		"blue-future": "Future SciFi (Default)",
		"lcars":       "LCARS",
		"qlab":        "QLab",
	} {
		th, ok := byName[name]
		if !ok {
			t.Fatalf("theme %q not discovered, got %+v", name, byName)
		}
		if th.Label != label || th.File != name+".css" {
			t.Fatalf("theme %q = %+v, want label %q", name, th, label)
		}
	}

	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`/css/themes/lcars.css`,
		`/css/themes/qlab.css`,
		`/css/themes/blue-future.css`,
		`<option value="lcars">LCARS</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}

	resp := get(t, r, "/api/themes")
	if resp.Code != 200 {
		t.Fatalf("GET /api/themes = %d, want 200", resp.Code)
	}
	var list []Theme
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding /api/themes: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("/api/themes returned %d themes, want 3", len(list))
	}
}
