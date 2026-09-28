package routes

import (
	"os/exec"
	"strings"

	"github.com/gin-gonic/gin"
)

// Audio-output enumeration for the Settings > Audio tab. Parses `aplay -L`:
// non-indented lines are device IDs ("hw:CARD=vc4hdmi0,DEV=0"), each followed
// by indented description lines. "null" is skipped (not a destination).
// No ALSA tooling -> empty list, and the tab keeps its manual text input,
// so the request never fails and the operator can always type a device.

type audioDevice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func parseAplayDevices(text string) []audioDevice {
	var out []audioDevice
	var cur *audioDevice
	flush := func() {
		if cur != nil && cur.ID != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur != nil && cur.Label == cur.ID {
				cur.Label = cur.ID + " — " + strings.TrimSpace(line)
			}
			continue
		}
		flush()
		id := strings.TrimSpace(line)
		if id == "" || id == "null" {
			continue
		}
		cur = &audioDevice{ID: id, Label: id}
	}
	flush()
	return out
}

func audioDevices() []audioDevice {
	aplay, err := exec.LookPath("aplay")
	if err != nil {
		return nil
	}
	out, err := exec.Command(aplay, "-L").Output()
	if err != nil {
		return nil
	}
	return parseAplayDevices(string(out))
}

// registerAudioRoute exposes the list for the Settings > Audio tab.
func registerAudioRoute(rg *gin.RouterGroup) {
	rg.GET("/audio/devices", func(c *gin.Context) {
		devs := audioDevices()
		if devs == nil {
			devs = []audioDevice{}
		}
		c.JSON(200, gin.H{"devices": devs})
	})
}
