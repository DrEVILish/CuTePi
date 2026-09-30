package routes

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every theme comes from the ftl-themes submodule: no app-owned themes, each
// bundle linked directly (it layers itself), and the picker and JSON endpoint
// list exactly the manifest.
func TestThemesComeOnlyFromFtlThemes(t *testing.T) {
	themes := Themes()
	if len(themes) == 0 {
		t.Skip("ftl-themes submodule not checked out")
	}
	for _, th := range themes {
		if !strings.HasPrefix(th.ID, "ftl:") || th.ID != "ftl:"+th.Name {
			t.Fatalf("theme %+v: id must be ftl:<slug>", th)
		}
		if th.Href != "/ftl/themes/"+th.Name+".css" {
			t.Fatalf("theme %+v: bundle must be linked directly", th)
		}
		if th.Label == "" || strings.Contains(th.Label, "(shared)") {
			t.Fatalf("theme %+v: plain label expected", th)
		}
	}
	if DefaultThemeID != "ftl:xbmc" {
		t.Fatalf("DefaultThemeID = %q", DefaultThemeID)
	}

	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{`id="cutepi-theme-css"`, `/ftl/themes/xbmc.css`, `<option value="ftl:xbmc">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}
	for _, gone := range []string{"/css/themes/", `value="app:`} {
		if strings.Contains(body, gone) {
			t.Fatalf("index still references local themes (%q)", gone)
		}
	}
	if strings.Contains(body, `id="cutepi-theme-css" rel="stylesheet" href=""`) {
		t.Fatal(`the theme <link> must ship with no href, not href=""`)
	}
	resp := get(t, r, "/api/themes")
	var list []Theme
	if resp.Code != 200 || json.Unmarshal(resp.Body.Bytes(), &list) != nil || len(list) != len(themes) {
		t.Fatalf("GET /api/themes = %d, %d themes (want %d)", resp.Code, len(list), len(themes))
	}
}
