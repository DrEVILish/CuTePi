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

// Variants and tint come through from the manifest (ftl-themes contract:
// palette variants; theme tint), and the picker markup has their fields.
func TestThemeVariantsAndTint(t *testing.T) {
	themes := Themes()
	if len(themes) == 0 {
		t.Skip("ftl-themes submodule not checked out")
	}
	var aero, plain *Theme
	for i := range themes {
		switch themes[i].Name {
		case "win7-aero":
			aero = &themes[i]
		case "xbmc":
			plain = &themes[i]
		}
	}
	if aero == nil || plain == nil {
		t.Fatal("win7-aero or xbmc missing from the manifest")
	}
	if aero.Tint == nil || aero.Tint.Token != "--aero-tint" || aero.Tint.Label == "" {
		t.Errorf("win7-aero tint = %+v, want --aero-tint with a label", aero.Tint)
	}
	if len(aero.Variants) == 0 {
		t.Error("win7-aero has no variants")
	}
	if plain.Tint != nil || len(plain.Variants) != 0 {
		t.Errorf("xbmc should have no tint or variants: %+v", plain)
	}
	if !strings.Contains(string(ThemeMap()), `"--aero-tint"`) {
		t.Error("theme map lacks the tint token the boot script applies")
	}
	for _, bad := range []*ThemeTint{
		{Token: "--x;background:url(x)", Default: "#000000"},
		{Token: "--x", Default: "red"},
	} {
		if validTint(bad) != nil {
			t.Errorf("unsafe tint %+v accepted", bad)
		}
	}
}
