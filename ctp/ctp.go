package ctp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"sort"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/media"
	"CuTePi/ws"
)

type Media struct {
	Media_id         int       `db:"media_id"`
	Filename         string    `db:"filename"`
	Mimetype         string    `db:"mimetype"`
	Size             int       `db:"size"`
	Duration         float64   `db:"duration"`
	Resolution       string    `db:"resolution"`
	ThumbnailPending bool      `db:"thumbnail_pending"`
	Waveform         string    `db:"waveform"`   // JSON array of amplitude peaks (0..1), "" if unanalysed
	MediaInfo        string    `db:"media_meta"` // JSON MediaInfo (container/codec detail), "" on old imports
	WaveformPending  bool      `db:"waveform_pending"`
	Missing          bool      `db:"missing"` // source file absent from disk (startup scan)
	DateAdded        time.Time `db:"date_added"`
	LoudnessGain     float64   `db:"loudness_gain"`
}

type Cue struct {
	Media
	Cue_id         int     `db:"cue_id"`
	CuePos         int     `db:"cuePos"`
	CueNum         string  `db:"cueNum"`
	Media_id       int     `db:"media_id"`
	Title          string  `db:"title"`
	PosStart       int     `db:"posStart"`
	PosEnd         int     `db:"posEnd"`
	PreWait        int     `db:"preWait"`
	CueDuration    int     `db:"cueDuration"`
	PostWait       int     `db:"postWait"`
	Hold           bool    `db:"hold"`
	Loop           bool    `db:"loop"`
	LoopCount      int     `db:"loop_count"` // 0 = infinite, N = play N times (when loop is on)
	Color          string  `db:"color"`
	Parent         int     `db:"parent"`
	FadeOut        int     `db:"fadeOut"` // ms; fade & stop other cues over this time
	FadeAction     string  `db:"fadeAction"`
	AutoContinue   bool    `db:"autoContinue"`
	Volume         float64 `db:"volume"` // per-cue master gain in dB; 0 = 0dB
	FadeIn         int     `db:"fadeIn"` // ms; audio and video fade from silence/black
	Rate           float64 `db:"rate"`
	Balance        float64 `db:"balance"`
	Mute           bool    `db:"mute"`
	LastResult     int     `db:"last_result"`    // 0 never played, 1 ok, 2 error (§12.3)
	LastPlayedAt   int64   `db:"last_played_at"` // unix ms of the last fire
	SheetIndex     float64 `db:"sheet_index"`    // visual+playback order (§4)
	FadeCurve      string  `db:"fade_curve"`     // linear|smooth|log|exp (§12.7)
	FitMode        string  `db:"fit_mode"`       // fit|stretch: image/video frame fitting (§5.5)
	Rotation       int     `db:"rotation"`       // 0|90|180|270 clockwise degrees (§5.5)
	Flip           string  `db:"flip"`           // none|h|v: mirror horizontal/vertical (§5.5)
	ScheduleEnabled bool   `db:"schedule_enabled"` // whether scheduling is enabled for this cue
	ScheduleDays    int     `db:"schedule_days"`    // bitmask: bit0=Mon, bit1=Tue, ..., bit6=Sun
	ScheduleTimeMs  int     `db:"schedule_time_ms"` // time of day in milliseconds since 00:00:00
	PreWaitFmt     string
	CueDurationFmt string
	PostWaitFmt    string
	MediaType      string // "video" | "audio" | "image" | "other", for the row icon
	Selected       bool
	Playing        bool   // true if this cue is the currently playing file
	PlayPos        int    // ms into the playing clip (progress bar) when Playing
	PlayDur        int    // ms total duration of the playing clip
	InSelection    bool   // member of the multi-selection (§12.4); anchor uses Selected
	WaitKind       string // "pre"|"post" while a chain wait counts down on this cue (§12.2)
	WaitLeftS      int    // whole seconds left in that wait (render-time)
}

type Cuesheet struct {
	Cues   []Cue
	Groups []Group // cue groups (folder membership is a presentation layer)
}

type Mediapool struct {
	Medias []Media
}

var (
	csMu      sync.Mutex
	csVersion uint64
)

func bumpCuesheetVersion() {
	csMu.Lock()
	csVersion++
	csMu.Unlock()
	go ws.Broadcast()
}

// NotifyCuesheetChanged is the exported bump for outside packages that
// mutate presentation state (routes' wait countdowns) without touching rows.
func NotifyCuesheetChanged() {
	bumpCuesheetVersion()
}

func bumpMediaVersion() {
	go ws.BroadcastMedia()
}

// CuesheetVersion returns the monotonic version for the cuesheet/selection
// state. Clients poll this to stay in sync; server is source of truth.
func CuesheetVersion() uint64 {
	csMu.Lock()
	defer csMu.Unlock()
	return csVersion
}

// stateKeySelectedCue is the persisted "currently selected cue position"
// (0 = none). Selection is server-authoritative DB state: it survives a
// reload and is shared across clients.
const stateKeySelectedCue = "selectedCuePos"

// SelectedCuePos returns the persisted position of the selected cue (0 = none).
func SelectedCuePos() (int, error) {
	var val string
	err := db.Get(&val, `SELECT value FROM state WHERE key = ?;`, stateKeySelectedCue)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pos, err := strconv.Atoi(val)
	if err != nil {
		return 0, err
	}
	return pos, nil
}

func setSelectedCuePos(pos int) error {
	if pos < 0 {
		pos = 0
	}
	// Single-selection writes clear the multi-selection (§12.4): the sheet
	// goes back to one selected row. Bulk-selection writes go through
	// SetSelection, which persists both halves.
	_, _ = db.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedSet)
	_, err := db.Exec(`
		INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;
	`, stateKeySelectedCue, strconv.Itoa(pos))
	if err != nil {
		log.Printf("Error storing selected cue: %v", err)
		return err
	}
	bumpCuesheetVersion()
	return err
}

func GetCue(cuePos string) (cue Cue, err error) {
	query := `
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		WHERE cuePos = :cuePos
	`
	err = db.Get(&cue, query, sql.Named("cuePos", cuePos))
	if err != nil {
		log.Printf("Error Getting Cue: %v", err)
		return Cue{}, err // Return an empty Cue
	}
	cue.PreWaitFmt = FormatTime(cue.PreWait)
	cue.CueDurationFmt = FormatTime(effectiveCueDuration(cue))
	cue.PostWaitFmt = FormatTime(cue.PostWait)
	cue.MediaType = mediaTypeFromMimetype(cue.Mimetype)
	return cue, nil // Return the found Cue
}

// effectiveCueDuration returns the duration shown to the operator. A valid
// trim window takes precedence; otherwise use an explicitly stored duration,
// then the probed media duration. New cues leave cueDuration at zero, so they
// still display the real media length instead of 00:00:00.000.
// mediaTypeFromMimetype maps a media mimetype to the short kind string used
// for the cue row's media-type icon and the fade/stop logic.
func mediaTypeFromMimetype(mimetype string) string {
	switch {
	case strings.HasPrefix(mimetype, "video/"):
		return "video"
	case strings.HasPrefix(mimetype, "audio/"):
		return "audio"
	case strings.HasPrefix(mimetype, "image/"):
		return "image"
	default:
		return "other"
	}
}

func effectiveCueDuration(cue Cue) int {
	// Rate correction: playback duration is source duration divided by rate
	rate := cue.Rate
	if rate <= 0 {
		rate = 1
	}
	if cue.PosEnd > cue.PosStart {
		return int(float64(cue.PosEnd-cue.PosStart) / rate)
	}
	if cue.CueDuration > 0 {
		return int(float64(cue.CueDuration) / rate)
	}
	if cue.Duration > 0 {
		// Duration is in seconds; convert to ms and apply rate correction
		return int(float64(cue.Duration)/rate*1000 + 0.5)
	}
	return 0
}

func SetCue(cuePos string) (err error) {
	pos, err := strconv.Atoi(cuePos)
	if err != nil {
		log.Printf("Error setting cue: %v", err)
		return err
	}
	return setSelectedCuePos(pos)
}

// NextCue moves the selection to the next existing cue position after the
// currently selected one (walking gaps left by deletions), persisting it via
// the state table. If nothing is selected, the first cue is selected.
func NextCue() (err error) {
	cur, err := SelectedCuePos()
	if err != nil {
		return err
	}
	// Visual order (sheet_index), like playback and arrow navigation:
	// cuePos order diverges after a drag-reorder that doesn't reindex.
	next, err := nextSheetCue(cur, 1)
	if err != nil {
		log.Printf("Error getting the next cue position: %v", err)
		return err
	}
	if next == 0 {
		return nil
	}
	return setSelectedCuePos(next)
}

// SelectedGroupPos returns the selected group id (>0) when the persisted
// selection points at a group header (selections store groups as the negative
// group id), or 0 when a cue (or nothing) is selected.
func SelectedGroupPos() (int, error) {
	var val string
	err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeySelectedCue)
	if err != nil {
		return 0, err
	}
	id, perr := strconv.Atoi(strings.TrimSpace(val))
	if perr != nil {
		return 0, nil
	}
	if id >= 0 {
		return 0, nil
	}
	return -id, nil
}

