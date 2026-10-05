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

// Theme is one selectable UI theme. Every theme comes from the pinned
// ftl-themes submodule (third_party/ftl-themes, contract v4): a bundle that
// carries a layout as well as a palette, keyed on html[data-theme=<Name>].
// CuTePi ships no themes of its own.
type Theme struct {
	// ID is the stable identifier stored in localStorage and used by the
	// settings picker: "ftl:<slug>".
	ID string `json:"id"`
	// Name is the html[data-theme] value the bundle is keyed on (the slug).
	Name string `json:"name"`
	// Label is the human-readable name shown in Settings.
	Label string `json:"label"`
	// Href is the bundle URL to link for this theme.
	Href string `json:"href"`
	// Scheme is "light" or "dark": how Bootstrap's data-bs-theme is set while
	// the theme is active.
	Scheme string `json:"scheme"`
	// Version is the generated ftl-themes CSS content hash for cache busting.
	Version string `json:"version"`
	// Variants are the theme's sub-themes (html[data-variant=<id>]), offered
	// beneath it in the picker (contract "Palette variants").
	Variants []ThemeVariant `json:"variants,omitempty"`
	// Tint is the theme's user-chosen colour, if it declares one: the picker
	// must then offer a colour control labelled Tint.Label (contract "Theme
	// tint"), applied as an inline custom property on <html>.
	Tint *ThemeTint `json:"tint,omitempty"`
}

// ThemeVariant is one palette variant of a theme.
type ThemeVariant struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ThemeTint is a theme's declared tint token.
type ThemeTint struct {
	Token   string `json:"token"`
	Default string `json:"default"`
	Label   string `json:"label"`
}

// DefaultThemeID is what an unset or unrecognised preference resolves to:
// xbmc — near-black home-theatre shell, glowing blue underline selection.
const DefaultThemeID = "ftl:xbmc"

// themeDistDir locates the pinned ftl-themes bundles whether the process runs
// from the repo root (server) or from routes/ (go test).
func themeDistDir() string { return findRepoDir("third_party/ftl-themes/dist") }

func findRepoDir(rel string) string {
	for _, prefix := range []string{"./", "../"} {
		d := prefix + rel
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "./" + rel
}

// Themes reads the submodule's generated manifest on every call (one small
// file), so the picker is always current without a restart. A missing
// submodule means no themes: the page then renders with the app's base CSS.
func Themes() []Theme {
	data, err := os.ReadFile(filepath.Join(themeDistDir(), "themes.json"))
	if err != nil {
		return nil
	}
	var manifest []struct {
		Slug     string         `json:"slug"`
		Label    string         `json:"label"`
		Scheme   string         `json:"scheme"`
		Version  string         `json:"version"`
		Variants []ThemeVariant `json:"variants"`
		Tint     *ThemeTint     `json:"tint"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	out := make([]Theme, 0, len(manifest))
	for _, m := range manifest {
		if m.Slug == "" {
			continue
		}
		scheme := m.Scheme
		if scheme == "" {
			scheme = "dark"
		}
		out = append(out, Theme{
			ID:    "ftl:" + m.Slug,
			Name:  m.Slug,
			Label: m.Label,
			// v4 bundles wrap themselves in @layer ui; header.html orders
			// the layers so the app's unlayered CSS always wins.
			Href:     "/ftl/themes/" + m.Slug + ".css",
			Scheme:   scheme,
			Version:  m.Version,
			Variants: m.Variants,
			Tint:     validTint(m.Tint),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// validTint keeps a tint only if its token is a custom property name and its
// default a #rrggbb colour: both are written into the page (an inline style
// on <html>), so nothing else from the manifest reaches it.
func validTint(t *ThemeTint) *ThemeTint {
	if t == nil || !tokenRE.MatchString(t.Token) || !hexColorRE.MatchString(t.Default) {
		return nil
	}
	return t
}

var (
	tokenRE    = regexp.MustCompile(`^--[a-z0-9-]+$`)
	hexColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// ThemeNames reports the valid data-theme values.
func ThemeNames() []string {
	names := []string{}
	for _, t := range Themes() {
		names = append(names, t.Name)
	}
	return names
}

// ThemeMap is the id -> {name, href, scheme, variants, tint} map the
// pre-paint boot script in header.html uses to pick a stylesheet, variant and
// tint before the first render, and that ui.js reuses when the picker changes.
func ThemeMap() template.JS {
	m := map[string]Theme{}
	for _, t := range Themes() {
		m[t.ID] = t
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

// registerThemeRoutes serves the theme list for the settings UI.
func registerThemeRoutes(rg *gin.RouterGroup) {
	rg.GET("/themes", func(c *gin.Context) {
		c.JSON(http.StatusOK, Themes())
	})
}
