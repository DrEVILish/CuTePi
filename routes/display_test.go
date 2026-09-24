package routes

import (
	"testing"
)

// parseXrandrModes accepts the connected-mode lines xrandr --query prints
// (a current mode carries the *) and rejects resolution-shaped tokens from
// unrelated lines.
func TestParseXrandrModes(t *testing.T) {
	got := parseXrandrModes(`Screen 0: minimum 8 x 8, current 3840 x 2160
HDMI-0 connected primary 3840x2160+0+0 (normal left inverted right x axis y axis) 600mm x 340mm
   3840x2160     60.00*+  30.00
   1920x1080    120.00   119.88
   640x480       59.94
DP-1 disconnected (normal left inverted right x axis y axis)
`)
	if got["3840x2160"] != true || got["1920x1080"] != true {
		t.Fatalf("xrandr parse missed modes: %v", got)
	}
	if got["640x480"] != true {
		t.Errorf("xrandr offers ALL listed modes (any listed refresh is selectable), not only the starred one: %v", got)
	}
}

func TestParseDRMModes(t *testing.T) {
	got := parseDRMModes("1920x1080 60 59.94\n3840x2160 30 29.97 60\ngarbage\n2160x3840\n")
	if got["1920x1080"] != true || got["3840x2160"] != true {
		t.Fatalf("drm parse missed modes: %v", got)
	}
	if got["2160x3840"] {
		t.Errorf("shown width-first token misparsed: %v", got)
	}
}

func TestDisplayModesFallback(t *testing.T) {
	// No xrandr + no DRM file (CI container): the dropdown must still have
	// sane content.
	modes := displayModes()
	if len(modes) == 0 {
		t.Fatalf("no fallback modes")
	}
}
