package media

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Playback warning (DESIGN §5.7): from the probed codec, resolution and frame
// rate, estimate whether a video plays at full rate on this hardware. It
// informs; it never refuses a file.
//
// The rates are the steady decode rates measured at 1920x1080 on the Raspberry
// Pi 4 through the plane wall (README "Plane wall with CuTePi's presenter",
// 2026-10-20); an estimate scales them by pixel count. Codecs with no
// measurement get no warning: only measured limits are reported.

// pi4 reports whether this machine is a Raspberry Pi 4 (the only hardware the
// table was measured on). A variable so tests can set it.
var pi4 = sync.OnceValue(func() bool {
	b, err := os.ReadFile("/proc/device-tree/model")
	return err == nil && strings.Contains(string(b), "Raspberry Pi 4")
})

const px1080 = 1920 * 1080

// decodeRate1080 is the measured steady rate at 1080p for v on the Pi 4, the
// decoder kind, and whether a measurement exists.
func decodeRate1080(v *MediaVideoInfo) (fps float64, hw bool, ok bool) {
	codec, profile := strings.ToLower(v.Codec), strings.ToLower(v.Profile)
	tenBit := strings.Contains(v.PixFmt, "10") || strings.Contains(v.PixFmt, "12")
	switch codec {
	case "hevc":
		// Hardware (v4l2slh265dec) to 4Kp60: four times the 1080p60 rate.
		return 240, true, true
	case "h264":
		if tenBit {
			return 51, false, true // High 10: avdec_h264
		}
		return 75, true, true // v4l2h264dec, ~75 fps ceiling at 1080p (PERFORMANCE_PROPOSALS)
	case "cfhd":
		if v.Alpha || strings.Contains(v.PixFmt, "gbr") || strings.Contains(v.PixFmt, "rgb") || strings.Contains(v.PixFmt, "444") {
			return 33, false, true
		}
		return 50, false, true
	case "prores":
		if strings.Contains(profile, "4444") {
			switch {
			case v.Alpha:
				return 27, false, true
			case strings.Contains(profile, "xq"):
				return 37, false, true
			}
			return 29, false, true
		}
	case "vp9":
		if v.Alpha {
			return 26, false, true
		}
	case "av1":
		return 60, false, true // cutepidav1ddec at 1080p60, with little margin
	}
	return 0, false, false
}

// PlaybackWarning is "" when v should play at full rate (or nothing is known),
// otherwise a one-line warning such as "HEVC 2160p: hardware decode, about 20
// fps expected on this Pi (file 60 fps)". displayHz is the wall's refresh (0
// when unknown: 60 is assumed).
func PlaybackWarning(v *MediaVideoInfo, displayHz int) string {
	if v == nil || v.Width <= 0 || v.Height <= 0 || !pi4() {
		return ""
	}
	return playbackWarning(v, displayHz)
}

func playbackWarning(v *MediaVideoInfo, displayHz int) string {
	if displayHz <= 0 {
		displayHz = 60
	}
	target := float64(displayHz)
	if v.FPS > 0 && v.FPS < target {
		target = v.FPS
	}
	name := fmt.Sprintf("%s %dp", strings.ToUpper(v.Codec), v.Height)
	// The Pi 4's H.264 block decodes up to 1920x1088; larger goes to software,
	// whose rate there was never measured.
	if strings.EqualFold(v.Codec, "h264") && v.Width*v.Height > 1920*1088 {
		return name + ": above the hardware decoder's 1080p, software decode; expect dropped frames"
	}
	rate, hw, ok := decodeRate1080(v)
	if !ok {
		return ""
	}
	expected := rate * px1080 / float64(v.Width*v.Height)
	if expected >= target*0.95 {
		return ""
	}
	kind := "software"
	if hw {
		kind = "hardware"
	}
	return fmt.Sprintf("%s: %s decode, about %.0f fps expected on this Pi (needs %.0f)", name, kind, expected, target)
}
