package routes

import (
	"encoding/json"
	"strings"
	"testing"
)

// A dropped-in name.css must surface everywhere with no code changes:
// discovery, the settings select (index body) and the JSON endpoint.
func TestThemeDiscoveryAndEndpoint(t *testing.T) {
	byID := map[string]Theme{}
	for _, th := range Themes() {
		byID[th.ID] = th
	}
	for name, label := range map[string]string{
		"blue-future": "Future SciFi (Default)",
	} {
		th, ok := byID["app:"+name]
		if !ok {
			t.Fatalf("app theme %q not discovered, got %+v", name, byID)
		}
		if th.Label != label || th.File != name+".css" {
			t.Fatalf("app theme %q = %+v, want label %q", name, th, label)
		}
		if th.Href != "/css/themes/"+name+".css" || th.Source != "app" {
			t.Fatalf("app theme %q = %+v, want app href", name, th)
		}
	}

	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		// Exactly one stylesheet is linked, and the id -> href map the
		// pre-paint script picks from carries every discovered theme.
		`id="cutepi-theme-css"`,
		`/css/themes/blue-future.css`,
		`<option value="app:blue-future">Future SciFi (Default)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	// href="" would resolve to the page itself and be fetched as CSS.
	if strings.Contains(body, `id="cutepi-theme-css" rel="stylesheet" href=""`) {
		t.Fatal(`the theme <link> must ship with no href, not href=""`)
	}

	resp := get(t, r, "/api/themes")
	if resp.Code != 200 {
		t.Fatalf("GET /api/themes = %d, want 200", resp.Code)
	}
	var list []Theme
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding /api/themes: %v", err)
	}
	apps := 0
	for _, th := range list {
		if th.Source == "app" {
			apps++
		}
		if th.ID == "" || th.Name == "" || th.Href == "" {
			t.Fatalf("theme %+v is missing an id/name/href", th)
		}
	}
	if apps != 1 {
		t.Fatalf("/api/themes returned %d app themes, want 1 (blue-future)", apps)
	}
}

// The shared bundles are an addition, not a replacement: this app's own
// themes keep their names, and the default stays this app's own file so its
// appearance does not change just because themes became shared.
func TestSharedThemesAreOfferedAlongsideAppThemes(t *testing.T) {
	var shared []Theme
	for _, th := range Themes() {
		if th.Source == "ftl" {
			shared = append(shared, th)
		}
	}
	if len(shared) == 0 {
		t.Skip("ftl-themes submodule not checked out")
	}
	byID := map[string]Theme{}
	for _, th := range shared {
		byID[th.ID] = th
		if !strings.HasPrefix(th.Href, "/ftl/themes/") {
			t.Fatalf("shared theme %+v should be served from /ftl/themes/", th)
		}
		if !strings.HasSuffix(th.Label, "(shared)") {
			t.Fatalf("shared theme %+v should be labelled as shared", th)
		}
	}
	// Only blue-future stays app-owned (the app's default); the rest of the
	// family now comes from the shared bundles.
	appCount := 0
	for _, th := range Themes() {
		if th.Source == "app" {
			appCount++
		}
	}
	if appCount != 1 {
		t.Fatalf("got %d app themes, want exactly blue-future", appCount)
	}
	if DefaultThemeID != "ftl:xbmc" {
		t.Fatalf("DefaultThemeID = %q, want ftl:xbmc (the theme source is the submodule)", DefaultThemeID)
	}
}