// setSelectedGroupPos persists a group-header selection (encoded negative).
// UPSERT, not UPDATE: on a fresh DB the state key row may not exist yet, and
// a bare UPDATE would silently no-op (same shape as setSelectedCuePos).
// Like every single-selection write, it clears the multi-selection set.
func setSelectedGroupPos(groupID int) error {
	_, _ = db.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedSet)
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`,
		stateKeySelectedCue, strconv.Itoa(-groupID))
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// SetSelectedGroup is the exported group-selection setter used by the routes.
func SetSelectedGroup(groupID int) error {
	return setSelectedGroupPos(groupID)
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

// stateKeySelectedSet persists multi-selection (§12.4): the JSON list of
// cuePos values in the selection (the ANCHOR is the classic selectedCuePos).
// Any single-selection write clears it.
const stateKeySelectedSet = "selectedCueSet"

// SelectedSet returns the multi-selection cue positions (excluding the
// anchor, which is tracked by selectedCuePos).
func SelectedSet() []int {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeySelectedSet); err != nil {
		return nil
	}
	var out []int
	_ = json.Unmarshal([]byte(val), &out)
	return out
}

// setSelectedSet persists the multi-selection (empty list clears it).
func setSelectedSet(set []int) error {
	if len(set) == 0 {
		_, err := db.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedSet)
		if err != nil {
			return err
		}
		bumpCuesheetVersion()
		return nil
	}
	blob, err := json.Marshal(set)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeySelectedSet, string(blob))
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// SetSelection persists anchor + multi-selection in one call.
func SetSelection(anchor int, set []int) error {
	if err := setSelectedCuePos(anchor); err != nil {
		return err
	}
	return setSelectedSet(set)
}

// SetGroupSelection is SetSelection for a possibly group-anchored selection:
// anchor is a cuePos (>0), a group id (<0), or 0 for none. setSelectedCuePos
// clamps negatives to 0, so group anchors persist directly.
func SetGroupSelection(anchor int, set []int) error {
	if anchor < 0 {
		if err := setSelectedGroupPos(-anchor); err != nil {
			return err
		}
		return setSelectedSet(set)
	}
	return SetSelection(anchor, set)
}

// SetCueResult records a cue's last fire outcome (and when). Unknown cues
// are ignored (best-effort: the sheet may have changed under a runner).
func SetCueResult(cuePos int, result int) {
	_, err := db.Exec(`UPDATE cuesheet SET last_result = ?, last_played_at = ?
		WHERE cuePos = ?`, result, time.Now().UnixMilli(), cuePos)
	if err != nil {
		log.Printf("SetCueResult(%d): %v", cuePos, err)
		return
	}
	bumpCuesheetVersion()
}

// ClearCueResults resets every cue's health state (operator action).
func ClearCueResults() error {
	_, err := db.Exec(`UPDATE cuesheet SET last_result = 0, last_played_at = 0`)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

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

// SelectUnit is one selectable entry in sheet order: a cue row or a group
// header (exported — the renderer's GO-bar preview walks the same list, so
// the preview can never disagree with what GO/Space fires). Collapsed groups
// contribute only their header; their members are unreachable.
type SelectUnit struct {
	CuePos  int  // >0 for cues, 0 for group headers
	GroupID int  // >0 for group headers, 0 for cues
	IsGroup bool // GroupID > 0
}

// FlatRow is one rendered (and keyboard-selectable) row of the cuesheet in
// visual order: a group header (Group != nil) or a cue (Cue != nil), at the
// given nesting depth (0 = top level). Shared by the cuesheet renderer and
// the keyboard-selection walk so the two can never disagree.
type FlatRow struct {
	Group *Group
	Cue   *Cue
	Depth int
	// GroupColor is the innermost containing group's colour ("" = none),
	// carried so member rows can draw the folder outline in it.
	GroupColor string
	// Group-boundary flags for the folder outline (§5.4): a cue is the
	// first/last rendered member of its direct group's run.
	FirstInGroup bool
	LastInGroup  bool
}

// groupAncestors returns the chain of parent group ids above gid (nearest
// first), guarded against parent-cycles and dangling parents.
func groupAncestors(gid int, byID map[int]Group) []int {
	var chain []int
	g, ok := byID[gid]
	for ok && g.ParentGroupID != 0 && len(chain) <= len(byID) {
		if chainContains(chain, g.ParentGroupID) {
			break // cycle: stop walking
		}
		chain = append(chain, g.ParentGroupID)
		g, ok = byID[g.ParentGroupID]
	}
	return chain
}

func chainContains(chain []int, id int) bool {
	for _, v := range chain {
		if v == id {
			return true
		}
	}
	return false
}

// cueInCollapsed reports whether the cue belongs to gid's subtree — a direct
// member or a member of a descendant subgroup. Only those rows hide when gid
// collapses; anything else parked inside the span renders at its position.
// Dangling parents (healed at startup) and parent-cycles never match: the
// walk is capped at the group count.
func cueInCollapsed(cue *Cue, gid int, byID map[int]Group) bool {
	for p, n := cue.Parent, 0; p != 0 && n <= len(byID); n++ {
		if p == gid {
			return true
		}
		g, ok := byID[p]
		if !ok {
			return false
		}
		p = g.ParentGroupID
	}
	return false
}

// FlattenSheet folds the MERGED visual sequence (cues and group headers by
// sheet_index) into rendered rows: a group header (Group != nil) or a cue
// (Cue != nil) at its nesting depth. A header OPENS a span; the next header
// of the same or shallower depth closes it. A cue renders inside the
// innermost open span only when its STORED parent equals that span's group —
// otherwise as a top-level gap row at its position. Read-only: never writes.
// Collapsing hides a group's subtree members only (header stays); unrelated
// rows parked inside the span keep rendering. Empty groups render wherever
// their header sits — every group position is real, nothing trails.
func FlattenSheet(cs *Cuesheet) []FlatRow {
	byID := make(map[int]Group, len(cs.Groups))
	for _, g := range cs.Groups {
		byID[g.GroupID] = g
	}
	hidden := make(map[int]bool, len(cs.Groups))
	depthOf := make(map[int]int, len(cs.Groups))
	for gid := range byID {
		chain := groupAncestors(gid, byID)
		depthOf[gid] = len(chain)
		for _, anc := range chain {
			if byID[anc].Collapse {
				hidden[gid] = true
				break
			}
		}
	}

	groups := make([]Group, len(cs.Groups))
	copy(groups, cs.Groups)
	sort.Slice(groups, func(i, j int) bool { return groups[i].SheetIndex < groups[j].SheetIndex })

	type item struct {
		kind string // "cue" | "group"
		cue  *Cue
		g    *Group
	}
	items := make([]item, 0, len(cs.Cues)+len(cs.Groups))
	gi := 0
	for _, cue := range cs.Cues {
		for gi < len(groups) && groups[gi].SheetIndex <= cue.SheetIndex {
			g := groups[gi]
			items = append(items, item{kind: "group", g: &g})
			gi++
		}
		c := cue
		items = append(items, item{kind: "cue", cue: &c})
	}
	for ; gi < len(groups); gi++ {
		g := groups[gi]
		items = append(items, item{kind: "group", g: &g})
	}

	rows := make([]FlatRow, 0, len(items))
	type span struct {
		groupID  int
		depth    int
		lastRow  int // index in rows of the last emitted row of this span
		skipping bool
	}
	var stack []span
	closeTo := func(depth int) {
		for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.lastRow >= 0 && top.lastRow < len(rows) {
				rows[top.lastRow].LastInGroup = true // bottom edge of the folder outline
			}
		}
	}
	for it := range items {
		if items[it].kind == "group" {
			g := items[it].g
			d := depthOf[g.GroupID]
			closeTo(d)
			// Header visible unless a PROPER ancestor is collapsed.
			if hidden[g.GroupID] {
				stack = append(stack, span{groupID: g.GroupID, depth: d, lastRow: -1, skipping: true})
				continue
			}
		rows = append(rows, FlatRow{Group: g, Depth: d})
		// A collapsed group's own members are hidden too (not just
		// descendants of collapsed ancestors): the header stays, its span
		// skips. Without this, collapsing changed state but rendered
		// nothing — the button and arrow keys looked dead.
		stack = append(stack, span{groupID: g.GroupID, depth: d, lastRow: len(rows) - 1, skipping: g.Collapse})
		continue
		}
		// A cue: member of the innermost open span.
		if len(stack) == 0 {
			rows = append(rows, FlatRow{Cue: items[it].cue, Depth: 0})
			continue
		}
		top := stack[len(stack)-1]
		if top.skipping && cueInCollapsed(items[it].cue, top.groupID, byID) {
			continue // member of a collapsed group's subtree: not rendered
		}
		// Literal membership (§6.4): the cue renders inside this span only
		// when its STORED parent is this span's group. Anything else —
		// top-level cues and strays whose group sits elsewhere — renders as
		// a plain top-level row at its position; the folder outline closes
		// above it and resumes after it. The renderer never rewrites.
		if items[it].cue.Parent != top.groupID {
			if top.lastRow >= 0 && top.lastRow < len(rows) {
				rows[top.lastRow].LastInGroup = true
			}
			rows = append(rows, FlatRow{Cue: items[it].cue, Depth: 0})
			top.lastRow = -1
			stack[len(stack)-1] = top
			continue
		}
		// The innermost open span's colour draws the member's folder
		// outline (QLab-style); empty when the group has no colour.
		groupColor := ""
		if g, ok := byID[top.groupID]; ok {
			groupColor = g.Color
		}
		cue := items[it].cue
		// Members sit one level deeper than their group header (§6.4):
		// top-depth + 1 so the CSS indent formula puts the first level at
		// 1.5rem, +1.1rem per nesting level after.
		rows = append(rows, FlatRow{Cue: cue, Depth: top.depth + 1, GroupColor: groupColor,
			FirstInGroup: top.lastRow >= 0 && rows[top.lastRow].Group != nil})
		// The header directly above us? Its span now owns this row.
		for i := range stack {
			if stack[i].lastRow == len(rows)-2 && stack[i].lastRow >= 0 && rows[stack[i].lastRow].Group != nil {
				stack[i].lastRow = len(rows) - 1
			}
		}
		top.lastRow = len(rows) - 1
		stack[len(stack)-1] = top
	}
	closeTo(-1) // close everything at the sequence end
	return rows
}

// SelectUnits returns the sheet's selectable entries in order: every rendered
// row (group headers at any nesting level, plus their visible cues). Users of
// collapsed groups are skipped entirely.
func SelectUnits() ([]SelectUnit, error) {
	cs, err := GetCuesheet()
	if err != nil {
		return nil, err
	}
	rows := FlattenSheet(&cs)
	units := make([]SelectUnit, 0, len(rows))
	for _, r := range rows {
		if r.Group != nil {
			units = append(units, SelectUnit{GroupID: r.Group.GroupID, IsGroup: true})
			continue
		}
		units = append(units, SelectUnit{CuePos: r.Cue.CuePos})
	}
	return units, nil
}

// SelectionStepAt returns the current selection expressed on SelectUnits'
// index space (or -1 when nothing is selected).
func SelectUnitIndex() (int, error) {
	units, err := SelectUnits()
	if err != nil {
		return -1, err
	}
	var selCue, selGroup int
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?;`, stateKeySelectedCue); err == nil {
		selInt, perr := strconv.Atoi(strings.TrimSpace(val))
		if perr == nil {
			if selInt < 0 {
				selGroup = -selInt
			} else {
				selCue = selInt
			}
		}
	}
	for i, u := range units {
		if selGroup != 0 && u.GroupID == selGroup {
			return i, nil
		}
		if selGroup == 0 && u.CuePos == selCue {
			return i, nil
		}
	}
	return -1, nil
}

