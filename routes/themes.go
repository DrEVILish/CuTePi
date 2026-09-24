package routes

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
)

// Theme is one selectable UI theme. Two kinds exist:
//
//   - "app" themes: a single portable CSS file in public/css/themes/<name>.css.
//     Dropping a new name.css into that folder is enough — it is served as a
//     static file, listed by Themes(), and offered in the settings UI with no
//     code changes.
//   - "ftl" themes: a bundle from the pinned ftl-themes submodule, shared with
//     the other apps in the family. Those carry a layout as well as a palette,
//     so selecting one also re-arranges the page shell.
//
// Both kinds set the same html[data-theme=<Name>] attribute, so an app theme
// and an ftl theme can share a Name (both have "lcars"). They are told apart
// by ID ("app:lcars" vs "ftl:lcars"), and exactly one stylesheet is linked at
// a time — which is what the ftl-themes contract requires anyway.
type Theme struct {
	// ID is the stable, unambiguous identifier stored in localStorage and
	// used by the settings picker: "app:<name>" or "ftl:<slug>".
	ID string `json:"id"`
	// Name is the html[data-theme] value the stylesheet is keyed on.
	Name string `json:"name"`
	// Label is the human-readable name shown in Settings.
	Label string `json:"label"`
	// File is the basename for app themes; empty for ftl themes.
	File string `json:"file"`
	// Href is the stylesheet URL to link for this theme.
	Href string `json:"href"`
	// Source is "app" or "ftl".
	Source string `json:"source"`
	// Scheme is "light" or "dark": how Bootstrap's data-bs-theme should be
	// set while this theme is active (shared themes only; app themes are
	// always dark).
	Scheme string `json:"scheme"`
}

// themeNameRe reads the display label from an app theme file's header comment:
// "Theme-Name: Human Readable". Files without it fall back to the filename.
var themeNameRe = regexp.MustCompile(`(?m)^\s*\*?\s*Theme-Name:\s*(.+?)\s*$`)

// DefaultThemeID is what an unset or unrecognised preference resolves to.
// The theme source is the ftl-themes submodule (app: files still exist as
// options), and xbmc — near-black home-theatre shell, glowing blue underline
// selection — is the closest living kin of the retired app:blue-future look.
const DefaultThemeID = "ftl:xbmc"

// themesDir locates the app themes folder whether the process runs from the
// repo root (server, smoke tests) or from routes/ (go test).
func themesDir() string { return findRepoDir("public/css/themes") }

// ftlDistDir locates the pinned ftl-themes bundles the same way.
func ftlDistDir() string { return findRepoDir("third_party/ftl-themes/dist") }

func findRepoDir(rel string) string {
	for _, prefix := range []string{"./", "../"} {
		d := prefix + rel
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "./" + rel
}

// Themes scans both sources on every call — a handful of small files, so the
// settings UI and the header link are always current without a restart.
func Themes() []Theme {
	return append(appThemes(), ftlThemes()...)
}

func appThemes() []Theme {
	entries, err := os.ReadDir(themesDir())
	if err != nil {
		return nil
	}
	var out []Theme
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".css") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".css")
		label := name
		if data, err := os.ReadFile(filepath.Join(themesDir(), e.Name())); err == nil {
			if m := themeNameRe.FindSubmatch(data); m != nil {
				label = string(m[1])
			}
		}
		out = append(out, Theme{
			ID:     "app:" + name,
			Name:   name,
			Label:  label,
			File:   e.Name(),
			Href:   "/css/themes/" + e.Name(),
			Source: "app",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ftlThemes reads the submodule's generated manifest. Its absence (submodule
// not initialised) simply means no shared themes are offered — the app themes
// above still work, so a missing submodule degrades rather than breaks.
func ftlThemes() []Theme {
	data, err := os.ReadFile(filepath.Join(ftlDistDir(), "themes.json"))
	if err != nil {
		return nil
	}
	var manifest []struct {
		Slug   string `json:"slug"`
		Label  string `json:"label"`
		Scheme string `json:"scheme"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	out := make([]Theme, 0, len(manifest))
	for _, m := range manifest {
		if m.Slug == "" {
			continue
		}
		// Scheme comes stamped from the manifest (build_manifest.py derives
		// it from the theme's surface/background tokens). Older manifests
		// without the field read as dark — the historical default.
		scheme := m.Scheme
		if scheme == "" {
			scheme = "dark"
		}
		out = append(out, Theme{
			ID:     "ftl:" + m.Slug,
			Name:   m.Slug,
			Label:  m.Label + " (shared)",
			Href:   "/ftl/themes/" + m.Slug + ".css",
			Source: "ftl",
			Scheme: scheme,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ThemeNames reports the valid data-theme values reaching the inspector
// and cuesheet constraints ("app:<name>", "ftl:<slug>", or the bare app name).
func ThemeNames() []string {
	names := []string{}
	for _, t := range Themes() {
		names = append(names, t.Name)
	}
	return names
}

// ThemeMap is the id -> {name, href} map the pre-paint boot script in
// header.html uses to pick a stylesheet before the first render, and that
// ui.js reuses when the picker changes.
func ThemeMap() template.JS {
	m := map[string]map[string]string{}
	for _, t := range Themes() {
		m[t.ID] = map[string]string{"name": t.Name, "href": t.Href, "scheme": t.Scheme}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(data)
}

// TemplateFuncs is the single template function map for the app (used by
// main.go and the route tests alike, so the two can never drift apart and
// break template parsing in only one of them).
func TemplateFuncs() map[string]any {
	return map[string]any{
		"contains":    strings.Contains,
		"hasPrefix":   strings.HasPrefix,
		"hasSuffix":   strings.HasSuffix,
		"formatTime":  ctp.FormatTime,
		"urlPath":     url.PathEscape,
		"typeIcon":    TypeIcon,
		"displayTime": DisplayTime,
		"progressPct": ProgressPct,
		"formatSchedule": func(ms int) string {
			// Cue schedule reminder: HH:MM from milliseconds since midnight.
			if ms <= 0 {
				return ""
			}
			return fmt.Sprintf("%02d:%02d", ms/3600000, (ms/60000)%60)
		},
		"div":    func(a, b int) int { return a / b },
		"hasBit": func(mask, bit int) bool { return mask&(1<<uint(bit)) != 0 },
		// safeCSS passes server-generated CSS values (e.g. a colour with a
		// var() fallback) through html/template's style sanitizer, which
		// otherwise rewrites them to ZgotmplZ.
		"safeCSS": func(s string) template.CSS { return template.CSS(s) },
		"add":     func(a, b int) int { return a + b },
		"mod":     func(a, b int) int { return a % b },
		"listDays": func() []string {
			return []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
		},
		"themeFiles":     Themes,
		"themeMap":       ThemeMap,
		"defaultThemeID": func() string { return DefaultThemeID },
		"assetStamp":     AssetStamp,
	}
}

// registerThemeRoutes serves the discovered theme list for the settings UI
// and the boot-time theme allowlist (public/src/ui.js).
func registerThemeRoutes(rg *gin.RouterGroup) {
	rg.GET("/themes", func(c *gin.Context) {
		c.JSON(http.StatusOK, Themes())
	})
}
