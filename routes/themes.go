package routes

import (
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

// Theme is one discoverable UI theme: a single portable CSS file in
// public/css/themes/<name>.css. Dropping a new name.css into that folder is
// enough — it is served as a static file, listed by Themes(), linked in
// header.html and offered in the settings UI with no code changes. Each file
// is self-contained (tokens + generic element rules under
// html[data-theme="<name>"], CuTePi-specific flourishes in a marked section)
// so it can move to another app by copying the file and setting data-theme.
type Theme struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	File  string `json:"file"`
}

// themeNameRe reads the display label from a theme file's header comment:
// "Theme-Name: Human Readable". Files without it fall back to the filename.
var themeNameRe = regexp.MustCompile(`(?m)^\s*\*?\s*Theme-Name:\s*(.+?)\s*$`)

// themesDir locates the themes folder whether the process runs from the repo
// root (server, smoke tests) or from routes/ (go test).
func themesDir() string {
	for _, d := range []string{"./public/css/themes", "../public/css/themes"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "./public/css/themes"
}

// Themes scans the themes folder on every call — a handful of small files,
// so the settings UI and header links are always current without a restart
// after adding a file.
func Themes() []Theme {
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
		out = append(out, Theme{Name: name, Label: label, File: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ThemeNames reports the valid data-theme values (discovered files plus the
// token-driven "custom" theme, which has no file).
func ThemeNames() []string {
	names := []string{"custom"}
	for _, t := range Themes() {
		names = append(names, t.Name)
	}
	return names
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
		"div":         func(a, b int) int { return a / b },
		"hasBit":      func(mask, bit int) bool { return mask & (1 << uint(bit)) != 0 },
		// safeCSS passes server-generated CSS values (e.g. a colour with a
		// var() fallback) through html/template's style sanitizer, which
		// otherwise rewrites them to ZgotmplZ.
		"safeCSS":     func(s string) template.CSS { return template.CSS(s) },
		"add":         func(a, b int) int { return a + b },
		"mod":         func(a, b int) int { return a % b },
		"listDays": func() []string {
			return []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
		},
		"themeFiles": Themes,
		"assetStamp": AssetStamp,
	}
}

// registerThemeRoutes serves the discovered theme list for the settings UI
// and the boot-time theme allowlist (public/src/ui.js).
func registerThemeRoutes(rg *gin.RouterGroup) {
	rg.GET("/themes", func(c *gin.Context) {
		c.JSON(http.StatusOK, Themes())
	})
}