// ExtendSelection grows the anchor+set selection to cover every VISIBLE unit
// between the anchor and the target (SelectUnits order: visual, collapsed
// members excluded, headers included as -groupID). The anchor keeps its
// identity (cue pos or -groupID); everything else in the span joins the set.
// No anchor (or anchor/target off-sheet, e.g. after a re-render) falls back
// to a plain single-select of the target.
func ExtendSelection(targetCue, targetGroup int) error {
	units, err := SelectUnits()
	if err != nil {
		return err
	}
	ti := -1
	for i, u := range units {
		if targetGroup != 0 && u.IsGroup && u.GroupID == targetGroup {
			ti = i
			break
		}
		if targetGroup == 0 && !u.IsGroup && u.CuePos == targetCue {
			ti = i
			break
		}
	}
	ai, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	if ai < 0 || ti < 0 {
		if targetGroup != 0 {
			return SetSelectedGroup(targetGroup)
		}
		return SetCue(strconv.Itoa(targetCue))
	}
	var anchor int
	if sel, _ := SelectedCuePos(); sel != 0 {
		anchor = sel
	} else if gid, _ := SelectedGroupPos(); gid != 0 {
		anchor = -gid
	}
	lo, hi := ai, ti
	if lo > hi {
		lo, hi = hi, lo
	}
	set := make([]int, 0, hi-lo)
	for _, u := range units[lo : hi+1] {
		var id int
		if u.IsGroup {
			id = -u.GroupID
		} else {
			id = u.CuePos
		}
		if id != anchor {
			set = append(set, id)
		}
	}
	// Keep selection members outside the span (matches the old numeric-range
	// behaviour of accumulating, not replacing). Stale entries (cues/groups
	// deleted since they were selected, e.g. past a bulk delete) are pruned:
	// a dead position must not sneak back into a later bulk op.
	have := make(map[int]bool, len(set))
	for _, p := range set {
		have[p] = true
	}
	live, _ := selectedSetPruned()
	for _, p := range live {
		if !have[p] {
			set = append(set, p)
		}
	}
	return SetGroupSelection(anchor, set)
}

// selectedSetPruned returns the stored multi-selection minus entries whose
// row no longer exists (deleted cues/groups). Dead positions otherwise hide
// in the set until a later ExtendSelection/Bulk call resurrects them.
func selectedSetPruned() ([]int, error) {
	set := SelectedSet()
	if len(set) == 0 {
		return set, nil
	}
	out := make([]int, 0, len(set))
	for _, p := range set {
		if p < 0 {
			var g int
			if err := db.Get(&g, `SELECT group_id FROM cue_group WHERE group_id = ?`, -p); err == nil {
				out = append(out, p)
			}
			continue
		}
		var c int
		if err := db.Get(&c, `SELECT cuePos FROM cuesheet WHERE cuePos = ?`, p); err == nil {
			out = append(out, p)
		}
	}
	if len(out) != len(set) {
		_ = setSelectedSet(out)
	}
	return out, nil
}

// ExtendStep grows/shrinks the multi-selection one visible unit for
// Shift+arrows (dir +1 down, -1 up). The head is the selected unit farthest
// along dir; it steps one further — or back toward the anchor, shrinking the
// span. Stepping onto the anchor clears the set. No anchor: plain step.
func ExtendStep(dir int) error {
	units, err := SelectUnits()
	if err != nil {
		return err
	}
	ai, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	if ai < 0 {
		return SelectStep(dir)
	}
	idOf := func(u SelectUnit) int {
		if u.IsGroup {
			return -u.GroupID
		}
		return u.CuePos
	}
	at := make(map[int]int, len(units))
	for i, u := range units {
		at[idOf(u)] = i
	}
	var anchor int
	if sel, _ := SelectedCuePos(); sel != 0 {
		anchor = sel
	} else if gid, _ := SelectedGroupPos(); gid != 0 {
		anchor = -gid
	}
	// The head (focus end) isn't stored: take the selected unit farthest
	// from the anchor, preferring the stepping side on ties. Stepping back
	// toward the anchor shrinks the span; stepping onto it clears the set.
	head := ai
	best := -1
	for _, p := range SelectedSet() {
		i, ok := at[p]
		if !ok {
			continue
		}
		d := i - ai
		if d < 0 {
			d = -d
		}
		if d > best || d == best && (dir > 0 && i > head || dir < 0 && i < head) {
			best, head = d, i
		}
	}
	head += dir
	if head < 0 || head >= len(units) {
		return nil
	}
	if head == ai {
		return SetGroupSelection(anchor, nil)
	}
	lo, hi := ai, head
	if lo > hi {
		lo, hi = hi, lo
	}
	set := make([]int, 0, hi-lo)
	for _, u := range units[lo : hi+1] {
		if id := idOf(u); id != anchor {
			set = append(set, id)
		}
	}
	return SetGroupSelection(anchor, set)
}

