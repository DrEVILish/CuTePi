package routes

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// Display-mode enumeration for the Settings > Display tab. Sources, in
// order of authority:
//   1. xrandr (the console runs under X for the wall)
//   2. the kernel DRM connector list (/sys/class/drm/*/modes)
//   3. nothing readable -> a small sane preset list so the dropdown is
//      never empty and the operator can still pin a mode there.
// Every path degrades without failing the request.

// presetModes keeps the dropdown useful when no display subsystem is
// reachable (CI, container, headless boot before the wall powers up).
var presetModes = []string{"1280x720", "1920x1080", "2560x1440", "3840x2160"}

var (
	xrandrModeRe = regexp.MustCompile(`^\s*(\d{3,5}x\d{2,5})\s+\d+(?:\.\d+)?`)
	drmModeRe    = regexp.MustCompile(`^(\d{3,5}x\d{2,5})\s+\d+(?:\.\d+)?\b`)
)

// parseXrandrModes picks the resolution tokens xrandr reports (star = the
// current mode).
func parseXrandrModes(text string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if m := xrandrModeRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// parseDRMModes reads a /sys/class/drm connector's `modes` file
// (pi drm: "1920x1080 60 59.94 1920x1080x24" lines, one mode per line).
func parseDRMModes(text string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		if m := drmModeRe.FindStringSubmatch(sc.Text()); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// displayModes gathers the distinct resolution strings the wall can take:
// xrandr first (it reports the connected sink), then the kernel DRM list,
// then presets.
func displayModes() []string {
	modes := map[string]bool{}
	if xrandr, err := exec.LookPath("xrandr"); err == nil {
		if out, err := exec.Command(xrandr, "--query").CombinedOutput(); err == nil {
			modes = parseXrandrModes(string(out))
		}
	}
	if len(modes) == 0 {
		drmPaths, _ := filepath.Glob("/sys/class/drm/*/modes")
		for _, p := range drmPaths {
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			for k := range parseDRMModes(string(data)) {
				modes[k] = true
			}
		}
	}
	if len(modes) == 0 {
		return presetModes
	}
	out := make([]string, 0, len(modes))
	for mode := range modes {
		out = append(out, mode)
	}
	return out
}

// registerDisplayRoute exposes the list for the Settings > Display tab.
func registerDisplayRoute(rg *gin.RouterGroup) {
	rg.GET("/display/modes", func(c *gin.Context) {
		c.JSON(200, gin.H{"modes": displayModes()})
	})
}
