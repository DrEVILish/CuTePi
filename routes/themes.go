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
	"strconv"
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
	// used by the settings picker: "app:<name>", "ftl:<slug>" or "custom".
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

// DefaultThemeID is what an unset or unrecognised preference resolves to. It
// is CuTePi's own blue-future file, not the shared ftl-themes theme of the
// same name: the two have diverged (ftl's is tuned to match a different
// device exactly), and this app's appearance must not change just because
// themes became shared.
const DefaultThemeID = "app:blue-future"

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
		Slug  string `json:"slug"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	out := make([]Theme, 0, len(manifest))
	for _, m := range manifest {
		if m.Slug == "" {
			continue
		}
		out = append(out, Theme{
			ID:     "ftl:" + m.Slug,
			Name:   m.Slug,
			Label:  m.Label + " (shared)",
			Href:   "/ftl/themes/" + m.Slug + ".css",
			Source: "ftl",
			Scheme: themeScheme(m.Slug),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// themeScheme decides whether a shared theme's panels read as light or dark,
// so the app can flip Bootstrap's data-bs-theme to match (Bootstrap's
// dark-mode text stays invisible on a light theme otherwise — empty selects,
// unreadable form help). The manifest carries no scheme field yet, so it is
// derived: the app's panels sit on --ftl-surface; if that is translucent or
// missing it is composited over the theme's shell/page background
// (--ftl-app-bg / --ftl-app-main-bg / --ftl-bg, first that exists) and the
// result's luminance decides. Themes the parser can't read stay "dark" (the
// historical default).
var (
	themeVarRe   = regexp.MustCompile(`--ftl-[a-z0-9-]+:\s*([^;]+)`)
	themeHexRe   = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)
	themeRGBARe  = regexp.MustCompile(`^rgba?\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)`)
)

func themeScheme(slug string) string {
	raw, err := os.ReadFile(filepath.Join(ftlDistDir(), slug+".css"))
	if err != nil {
		return "dark"
	}
	get := func(name string) string {
		val := ""
		for _, m := range themeVarRe.FindAllStringSubmatch(string(raw), -1) {
			if strings.HasPrefix(strings.TrimSpace(m[0]), name+":") {
				val = strings.TrimSpace(m[1]) // last declaration wins, as in CSS
			}
		}
		return val
	}
	surface := parseCSSColor(get("--ftl-surface"))
	if surface == nil {
		return "dark"
	}
	if surface[3] < 1 {
		// translucent surface: composite it over the first solid background
		// the theme paints (shell, main, then page).
		for _, name := range []string{"--ftl-app-bg", "--ftl-app-main-bg", "--ftl-bg"} {
			if base := parseCSSColor(get(name)); base != nil {
				surface = mix(surface, base)
				break
			}
		}
	}
	r, g, b := float64(surface[0])/255, float64(surface[1])/255, float64(surface[2])/255
	lum := 0.2126*r + 0.7152*g + 0.0722*b
	if lum > 0.55 {
		return "light"
	}
	return "dark"
}

// parseCSSColor reads the subset of CSS colour syntaxes the theme files use
// (6-digit hex and rgb/rgba). Nil means "not parsed, use the dark default".
func parseCSSColor(v string) []float64 {
	if m := themeHexRe.FindStringSubmatch(v); m != nil {
		n, _ := strconv.ParseUint(m[1], 16, 32)
		return []float64{float64((n >> 16) & 255), float64((n >> 8) & 255), float64(n & 255), 1}
	}
	if m := themeRGBARe.FindStringSubmatch(v); m != nil {
		r, _ := strconv.ParseFloat(m[1], 64)
		g, _ := strconv.ParseFloat(m[2], 64)
		b, _ := strconv.ParseFloat(m[3], 64)
		a := 1.0
		if strings.HasPrefix(strings.TrimSpace(v), "rgba(") {
			a, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v[strings.LastIndex(v, ",")+1:]), ")"), 64)
		}
		return []float64{r, g, b, a}
	}
	return nil
}

func mix(top, base []float64) []float64 {
	a := top[3]
	out := make([]float64, 4)
	for i := 0; i < 3; i++ {
		out[i] = top[i]*a + base[i]*(1-a)
	}
	out[3] = 1
	return out
}

// ThemeNames reports the valid data-theme values (discovered themes plus the
// token-driven "custom" theme, which has no file).
func ThemeNames() []string {
	names := []string{"custom"}
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
