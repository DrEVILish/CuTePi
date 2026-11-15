package ctp

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// stateKeyEscFadeMs persists the ESC fade-out time in milliseconds
// (default 500): single ESC fades the running output over this time, then
// stops. Set in Settings > General.
const stateKeyEscFadeMs = "escFadeMs"

// DefaultEscFadeMs is the ESC fade time when nothing is stored.
const DefaultEscFadeMs = 1000

// GetEscFadeMs reports the ESC fade-out time in ms.
func GetEscFadeMs() int {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyEscFadeMs); err != nil {
		return DefaultEscFadeMs
	}
	ms, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil || ms < 0 {
		return DefaultEscFadeMs
	}
	return ms
}

// SetEscFadeMs persists the ESC fade-out time (0..10000 ms).
func SetEscFadeMs(ms int) error {
	if err := ValidateEscFadeMs(ms); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyEscFadeMs, strconv.Itoa(ms))
	return err
}

// colorRe is the only colour syntax stored for cues and groups: CSS hex
// (#rgb, #rgba, #rrggbb, #rrggbbaa). Group colours are emitted into style
// attributes through safeCSS (which bypasses html/template's CSS
// sanitiser), so a looser "starts with #" check let a value like
// "#000;background:url(//evil)" inject arbitrary CSS.
var colorRe = regexp.MustCompile(`^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// ValidColor reports whether c is empty (no colour) or a CSS hex colour.
func ValidColor(c string) bool {
	return c == "" || colorRe.MatchString(c)
}

// ValidateEscFadeMs checks an Esc fade duration without persisting it.
func ValidateEscFadeMs(ms int) error {
	if ms < 0 || ms > 10000 {
		return fmt.Errorf("esc fade must be 0..10000 ms")
	}
	return nil
}

// stateKeyGoAdvance persists the GO-bar behaviour: fire the selected unit and
// then move the selection to the next one (default on), so repeated GOs walk
// the show. The group- and cue-play routes consult it after firing.
const stateKeyGoAdvance = "goAdvance"

// GetGoAdvance reports whether firing GO advances the selection.
func GetGoAdvance() bool {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyGoAdvance); err != nil {
		return true // default on
	}
	return strings.TrimSpace(val) != "0"
}

// SetGoAdvance persists the GO-advance behaviour.
func SetGoAdvance(on bool) error {
	v := "1"
	if !on {
		v = "0"
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyGoAdvance, v)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// stateKeyShowMode persists Edit/Show mode: Show mode arms scheduled cue
// triggers and test-pattern output; Edit mode (default) is for building the
// show without either firing.
const stateKeyShowMode = "showMode"

// GetShowMode reports whether the console is in Show mode.
func GetShowMode() bool {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyShowMode); err != nil {
		return false // default Edit
	}
	return strings.TrimSpace(val) != "0"
}

// SetShowMode persists Edit/Show mode.
func SetShowMode(on bool) error {
	v := "1"
	if !on {
		v = "0"
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyShowMode, v)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// Cue health results (§12.3): every fire records its outcome so a mid-show
// scan finds broken cues at a glance.
const (
	CueResultNever = 0
	CueResultOK    = 1
	CueResultError = 2
)

// Panic holding image (§12.9): when set, PANIC loads-and-holds this pool
// item instead of cutting to black. Empty = off (plain panic).
const stateKeyPanicHold = "panicHoldImage"

// GetPanicHoldImage returns the configured holding image filename ("" = off).
func GetPanicHoldImage() string {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyPanicHold); err != nil {
		return ""
	}
	return strings.TrimSpace(val)
}

// SetPanicHoldImage persists the holding image ("" clears it).
func SetPanicHoldImage(filename string) error {
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyPanicHold, strings.TrimSpace(filename))
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// Test patterns (§12.10): pool items the operator pinned into the Tests
// menu. Persisted as a JSON filename list in state; built-in GStreamer
// patterns live in the routes layer.
const stateKeyTestPatterns = "testPatterns"

// TestPatterns lists the custom (pool) test patterns.
func TestPatterns() []string {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyTestPatterns); err != nil {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(val), &out)
	return out
}

// AddTestPattern pins a media item into the Tests menu (idempotent).
func AddTestPattern(filename string) error {
	list := TestPatterns()
	for _, p := range list {
		if p == filename {
			return nil
		}
	}
	return saveTestPatterns(append(list, filename))
}

// RemoveTestPattern unpins a media item (removing it from the Tests menu
// without touching the media itself).
func RemoveTestPattern(filename string) error {
	list := TestPatterns()
	out := make([]string, 0, len(list))
	for _, p := range list {
		if p != filename {
			out = append(out, p)
		}
	}
	return saveTestPatterns(out)
}

func saveTestPatterns(list []string) error {
	blob, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyTestPatterns, string(blob))
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// stateKeyAutoNumber persists auto-numbering (§12.5): new cues numbered
// 1, 2, 3…; mid-sheet inserts take the numeric midpoint of their
// neighbours. Off = the plain MAX+1 sequence with no midpoint inserts.
const stateKeyAutoNumber = "autoNumberCues"

// GetAutoNumber reports whether auto-numbering is on (default on).
func GetAutoNumber() bool {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyAutoNumber); err != nil {
		return true
	}
	return strings.TrimSpace(val) != "0"
}

// SetAutoNumber persists the auto-numbering setting.
func SetAutoNumber(on bool) error {
	v := "1"
	if !on {
		v = "0"
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyAutoNumber, v)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// validGeom accepts pixels ("960", "960px", "-20") or a percentage ("50%").
func validGeom(v string) bool {
	v = strings.TrimSuffix(strings.TrimSuffix(v, "%"), "px")
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// stateKeyTestOverlay: test patterns carry a label with the display's
// resolution and refresh rate (§12.10). Off by default.
const stateKeyTestOverlay = "testOverlay"

// GetTestOverlay reports whether test patterns show the display-mode label.
func GetTestOverlay() bool {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyTestOverlay); err != nil {
		return false
	}
	return strings.TrimSpace(val) == "1"
}

// SetTestOverlay persists the test-pattern display-mode label setting.
func SetTestOverlay(on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyTestOverlay, v)
	return err
}
