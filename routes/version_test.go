package routes

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The app version is reported by /api/settings and shown in Settings, and
// VERSION holds a semantic version.
func TestVersionShown(t *testing.T) {
	r := setupTestServer(t)
	defer func(v string) { Version = v }(Version)
	Version = "9.8.7"
	var s map[string]any
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &s); err != nil || s["version"] != "9.8.7" {
		t.Fatalf("/api/settings version = %v (%v)", s["version"], err)
	}
	if body := get(t, r, "/").Body.String(); !strings.Contains(body, `id="settingsVersion">v9.8.7<`) {
		t.Fatal("Settings does not show the version")
	}
	raw, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if v := strings.TrimSpace(string(raw)); !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Fatalf("VERSION = %q, want MAJOR.MINOR.PATCH", v)
	}
}