// ExpandGroup uncollapses a group so a joined cue lands visibly instead of
// vanishing into a closed folder. No-op when already open.
func ExpandGroup(groupID int) error {
	_, err := db.Exec(`UPDATE cue_group SET collapse = 0 WHERE group_id = ?`, groupID)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// SelectStep moves the selection one unit forward/backward through
// SelectUnits()' list (groups and cues alike); clamps at the ends.
func SelectStep(dir int) (err error) {
	units, err := SelectUnits()
	if err != nil || len(units) == 0 {
		return err
	}
	idx, err := SelectUnitIndex()
	if err != nil {
		return err
	}
	idx += dir
	if idx < 0 || idx >= len(units) {
		return nil
	}
	u := units[idx]
	if u.IsGroup {
		return setSelectedGroupPos(u.GroupID)
	}
	return setSelectedCuePos(u.CuePos)
}

// routes.autoContinueFrom), so only the query for that next cue remains.
// NextCuePos returns the next existing cue position strictly after endingPos
// in sheet order, or 0 (and nil) when endingPos is the last cue.
func NextCuePos(endingPos int) (int, error) {
	return nextSheetCue(endingPos, 1)
}

// PrevCue moves the selection to the previous existing cue position before
// the currently selected one. It never moves below position 1 (0 = nothing
// selected, so there is nothing to go "previous" from).
func PrevCue() (err error) {
	cur, err := SelectedCuePos()
	if err != nil {
		return err
	}
	// Visual order (sheet_index), like NextCue above.
	prev, err := nextSheetCue(cur, -1)
	if err != nil {
		log.Printf("Error getting the previous cue position: %v", err)
		return err
	}
	if prev == 0 {
		return nil
	}
	return setSelectedCuePos(prev)
}

func cuesheetLength() (int, error) {
	var length int
	err := db.Get(&length, `SELECT COUNT(*) FROM cuesheet;`)
	if err != nil {
		log.Printf("Error getting cuesheet length: %v", err)
		return 0, err
	}
	return length, nil
}

func GetCuesheet() (cuesheet Cuesheet, err error) {
	var cues []Cue

	query := `
		SELECT *
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		ORDER BY cuesheet.sheet_index, cuesheet.cuePos
	`

	err = db.Select(&cues, query)
	if err != nil {
		log.Printf("Error GetCuesheet: %v", err)
		return Cuesheet{}, err
	}

	selected, err := SelectedCuePos()
	if err != nil {
		log.Printf("Error reading selected cue: %v", err)
	}

	inSet := make(map[int]bool)
	for _, p := range SelectedSet() {
		inSet[p] = true
	}
	for i := range cues {
		if cues[i].CuePos == selected {
			cues[i].Selected = true
		}
		if inSet[cues[i].CuePos] {
			cues[i].InSelection = true
		}
		cues[i].PreWaitFmt = FormatTime(cues[i].PreWait)
		cues[i].CueDurationFmt = FormatTime(effectiveCueDuration(cues[i]))
		cues[i].PostWaitFmt = FormatTime(cues[i].PostWait)
		cues[i].MediaType = mediaTypeFromMimetype(cues[i].Mimetype)
	}
	groups, _ := Groups()
	return Cuesheet{cues, groups}, nil
}

func GetMediapool() (pool Mediapool, err error) {
	var medias []Media
	// media_id DESC breaks ties: CURRENT_TIMESTAMP only has 1-second
	// resolution, so a multi-file upload can easily register several rows
	// with an identical date_added, which would otherwise leave their
	// relative order (newest-first, per spec) unspecified.
	query := `SELECT * FROM mediapool ORDER BY date_added DESC, media_id DESC`
	err = db.Select(&medias, query)
	if err != nil {
		log.Printf("Error GetMediapool: %v", err)
		return Mediapool{}, err
	}
	return Mediapool{medias}, nil
}

// MarkMissingFiles flags mediapool rows whose source file is no longer on
// disk. Runs once at startup (see main.go): no periodic scan. A file that is
// only temporarily absent is re-flagged as present on the next startup.
func MarkMissingFiles() {
	var filenames []string
	if err := db.Select(&filenames, `SELECT filename FROM mediapool`); err != nil {
		log.Printf("Error scanning media for missing files: %v", err)
		return
	}
	for _, f := range filenames {
		_, err := os.Stat(filepath.Join(config.MediaLocation(), f))
		missing := err != nil
		if _, uerr := db.Exec(`UPDATE mediapool SET missing = ? WHERE filename = ?`, boolInt(missing), f); uerr != nil {
			log.Printf("Error flagging media %q missing=%v: %v", f, missing, uerr)
		}
	}
	bumpMediaVersion()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReplaceCueMedia re-links a cue to a different media item (used to repair a
// cue whose original source file went missing). The cue's title and position
// are kept; only media_id changes.
func ReplaceCueMedia(cuePos, filename string) error {
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	var id int
	if err := db.Get(&id, `SELECT media_id FROM mediapool WHERE filename = ?`, filename); err != nil {
		return fmt.Errorf("media %q not found", filename)
	}
	res, err := db.Exec(`UPDATE cuesheet SET media_id = ? WHERE cuePos = ?;`, id, cuePosInt)
	if err != nil {
		log.Printf("Error re-linking cue %s to %q: %v", cuePos, filename, err)
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("cue at position %d not found", cuePosInt)
	}
	bumpCuesheetVersion()
	return nil
}

// RenameMedia renames a media item in the pool and updates every cue that
// references the old filename, keeping the media_id (and so thumbnails,
// waveforms and cue FKs) stable. The caller is responsible for renaming the
// file on disk; this only reconciles the pool/cuesheet. Fails if newName
// collides with an existing pool entry.
func RenameMedia(oldName, newName string) error {
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM mediapool WHERE filename = ?`, newName); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("a media file named %q already exists in the pool", newName)
	}
	_, err := db.Exec(`UPDATE mediapool SET filename = ? WHERE filename = ?;`, newName, oldName)
	if err != nil {
		log.Printf("Error renaming media %q -> %q: %v", oldName, newName, err)
		return err
	}
	bumpMediaVersion()
	bumpCuesheetVersion()
	return nil
}

// stateKeyAutoNumber persists auto-numbering (§12.5): new cues numbered
// 5, 10, 15…; mid-sheet inserts take the numeric midpoint of their
// neighbours. Off = the legacy MAX+1 integer sequence.
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

// parseCueNumFloat parses a cueNum that is a plain decimal number ("12",
// "12.5"); ok=false for hand-set text numbers, which auto-numbering never
// touches.
func parseCueNumFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// formatCueNum renders a cue number without a trailing ".0" (12 → "12",
// 12.5 → "12.5").
func formatCueNum(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

// nextCueNum computes the cue number for a new cue landing at insertAt
// (post-bump position). With auto-numbering on: an append gets the next
// multiple of 5; a mid-sheet insert between two numeric neighbours gets
// their midpoint ("12.5" between 12 and 13). Off (or non-numeric
// neighbours) falls back to the legacy MAX+1.
func nextCueNum(tx *sqlx.Tx, insertAt int, auto bool) (string, error) {
	var maxNum sql.NullFloat64
	if err := tx.Get(&maxNum, `SELECT MAX(CAST(cueNum AS REAL)) FROM cuesheet`); err != nil {
		return "", err
	}
	if !auto {
		return formatCueNum(maxNum.Float64 + 1), nil
	}
	if insertAt > 1 {
		var prevNum, nextNum sql.NullString
		_ = tx.Get(&prevNum, `SELECT cueNum FROM cuesheet WHERE cuePos = ?`, insertAt-1)
		_ = tx.Get(&nextNum, `SELECT cueNum FROM cuesheet WHERE cuePos = ?`, insertAt)
		p, pok := parseCueNumFloat(prevNum.String)
		n, nok := parseCueNumFloat(nextNum.String)
		if pok && nok && n > p {
			// Walk up from the midpoint in 0.5 steps until the number is
			// free (the UNIQUE constraint makes collision a hard error).
			mid := (p + n) / 2
			for step := 0.0; step < 50; step += 0.5 {
				cand := formatCueNum(mid + step)
				var exists int
				if err := tx.Get(&exists, `SELECT COUNT(*) FROM cuesheet WHERE cueNum = ?`, cand); err != nil {
					return "", err
				}
				if exists == 0 {
					return cand, nil
				}
			}
		}
	}
	// Append (or unparseable neighbours): next multiple of 5 above the max.
	base := math.Max(0, maxNum.Float64)
	return formatCueNum(math.Ceil((base+0.1)/5) * 5), nil
}

// BulkEdit applies one operation to many cues in ONE transaction (§12.4),
// so half a bulk edit can never persist. Ops: color, fadeAction, fadeCurve
// (string value), fadeOut (ms), autoContinue (bool), delete, group
// (parentGroupID; membership normalised after).
func BulkEdit(op, value string, positions []int) error {
	if len(positions) == 0 {
		return nil
	}
	// Group assignment moves the selection as one block to the group's end —
	// position and membership stored together by SheetDrop. It runs outside
	// the write tx below (SheetDrop has its own; SQLite runs
	// single-connection and any pool query while a tx holds the connection
	// deadlocks).
	if op == "group" {
		gid, perr := strconv.Atoi(value)
		if perr != nil {
			return fmt.Errorf("invalid groupId %q", value)
		}
		if gid != 0 {
			if _, err := GetGroup(gid); err != nil {
				return err
			}
			// Visual order keeps the block's relative order intact.
			var all []int
			if err := db.Select(&all, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
				return err
			}
			want := map[int]bool{}
			for _, p := range positions {
				want[p] = true
			}
			var ordered []int
			for _, p := range all {
				if want[p] {
					ordered = append(ordered, p)
				}
			}
			return SheetDrop(ordered, 0, "group", gid, true, false, false, nil, 0)
		}
		// gid == 0 releases in place: rows keep their positions and render
		// as gap rows where they stand. Falls through to the tx below.
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	switch op {
	case "color":
		if value != "" && !strings.HasPrefix(value, "#") {
			return fmt.Errorf("invalid color %q", value)
		}
		for _, p := range positions {
			if _, err := tx.Exec(`UPDATE cuesheet SET color = ? WHERE cuePos = ?`, value, p); err != nil {
				return err
			}
		}
	case "fadeAction", "fadeCurve":
		if _, err := parseCueColumn(op, value); err != nil {
			return err
		}
		col := op
		if op == "fadeCurve" {
			col = "fade_curve"
		}
		for _, p := range positions {
			if _, err := tx.Exec(`UPDATE cuesheet SET `+col+` = ? WHERE cuePos = ?`, value, p); err != nil {
				return err
			}
		}
	case "fadeOut":
		v, perr := strconv.Atoi(value)
		if perr != nil || v < 0 {
			return fmt.Errorf("invalid fadeOut %q", value)
		}
		for _, p := range positions {
			if _, err := tx.Exec(`UPDATE cuesheet SET fadeOut = ? WHERE cuePos = ?`, v, p); err != nil {
				return err
			}
		}
	case "autoContinue":
		v := 0
		if value == "true" || value == "1" {
			v = 1
		}
		for _, p := range positions {
			if _, err := tx.Exec(`UPDATE cuesheet SET autoContinue = ? WHERE cuePos = ?`, v, p); err != nil {
				return err
			}
		}
	case "delete":
		for _, p := range positions {
			if _, err := tx.Exec(`DELETE FROM cuesheet WHERE cuePos = ?`, p); err != nil {
				return err
			}
		}
	case "group":
		// Only gid == 0 reaches here (release in place); joins take the
		// SheetDrop path above.
		for _, p := range positions {
			if _, err := tx.Exec(`UPDATE cuesheet SET parent = 0 WHERE cuePos = ?`, p); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown bulk op %q", op)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	if op == "delete" {
		// Reindex to 1..N after removals, preserving visual order.
		var rest []int
		if err := db.Select(&rest, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
			return err
		}
		if _, err := ReorderCues(rest); err != nil {
			return err
		}
		// Clear the selection only if the selected cue was among the
		// deleted ones; ReorderCues already follows surviving selections.
		deleted := make(map[int]bool, len(positions))
		for _, p := range positions {
			deleted[p] = true
		}
		if sel, _ := SelectedCuePos(); deleted[sel] {
			_ = setSelectedCuePos(0)
			_ = setSelectedSet(nil)
		}
	}
	return nil
}

// BulkGroupNewAt creates a group AT the anchor cue's slot containing the
// given cues (relative visual order kept), in one action (§12.4). Rolls the
// group back if any step fails.
func BulkGroupNewAt(positions []int, atCue int) (int, error) {
	if len(positions) == 0 {
		return 0, fmt.Errorf("no cues to group")
	}
	id, err := CreateGroup("New Group", 0)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int, error) {
		_ = DeleteGroup(id)
		return 0, err
	}
	inSet := make(map[int]bool, len(positions))
	for _, p := range positions {
		inSet[p] = true
	}
	parents, err := storedParents()
	if err != nil {
		return fail(err)
	}
	for p := range inSet {
		parents[p] = id
	}
	seq, err := loadSheetSequence()
	if err != nil {
		return fail(err)
	}
	// The block takes the anchor's slot: count surviving rows before it.
	anchorIdx := -1
	for i, item := range seq {
		if item.Kind == "cue" && item.CuePos == atCue {
			anchorIdx = i
			break
		}
	}
	slot := 0
	var members []SheetItem
	rest := make([]SheetItem, 0, len(seq))
	for i, item := range seq {
		if item.Kind == "group" && item.GroupID == id {
			continue // fresh header: re-seated below
		}
		if item.Kind == "cue" && inSet[item.CuePos] {
			members = append(members, item)
			continue
		}
		rest = append(rest, item)
		if anchorIdx >= 0 && i < anchorIdx {
			slot++
		}
	}
	if anchorIdx < 0 {
		slot = len(rest)
	}
	out := make([]SheetItem, 0, len(rest)+len(members)+1)
	out = append(out, rest[:slot]...)
	out = append(out, SheetItem{Kind: "group", GroupID: id})
	out = append(out, members...)
	out = append(out, rest[slot:]...)
	if err := applyOrder(out, parents); err != nil {
		return fail(err)
	}
	return id, nil
}

// RenumberCues renumbers every cue 5, 10, 15… in sheet order (§12.5). An// explicit operator action — it rewrites hand-set numbers. Two-phase like
// ReorderCues: temporary high numbers first so the UNIQUE constraint never
// trips mid-reassign.
func RenumberCues() error {
	length, err := cuesheetLength()
	if err != nil {
		return err
	}
	var order []int
	if err := db.Select(&order, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	offset := length + 100000
	for _, pos := range order {
		if _, err := tx.Exec(`UPDATE cuesheet SET cueNum = ? WHERE cuePos = ?`,
			strconv.Itoa(pos+offset), pos); err != nil {
			return err
		}
	}
	for i, pos := range order {
		if _, err := tx.Exec(`UPDATE cuesheet SET cueNum = ? WHERE cuePos = ?`,
			formatCueNum(float64((i+1)*5)), pos); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

func AddCue(filename string, cuePos string) (err error) {
	title, err := uniqueCueField("title", filename)
	if err != nil {
		return err
	}
	// Read settings BEFORE the tx opens: SQLite runs single-connection, so
	// any pool query while the tx holds the connection deadlocks.
	autoNumber := GetAutoNumber()
	// Target position: an empty cuePos appends after the last cue; otherwise
	// cuePos names the cue the new row lands in front of — or, when that cue
	// opens its group's run, in front of the group's header (above the
	// folder, not as its first member).
	nextPos := 0
	if cuePos == "" {
		if err := db.Get(&nextPos, `SELECT COALESCE(MAX(cuePos), 0) + 1 FROM cuesheet`); err != nil {
			log.Printf("Error reading next cuePos: %v", err)
			return err
		}
	} else {
		if nextPos, err = strconv.Atoi(cuePos); err != nil {
			return err
		}
	}
	// Visual placement, resolved BEFORE the bump renumbers anything. The new
	// row's parent is the owner of the gap it lands in — computed once and
	// stored; appends always land top-level (parent 0).
	newParent := 0
	newSheetIndex := 0.0
	if cuePos != "" {
		seq, serr := loadSheetSequence()
		if serr != nil {
			return serr
		}
		at := len(seq)
		for i, item := range seq {
			if item.Kind == "cue" && item.CuePos == nextPos {
				at = i
				break
			}
		}
		if at > 0 && at < len(seq) && seq[at].Kind == "cue" && seq[at-1].Kind == "group" {
			var tp int
			if err := db.Get(&tp, `SELECT parent FROM cuesheet WHERE cuePos = ?`, nextPos); err == nil && tp == seq[at-1].GroupID {
				at-- // target opens its group's run: land above the header
			}
		}
		var err error
		newSheetIndex, err = gapSheetIndex(seq, at)
		if err != nil {
			return err
		}
		withNew := make([]SheetItem, 0, len(seq)+1)
		withNew = append(withNew, seq[:at]...)
		withNew = append(withNew, SheetItem{Kind: "cue", CuePos: nextPos})
		withNew = append(withNew, seq[at:]...)
		owner, err := gapOwner(withNew, at)
		if err != nil {
			return err
		}
		newParent = owner
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Bump the target position and everything after it up by one to make
	// space for the new cue. For an append, nothing is >= nextPos, so this
	// is a no-op and both paths share one insert statement. This can't be a
	// single set-based UPDATE: SQLite enforces the cuePos UNIQUE constraint
	// per-row as it processes the statement, so bumping e.g. position 1 to 2
	// while position 2 is still occupied (not yet bumped to 3) fails with a
	// transient UNIQUE violation depending on internal row order. Updating
	// highest-position-first, one row at a time, guarantees each target slot
	// is vacated before it's claimed.
	var positions []int
	err = tx.Select(&positions, `SELECT cuePos FROM cuesheet WHERE cuePos >= ? ORDER BY cuePos DESC`, nextPos)
	if err != nil {
		log.Printf("Error reading cuePos values to bump: %v", err)
		return err
	}
	for _, p := range positions {
		_, err = tx.Exec(`UPDATE cuesheet SET cuePos = cuePos + 1 WHERE cuePos = ?;`, p)
		if err != nil {
			log.Printf("Error updating cuePos: %v", err)
			return err
		}
	}

	// Visual index (§4): append = end of the sequence (resolved placement
	// above already set newSheetIndex for explicit positions).
	if cuePos == "" {
		if err := tx.Get(&newSheetIndex, `SELECT COALESCE(MAX(sheet_index), 0) + 1000 FROM cuesheet`); err != nil {
			return err
		}
	}
	// insert new cue at the nextPos position. cueNum comes from nextCueNum
	// (§12.5): auto-number 5,10,15… / numeric midpoint, or the legacy MAX+1.
	newCueNum, err := nextCueNum(tx, nextPos, autoNumber)
	if err != nil {
		log.Printf("Error computing cueNum: %v", err)
		return err
	}
	var result sql.Result
	result, err = tx.Exec(`
		INSERT INTO cuesheet (cuePos, cueNum, media_id, title, hold, loop, loop_count, sheet_index, parent)
		SELECT
			? AS cuePos,
			? AS cueNum,
			mp.media_id,
			? AS title,
			0, 0, 0, ?, ?
		FROM
			(SELECT media_id FROM mediapool WHERE filename = ?) AS mp;
	`, nextPos, newCueNum, title, newSheetIndex, newParent, filename)
	if err != nil {
		log.Printf("Error inserting into cuesheet: %v", err)
		return err // Log the error instead of panicking
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("media %q not found", filename)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// AddCueToGroup appends a media file as a cue directly inside a group
// (media drop onto a header means JOIN). cuePos appends at the end; the
// fractional sheet_index seats it as the first (or last) member. Rows sit
// on whole sheetIndexStep multiples, so +1 offsets never collide.
func AddCueToGroup(filename string, groupID int, first bool) error {
	title, err := uniqueCueField("title", filename)
	if err != nil {
		return err
	}
	autoNumber := GetAutoNumber()
	var headerIdx float64
	if err := db.Get(&headerIdx, `SELECT sheet_index FROM cue_group WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	// The group opens: the new cue must be visible on arrival.
	if err := ExpandGroup(groupID); err != nil {
		return err
	}
	var nextPos int
	if err := db.Get(&nextPos, `SELECT COALESCE(MAX(cuePos), 0) + 1 FROM cuesheet`); err != nil {
		return err
	}
	seat := headerIdx + 1
	if !first {
		var lastMember sql.NullFloat64
		_ = db.Get(&lastMember, `SELECT MAX(sheet_index) FROM cuesheet WHERE parent = ?`, groupID)
		if lastMember.Valid {
			seat = lastMember.Float64 + 1
		}
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	newCueNum, err := nextCueNum(tx, nextPos, autoNumber)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`
		INSERT INTO cuesheet (cuePos, cueNum, media_id, title, hold, loop, loop_count, sheet_index, parent)
		SELECT
			? AS cuePos,
			? AS cueNum,
			mp.media_id,
			? AS title,
			0, 0, 0, ?, ?
		FROM
			(SELECT media_id FROM mediapool WHERE filename = ?) AS mp;
	`, nextPos, newCueNum, title, seat, groupID, filename)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("media %q not found", filename)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// gapSheetIndex returns the sheet_index for a new row spliced at index at of
// seq: the midpoint of its new neighbours (half the leader for a head
// insert, leader + step for an append).
func gapSheetIndex(seq []SheetItem, at int) (float64, error) {
	sheetIndexOf := func(it SheetItem) (float64, error) {
		// NULL means unindexed (HealSheet backfills these at startup); 0 is
		// a legitimate head index and must survive as-is.
		if it.Kind == "group" {
			var v sql.NullFloat64
			if err := db.Get(&v, `SELECT sheet_index FROM cue_group WHERE group_id = ?`, it.GroupID); err != nil {
				return 0, err
			}
			if !v.Valid {
				return sheetIndexStep, nil
			}
			return v.Float64, nil
		}
		var v sql.NullFloat64
		if err := db.Get(&v, `SELECT sheet_index FROM cuesheet WHERE cuePos = ?`, it.CuePos); err != nil {
			return 0, err
		}
		if !v.Valid {
			return sheetIndexStep, nil
		}
		return v.Float64, nil
	}
	if at <= 0 {
		if len(seq) == 0 {
			return sheetIndexStep, nil
		}
		hi, err := sheetIndexOf(seq[0])
		if err != nil {
			return 0, err
		}
		// Halving a real 0 still sorts first: the merge tie-break puts a
		// cue before a header on equal index.
		return hi / 2, nil
	}
	if at >= len(seq) {
		lo, err := sheetIndexOf(seq[len(seq)-1])
		if err != nil {
			return 0, err
		}
		return lo + sheetIndexStep, nil
	}
	lo, err := sheetIndexOf(seq[at-1])
	if err != nil {
		return 0, err
	}
	hi, err := sheetIndexOf(seq[at])
	if err != nil {
		return 0, err
	}
	if hi <= lo {
		hi = lo + sheetIndexStep
	}
	return lo + (hi-lo)/2, nil
}

// editableCueColumns allow-lists which cuesheet columns may be updated via
// UpdateCue, since column names cannot be parameterized as bind values and
// col otherwise comes straight from a URL path segment.
var editableCueColumns = map[string]bool{
	"cueNum":       true,
	"title":        true,
	"posStart":     true,
	"posEnd":       true,
	"preWait":      true,
	"cueDuration":  true,
	"postWait":     true,
	"hold":         true,
	"loop":         true,
	"loop_count":   true,
	"color":        true,
	"fadeCurve":    true,
	"parent":       true,
	"fadeOut":      true,
	"fadeAction":   true,
	"autoContinue": true,
	"volume":       true,
	"fadeIn":       true,
	"rate":         true,
	"balance":      true,
	"mute":         true,
}

// CueColumnValue returns the current string value of one of the
// editable cue columns, for pre-filling the inline-edit form. col is
// checked against the same allow-list as UpdateCue.
func CueColumnValue(cue Cue, col string) (string, error) {
	if !editableCueColumns[col] {
		return "", fmt.Errorf("cue column %q is not editable", col)
	}
	switch col {
	case "cueNum":
		return cue.CueNum, nil
	case "title":
		return cue.Title, nil
	case "posStart":
		return FormatTime(cue.PosStart), nil
	case "posEnd":
		return FormatTime(cue.PosEnd), nil
	case "preWait":
		return FormatTime(cue.PreWait), nil
	case "cueDuration":
		return FormatTime(effectiveCueDuration(cue)), nil
	case "postWait":
		return FormatTime(cue.PostWait), nil
	case "hold":
		return strconv.FormatBool(cue.Hold), nil
	case "loop":
		return strconv.FormatBool(cue.Loop), nil
	case "loop_count":
		return strconv.Itoa(cue.LoopCount), nil
	case "color":
		return cue.Color, nil
	case "parent":
		return strconv.Itoa(cue.Parent), nil
	case "fadeOut":
		return FormatTime(cue.FadeOut), nil
	case "fadeAction":
		return cue.FadeAction, nil
	case "autoContinue":
		return strconv.FormatBool(cue.AutoContinue), nil
	case "volume":
		return strconv.FormatFloat(cue.Volume, 'f', -1, 64), nil
	case "fadeIn":
		return FormatTime(cue.FadeIn), nil
	case "rate":
		return strconv.FormatFloat(cue.Rate, 'f', -1, 64), nil
	case "balance":
		return strconv.FormatFloat(cue.Balance, 'f', -1, 64), nil
	case "mute":
		return strconv.FormatBool(cue.Mute), nil
	default:
		return "", fmt.Errorf("cue column %q is not editable", col)
	}
}

func parseBool(val string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no", "":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", val)
	}
}

func UpdateCue(cuePos string, col string, val string) (err error) {
	if !editableCueColumns[col] {
		return fmt.Errorf("cue column %q is not editable", col)
	}
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}

	// For time columns, parse the input (hh:mm:ss.ms or bare seconds); for
	// boolean columns, normalize the common true/false spellings.
	setVal, err := parseCueColumn(col, val)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		UPDATE cuesheet
		SET `+col+` = ?
		WHERE cuePos = ?;`, setVal, cuePosInt)
	if err != nil {
		log.Printf("Error updating cue: %v", err)
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// parseCueColumn validates and normalizes one editable-cue-column value into
// the SQL literal to set (per-column parser shared by UpdateCue and the
// batched UpdateCueFields).
func parseCueColumn(col string, val string) (string, error) {
	switch col {
	case "posStart", "posEnd", "preWait", "cueDuration", "postWait", "fadeOut", "fadeIn":
		ms, perr := ParseTime(val)
		if perr != nil {
			return "", fmt.Errorf("invalid time value for %s: %w", col, perr)
		}
		return strconv.Itoa(ms), nil
	case "hold", "loop", "autoContinue", "mute":
		b, berr := parseBool(val)
		if berr != nil {
			return "", fmt.Errorf("invalid %s value: %w", col, berr)
		}
		if b {
			return "1", nil
		}
		return "0", nil
	case "loop_count":
		n, perr := strconv.Atoi(strings.TrimSpace(val))
		if perr != nil || n < 0 {
			return "", fmt.Errorf("invalid loop_count %q (want an integer >= 0)", val)
		}
		return strconv.Itoa(n), nil
	case "fadeAction":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "peers", "list", "all":
			return strings.ToLower(strings.TrimSpace(val)), nil
		default:
			return "", fmt.Errorf("invalid fadeAction %q (want peers, list or all)", val)
		}
	case "parent":
		if _, perr := strconv.Atoi(val); perr != nil {
			return "", fmt.Errorf("invalid parent %q (want an integer)", val)
		}
		return val, nil
	case "color":
		c := strings.TrimSpace(val)
		if c != "" && !strings.HasPrefix(c, "#") {
			return "", fmt.Errorf("invalid color %q (want a #rrggbb hex value)", val)
		}
		return c, nil
	case "fadeCurve":
		v := strings.TrimSpace(val)
		switch v {
		case "linear", "smooth", "log", "exp":
			return v, nil
		}
		return "", fmt.Errorf("invalid fadeCurve %q (want linear, smooth, log or exp)", val)
	case "fit_mode":
		switch v := strings.TrimSpace(val); v {
		case "fit", "stretch":
			return v, nil
		default:
			return "", fmt.Errorf("invalid fit_mode %q (want fit or stretch)", val)
		}
	case "rotation":
		switch strings.TrimSpace(val) {
		case "0", "90", "180", "270":
			return strings.TrimSpace(val), nil
		default:
			return "", fmt.Errorf("invalid rotation %q (want 0, 90, 180 or 270)", val)
		}
	case "flip":
		switch v := strings.TrimSpace(val); v {
		case "none", "h", "v":
			return v, nil
		default:
			return "", fmt.Errorf("invalid flip %q (want none, h or v)", val)
		}
	case "volume", "rate", "balance":
		// Per-cue master gain in dB; 0 = 0dB. Clamped to the slider's range.
		f, perr := strconv.ParseFloat(val, 64)
		if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("invalid %s %q (want a finite number)", col, val)
		}
		lo, hi := -60.0, 12.0
		if col == "rate" {
			lo, hi = 0.25, 4
		}
		if col == "balance" {
			lo, hi = -1, 1
		}
		f = math.Max(lo, math.Min(hi, f))
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	default:
		return val, nil
	}
}

// UpdateCueFields batched-writes several editable cue columns in ONE
// statement, raising the cuesheet version (and thus the WebSocket sync
// signal) exactly once, after every write has committed. The per-column
// inspector save used to call UpdateCue repeatedly, so its middle writes
// broadcast before the final state existed and client refetches rendered a
// half-committed trim.
// col names come from the editableCueColumns allow-list, so composing the
// SET clause is injection-safe.
func UpdateCueFields(cuePos string, fields map[string]string) (err error) {
	if len(fields) == 0 {
		return nil
	}
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	cols := make([]string, 0, len(fields))
	args := make([]interface{}, 0, len(fields)+1)
	for _, col := range []string{
		"cueNum", "title", "posStart", "posEnd", "preWait", "cueDuration",
		"postWait", "hold", "loop", "loop_count", "color", "parent", "fadeOut",
		"fadeAction", "autoContinue", "volume", "fadeIn", "rate", "balance", "mute",
		"fit_mode", "rotation", "flip",
		"schedule_enabled", "schedule_days", "schedule_time_ms",
	} {
		val, ok := fields[col]
		if !ok {
			continue
		}
		setVal, err := parseCueColumn(col, val)
		if err != nil {
			return fmt.Errorf("%s: %w", col, err)
		}
		cols = append(cols, col+" = ?")
		args = append(args, setVal)
	}
	if len(cols) == 0 {
		return nil
	}
	_, err = db.Exec(`
		UPDATE cuesheet
		SET `+strings.Join(cols, ", ")+`
		WHERE cuePos = ?;`, append(args, cuePosInt)...)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// SetCueSchedule persists a cue's recurring schedule. The
// scheduler fires enabled cues whose day-of-week bit is set in
// ScheduleDays and whose ScheduleTimeMs (ms since midnight) matches
// the wall clock to the second. Day: 1=Mon .. 7=Sun; timeSec: seconds
// since midnight (0..86399) — second precision is what multi-node
// sync-fire needs, minute precision can't hit 14:31:00.
func SetCueSchedule(cuePos int, enabled bool, day int, timeSec int) error {
	// Day: 1=Mon .. 7=Sun. Guard before the shift: day=0 would be 1<<-1
	// (panic) and day=8 would set a bit no reader ever matches.
	if day < 1 || day > 7 || timeSec < 0 || timeSec >= 24*3600 {
		return errors.New("invalid schedule day or time")
	}
	if !enabled {
		// Keep the stored day/time on disable (inspector path behaviour):
		// re-enabling must resume from the same trigger, not a wiped one.
		_, err := db.Exec(`UPDATE cuesheet SET schedule_enabled = 0 WHERE cuePos = ?;`, cuePos)
		if err != nil {
			return err
		}
		bumpCuesheetVersion()
		return nil
	}
	_, err := db.Exec(`
		UPDATE cuesheet
		SET schedule_enabled = ?, schedule_days = ?, schedule_time_ms = ?
		WHERE cuePos = ?;`,
		boolToInt(enabled), 1<<(day-1), timeSec*1000, cuePos)
	if err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ScheduleInfo holds one row of the schedule query for the
// scheduler; it carries only the fields the scheduler needs.
type ScheduleInfo struct {
	CuePos       int     `db:"cuePos"`
	Title        string  `db:"title"`
	Filename     string  `db:"filename"`
	PosStart     int     `db:"posStart"`
	PosEnd       int     `db:"posEnd"`
	Hold         bool    `db:"hold"`
	Loop         bool    `db:"loop"`
	LoopCount    int     `db:"loop_count"`
	Volume       float64 `db:"volume"`
	LoudnessGain float64 `db:"loudness_gain"`
	Rate         float64 `db:"rate"`
	Balance      float64 `db:"balance"`
	Mute         bool    `db:"mute"`
	FadeIn       int     `db:"fadeIn"`
	FadeCurve    string  `db:"fade_curve"`
	FitMode      string  `db:"fit_mode"`
	Rotation     int     `db:"rotation"`
	Flip         string  `db:"flip"`
	Mimetype     string  `db:"mimetype"`
}

// NextSchedule reports the next upcoming enabled schedule strictly after
// now (any weekday): how far away it is plus its cue number/title. ok=false
// when nothing is scheduled. The GO button flashes on sub-minute horizons.
func NextSchedule(now time.Time) (dueIn time.Duration, num, title string, ok bool) {
	var rows []struct {
		Days   int    `db:"schedule_days"`
		TimeMs int    `db:"schedule_time_ms"`
		CueNum string `db:"cueNum"`
		Title  string `db:"title"`
	}
	if err := db.Select(&rows, `SELECT schedule_days, schedule_time_ms, cueNum, title
		FROM cuesheet WHERE schedule_enabled = 1`); err != nil || len(rows) == 0 {
		return 0, "", "", false
	}
	todayBit := (int(now.Weekday()) + 6) % 7 // Mon=0 .. Sun=6
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	best := time.Duration(1<<62)
	for _, r := range rows {
		for d := 0; d < 7; d++ {
			if r.Days&(1<<((todayBit+d)%7)) == 0 {
				continue
			}
			at := midnight.Add(time.Duration(d)*24*time.Hour + time.Duration(r.TimeMs)*time.Millisecond)
			if !at.After(now) {
				continue
			}
			if wait := at.Sub(now); wait < best {
				best, num, title, ok = wait, r.CueNum, r.Title, true
			}
			break // earliest matching weekday for this cue
		}
	}
	return best, num, title, ok
}

// GetScheduledCues returns every enabled schedule whose day-of-week bit is
// set and whose time-of-day fell due within the last second. The 1s window
// covers one missed scheduler tick plus jitter — anything older is stale,
// never "due" (enabling Show mode late in the day must not fire the whole
// day's past cues). Second precision throughout: minute-rounded times can
// never hit an exact-second sync-fire. The caller owns the result.
func GetScheduledCues(now time.Time) ([]ScheduleInfo, error) {
	day := int(now.Weekday())
	if day == 0 {
		day = 7 // Sunday = bit6
	}
	timeMs := now.Hour()*3600*1000 + now.Minute()*60*1000 + now.Second()*1000
	var rows []ScheduleInfo
	err := db.Select(&rows, `
		SELECT c.cuePos, c.title, m.filename, c.posStart, c.posEnd,
			c.hold, c.loop, c.loop_count, c.volume, m.loudness_gain,
			c.rate, c.balance, c.mute, c.fadeIn, c.fade_curve,
			c.fit_mode, c.rotation, c.flip,
			m.mimetype
		FROM cuesheet c
		JOIN mediapool m ON c.media_id = m.media_id
		WHERE c.schedule_enabled = 1
		AND ((c.schedule_days & ?) = ?)
		AND c.schedule_time_ms <= ?
		AND c.schedule_time_ms > ? - 1000
		AND (c.last_played_at = 0 OR c.last_played_at < ?)
		ORDER BY c.schedule_time_ms ASC, c.sheet_index ASC`,
		1<<(day-1), 1<<(day-1), timeMs, timeMs, now.UnixMilli())
	return rows, err
}
// sequence into the given order, keeping group headers at their relative
// positions. Membership re-derives from the resulting sequence — positions
// are the truth, nothing is inferred. Returns the final cue order.
func ReorderCues(order []int) ([]int, error) {
	if len(order) == 0 {
		return nil, nil
	}
	existing := make([]int, 0, len(order))
	if err := db.Select(&existing, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return nil, err
	}
	existingSet := make(map[int]bool, len(existing))
	for _, v := range existing {
		existingSet[v] = true
	}
	seen := make(map[int]bool, len(order))
	for _, v := range order {
		if !existingSet[v] {
			return nil, fmt.Errorf("reorder: unknown cuePos %d", v)
		}
		if seen[v] {
			return nil, fmt.Errorf("reorder: duplicate cuePos %d", v)
		}
		seen[v] = true
	}
	seq, err := loadSheetSequence()
	if err != nil {
		return nil, err
	}
	// Keep header slots; replace the cue slots' order with the given one.
	oi := 0
	for i := range seq {
		if seq[i].Kind == "cue" {
			seq[i].CuePos = order[oi]
			oi++
		}
	}
	// Any cues missing from the given order keep their relative tail order.
	for _, p := range existing {
		if !seen[p] {
			seq = append(seq, SheetItem{Kind: "cue", CuePos: p})
		}
	}
	// Parents travel with their cues; then separated members leave their
	// group (a full-order replace can strand a cue outside its run).
	parents, err := storedParents()
	if err != nil {
		return nil, err
	}
	if err := applyOrder(seq, parents); err != nil {
		return nil, err
	}
	var scope []int
	for _, item := range seq {
		if item.Kind == "cue" {
			scope = append(scope, item.CuePos)
		}
	}
	if err := healSeparated(seq, scope); err != nil {
		return nil, err
	}

	// Get selected cue position BEFORE reindexing transaction
	selected, _ := SelectedCuePos()

	// Re-index cuePos to contiguous 1..N in visual order so the primary key
	// matches the operator's view. This also fixes the selection if it points
	// to a cue that moved.
	type cueRow struct {
		CuePos int `db:"cuePos"`
	}
	var cues []cueRow
	if err := db.Select(&cues, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return nil, err
	}
	tx, err := db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	oldToNew := make(map[int]int)
	// First pass: set to negative to avoid UNIQUE collisions
	for i, c := range cues {
		newPos := i + 1
		oldToNew[c.CuePos] = newPos
		if c.CuePos != newPos {
			if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = -cuePos WHERE cuePos = ?`, c.CuePos); err != nil {
				return nil, err
			}
		}
	}
	// Second pass: set to final positive values
	for i, c := range cues {
		newPos := i + 1
		if c.CuePos != newPos {
			if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = ?`, newPos, -c.CuePos); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Update selection AFTER commit: the DB is single-connection, so no
	// writes may run while the reindex tx is open.
	if selected != 0 {
		if newPos, ok := oldToNew[selected]; ok {
			_ = setSelectedCuePos(newPos)
		}
	}

	// seq still holds pre-reindex positions — map to the new ones.
	final := make([]int, 0, len(order))
	for _, item := range seq {
		if item.Kind == "cue" {
			final = append(final, oldToNew[item.CuePos])
		}
	}
	return final, nil
}

func RemoveCue(cuePos string) (err error) {
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	selected, _ := SelectedCuePos()
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete the cue
	result, err := tx.Exec(`DELETE FROM cuesheet WHERE cuePos = ?`, cuePosInt)
	if err != nil {
		log.Printf("Error deleting cue from cuesheet: %v", err)
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil
	}

	// Re-index remaining cues in visual order (by sheet_index)
	// so cuePos becomes contiguous 1..N matching what the operator sees.
	type cueRow struct {
		CuePos int `db:"cuePos"`
	}
	var remainingCues []cueRow
	if err := tx.Select(&remainingCues, `SELECT cuePos FROM cuesheet ORDER BY sheet_index, cuePos`); err != nil {
		return err
	}
	oldToNew := make(map[int]int)
	for i, c := range remainingCues {
		newPos := i + 1
		oldToNew[c.CuePos] = newPos
		if c.CuePos != newPos {
			if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = ?`, newPos, c.CuePos); err != nil {
				return err
			}
		}
	}

	// Update selection: oldToNew is computed post-delete, so it already
	// accounts for the removed row — the selection simply follows its cue.
	// (The old code subtracted 1 more when the deleted cue was visually
	// before the selection, landing one row too high.)
	newSelected := selected
	if selected == cuePosInt {
		newSelected = 0
	} else {
		newSelected = oldToNew[selected]
	}
	if newSelected != selected {
		if _, err := tx.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeySelectedCue, strconv.Itoa(newSelected)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// MoveCueUp moves the cue at cuePos up by one (swaps with the cue above).
func MoveCueUp(cuePos string) error {
	p, perr := strconv.Atoi(cuePos)
	if perr != nil {
		return perr
	}
	return MoveSheetCue(p, -1)
}

func MoveCueDown(cuePos string) error {
	p, perr := strconv.Atoi(cuePos)
	if perr != nil {
		return perr
	}
	return MoveSheetCue(p, 1)
}

// RegisterMedia inserts a newly-uploaded file (already saved to the media
// directory as filename) into the mediapool, using the given probed
// metadata. The thumbnail is left pending for the background worker.
// ponytail: the `title` param is kept for API stability but no longer
// persisted - nothing ever read media_title; restore the column write (the
// DB column stays, nullable) if a surface for it appears.
func RegisterMedia(filename string, size int64, meta media.Metadata, title string) (err error) {
	// Upsert on filename: re-uploading a file the pool already knows replaces
	// its probe metadata and re-queues background work while keeping media_id
	// (and so every cue's FK to it) stable. A plain INSERT here made any
	// re-upload of a registered filename fail with a UNIQUE constraint error,
	// breaking the operator's "replace a file" flow.
	metaJSON, merr := json.Marshal(meta.Info)
	metaInfo := ""
	if merr == nil && meta.Info != nil {
		metaInfo = string(metaJSON)
	}
	_, err = db.Exec(`
		INSERT INTO mediapool (filename, mimetype, size, duration, resolution, media_meta, thumbnail_pending, waveform_pending)
		VALUES (:filename, :mimetype, :size, :duration, :resolution, :media_meta, 1, 1)
		ON CONFLICT(filename) DO UPDATE SET
			mimetype = excluded.mimetype,
			size = excluded.size,
			duration = excluded.duration,
			resolution = excluded.resolution,
			media_meta = excluded.media_meta,
			thumbnail_pending = 1,
			loudness_gain = 0,
			waveform_pending = 1;
	`,
		sql.Named("filename", filename),
		sql.Named("mimetype", meta.Mimetype),
		sql.Named("size", size),
		sql.Named("duration", meta.Duration),
		sql.Named("resolution", meta.Resolution),
		sql.Named("media_meta", metaInfo),
	)
	if err != nil {
		log.Printf("Error registering uploaded file: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// PendingThumbnails returns media rows still awaiting background work
// (thumbnail and/or waveform generation), so a background worker can pick
// them up (including after a restart, since the pending flags are persisted
// in the DB rather than an in-memory queue).
func PendingThumbnails() (medias []Media, err error) {
	err = db.Select(&medias, `SELECT * FROM mediapool WHERE thumbnail_pending = 1 OR waveform_pending = 1`)
	if err != nil {
		log.Printf("Error listing pending media work: %v", err)
		return nil, err
	}
	return medias, nil
}

// MarkThumbnailDone clears the pending flag for a media file, e.g. after
// its thumbnail has been (re)generated.
func MarkThumbnailDone(mediaID int) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET thumbnail_pending = 0 WHERE media_id = ?;`, mediaID)
	if err != nil {
		log.Printf("Error marking thumbnail done: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// RequestThumbnailRefresh flags a media file's thumbnail for regeneration.
func RequestThumbnailRefresh(filename string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET thumbnail_pending = 1 WHERE filename = ?;`, filename)
	if err != nil {
		log.Printf("Error requesting thumbnail refresh: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

func MediaLoudnessGain(filename string) (gain float64, err error) {
	err = db.Get(&gain, `SELECT loudness_gain FROM mediapool WHERE filename = ?`, filename)
	return
}

func StoreLoudnessGain(mediaID int, gain float64) error {
	if math.IsNaN(gain) || math.IsInf(gain, 0) {
		return fmt.Errorf("invalid loudness gain")
	}
	_, err := db.Exec(`UPDATE mediapool SET loudness_gain = ? WHERE media_id = ?`, gain, mediaID)
	return err
}

// StoreWaveform persists the computed amplitude peaks (JSON) for a media
// file and clears its waveform-pending flag.
func StoreWaveform(mediaID int, peaksJSON string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET waveform = ?, waveform_pending = 0 WHERE media_id = ?;`, peaksJSON, mediaID)
	if err != nil {
		log.Printf("Error storing waveform: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// FailWaveform gives up on a waveform that cannot be computed (undecodable
// file, missing codec): clears the pending flag so the worker stops
// retrying it every poll tick, leaving the timeline empty. The Analyse
// button can retry on demand via RequestWaveformAnalysis.
func FailWaveform(mediaID int) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET waveform_pending = 0 WHERE media_id = ?;`, mediaID)
	if err != nil {
		log.Printf("Error clearing waveform flag: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// RequestWaveformAnalysis flags a media file for (re)analysis by the
// background worker. "Analyse" regenerates the amplitude peaks used by the
// Cue Inspector's trim timeline.
func RequestWaveformAnalysis(filename string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET waveform_pending = 1 WHERE filename = ?;`, filename)
	if err != nil {
		log.Printf("Error requesting waveform analysis: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

func Delete(filename string) (err error) {
	res, err := db.Exec(`
		DELETE FROM mediapool
		WHERE filename = :filename;
	`, sql.Named("filename", filename))
	if err != nil {
		log.Printf("Error deleting cue from mediapool: %v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("media %q not found in pool", filename)
	}
	bumpMediaVersion()
	bumpCuesheetVersion()
	return nil
}

func ClearCueSheet() (err error) {
	_, err = db.Exec(`DELETE FROM cuesheet;`)
	if err != nil {
		log.Printf("Error clearing cuesheet: %v", err)
		return err
	}
	// The multi-selection dies with the sheet: stale positions would poison
	// a later bulk/extend action.
	_, _ = db.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedSet)
	// Groups are part of the sheet: a clear (or overwrite-import) must not
	// leave empty folders behind referencing nothing.
	if _, err = db.Exec(`DELETE FROM cue_group;`); err != nil {
		log.Printf("Error clearing cue groups: %v", err)
		return err
	}
	if _, err = db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeySelectedCue, "0"); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// UpdateMediaMeta persists refreshed detailed media info (JSON MediaInfo)
// for an already-registered media file.
func UpdateMediaMeta(filename string, metaJSON string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET media_meta = ? WHERE filename = ?;`, metaJSON, filename)
	if err != nil {
		return err
	}
	bumpMediaVersion()
	return nil
}
