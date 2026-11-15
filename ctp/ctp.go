package ctp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/ws"
)

type Media struct {
	Media_id         int       `db:"media_id"`
	Filename         string    `db:"filename"`
	SourceKind       string    `db:"source_kind"` // "file" or "endpoint"
	EndpointURL      string    `db:"endpoint_url"`
	EndpointTitle    string    `db:"endpoint_title"`
	Mimetype         string    `db:"mimetype"`
	Size             int       `db:"size"`
	Duration         float64   `db:"duration"`
	Resolution       string    `db:"resolution"`
	ThumbnailPending bool      `db:"thumbnail_pending"`
	Waveform         string    `db:"waveform"`         // JSON array of amplitude peaks (0..1), "" if unanalysed (media_waveform; AnalysedWaveforms and WaveformPeaks only)
	WaveformVersion  int64     `db:"waveform_version"` // GetCue: when the stored peaks last changed (unix ms), 0 if none
	MediaInfo        string    `db:"media_meta"`       // JSON MediaInfo (container/codec detail), "" on old imports
	WaveformPending  bool      `db:"waveform_pending"`
	Missing          bool      `db:"missing"` // source file absent from disk (startup scan)
	DateAdded        time.Time `db:"date_added"`
	LoudnessGain     float64   `db:"loudness_gain"`
}

type Cue struct {
	Media
	Cue_id          int     `db:"cue_id"`
	CuePos          int     `db:"cuePos"`
	CueNum          string  `db:"cueNum"`
	Media_id        int     `db:"media_id"`
	Title           string  `db:"title"`
	PosStart        int     `db:"posStart"`
	PosEnd          int     `db:"posEnd"`
	PreWait         int     `db:"preWait"`
	CueDuration     int     `db:"cueDuration"`
	PostWait        int     `db:"postWait"`
	Hold            bool    `db:"hold"`
	Loop            bool    `db:"loop"`
	LoopCount       int     `db:"loop_count"` // 0 = infinite, N = play N times (when loop is on)
	Color           string  `db:"color"`
	Parent          int     `db:"parent"`
	FadeOut         int     `db:"fadeOut"` // ms; fade & stop other cues over this time
	FadeAction      string  `db:"fadeAction"`
	AutoContinue    bool    `db:"autoContinue"`
	Volume          float64 `db:"volume"` // per-cue master gain in dB; 0 = 0dB
	FadeIn          int     `db:"fadeIn"` // ms; audio and video fade from silence/black
	Rate            float64 `db:"rate"`
	Balance         float64 `db:"balance"`
	Mute            bool    `db:"mute"`
	LastResult      int     `db:"last_result"`    // 0 never played, 1 ok, 2 error (§12.3)
	LastPlayedAt    int64   `db:"last_played_at"` // unix ms of the last fire
	SheetIndex      float64 `db:"sheet_index"`    // visual+playback order (§4)
	FadeCurve       string  `db:"fade_curve"`     // linear|smooth|log|exp (§12.7)
	FitMode         string  `db:"fit_mode"`       // fit|stretch: image/video frame fitting (§5.5)
	Opacity         float64 `db:"opacity"`        // 0..100 %: picture opacity on the wall (§5.5)
	GeomX           string  `db:"geom_x"`         // picture box: "" (fill), pixels, or "N%" of the display
	GeomY           string  `db:"geom_y"`
	GeomW           string  `db:"geom_w"`
	GeomH           string  `db:"geom_h"`
	CropL           string  `db:"crop_l"` // crop per source edge: "", pixels, or "N%"
	CropR           string  `db:"crop_r"`
	CropT           string  `db:"crop_t"`
	CropB           string  `db:"crop_b"`
	Align           string  `db:"align"`            // "[left|center|right] [top|middle|bottom]"; "" = box at X/Y, picture centred
	StopOthers      bool    `db:"stop_others"`      // firing stops the running cues (§6.1.2)
	Layer           string  `db:"layer"`            // top|bottom|under: its place when it keeps them
	LayerUnder      int     `db:"layer_under"`      // under: the cue_id it goes beneath
	LayerUnderNum   string  `db:"layer_under_num"`  // that cue's number ("" if none)
	Rotation        int     `db:"rotation"`         // 0|90|180|270 clockwise degrees (§5.5)
	Flip            string  `db:"flip"`             // none|h|v: mirror horizontal/vertical (§5.5)
	ScheduleEnabled bool    `db:"schedule_enabled"` // whether scheduling is enabled for this cue
	ScheduleDays    int     `db:"schedule_days"`    // bitmask: bit0=Mon, bit1=Tue, ..., bit6=Sun
	ScheduleTimeMs  int     `db:"schedule_time_ms"` // time of day in milliseconds since 00:00:00
	PreWaitFmt      string
	CueDurationFmt  string
	PostWaitFmt     string
	MediaType       string // "video" | "audio" | "image" | "other", for the row icon
	Selected        bool
	Playing         bool   // true if this cue is the currently playing file
	Running         bool   // running on any layer (§6.1.2), the focus included
	PlayPos         int    // ms into the playing clip (progress bar) when Playing
	PlayDur         int    // ms total duration of the playing clip
	PlayPaused      bool   // a running (non-focus) cue that is paused or parked on its last frame
	InSelection     bool   // member of the multi-selection (§12.4); anchor uses Selected
	WaitKind        string // "pre"|"post" while a chain wait counts down on this cue (§12.2)
	WaitLeftS       int    // whole seconds left in that wait (render-time)
	WaitPct         int    // 0..100 through the current wait phase (render-time)
}

// cueColumns lists fields represented by Cue explicitly. Avoid cuesheet.*
// because databases can carry newer optional columns that this binary does
// not map. Every column InitDB guarantees must be listed: opacity, geometry
// and crop were missing until 2026-10-07, so every cue loaded through
// GetCue played full-screen, opaque and uncropped whatever its settings.
const cueColumns = `cuesheet.cue_id, cuesheet.cuePos, cuesheet.cueNum, cuesheet.media_id,
	cuesheet.title, cuesheet.posStart, cuesheet.posEnd, cuesheet.preWait, cuesheet.cueDuration,
	cuesheet.postWait, cuesheet.hold, cuesheet.loop, cuesheet.loop_count, cuesheet.color,
	cuesheet.parent, cuesheet.fadeOut, cuesheet.fadeAction, cuesheet.autoContinue, cuesheet.volume,
	cuesheet.fadeIn, cuesheet.rate, cuesheet.balance, cuesheet.mute, cuesheet.last_result,
	cuesheet.last_played_at, cuesheet.sheet_index, cuesheet.fade_curve, cuesheet.fit_mode,
	cuesheet.rotation, cuesheet.flip, cuesheet.schedule_enabled, cuesheet.schedule_days,
	cuesheet.schedule_time_ms, cuesheet.opacity, cuesheet.geom_x, cuesheet.geom_y,
	cuesheet.geom_w, cuesheet.geom_h, cuesheet.crop_l, cuesheet.crop_r, cuesheet.crop_t,
	cuesheet.crop_b, cuesheet.align, cuesheet.stop_others, cuesheet.layer, cuesheet.layer_under,
	COALESCE((SELECT u.cueNum FROM cuesheet u WHERE u.cue_id = cuesheet.layer_under), '') AS layer_under_num`

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

// GetCueByID fetches a cue by its stable cue_id (unlike cuePos, unchanged
// by reordering), with the same derived fields as GetCue.
func GetCueByID(id int) (Cue, error) {
	var pos int
	if err := db.Get(&pos, `SELECT cuePos FROM cuesheet WHERE cue_id = ?`, id); err != nil {
		return Cue{}, err
	}
	return GetCue(strconv.Itoa(pos))
}

func GetCue(cuePos string) (cue Cue, err error) {
	query := `
		SELECT ` + cueColumns + `, COALESCE(mediapool.filename, '') AS filename,
			mediapool.source_kind, mediapool.endpoint_url, mediapool.endpoint_title,
			mediapool.mimetype, mediapool.size, mediapool.duration, mediapool.resolution,
			mediapool.thumbnail_pending, mediapool.media_meta,
			mediapool.waveform_pending, mediapool.missing, mediapool.date_added, mediapool.loudness_gain,
			COALESCE((SELECT updated FROM media_waveform w WHERE w.media_id = cuesheet.media_id AND w.peaks <> ''), 0) AS waveform_version
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		WHERE cuePos = :cuePos
	`
	err = db.Get(&cue, query, sql.Named("cuePos", cuePos))
	if err != nil {
		// No such cue is an expected answer (a group or nothing is selected);
		// callers handle it, so only real database faults are logged.
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Error Getting Cue: %v", err)
		}
		return Cue{}, err
	}
	cue.PreWaitFmt = FormatTime(cue.PreWait)
	cue.CueDurationFmt = FormatTime(EffectiveCueDuration(cue))
	cue.PostWaitFmt = FormatTime(cue.PostWait)
	cue.MediaType = mediaTypeFromMimetype(cue.Mimetype)
	return cue, nil // Return the found Cue
}

// mediaTypeFromMimetype maps a media mimetype to the short kind string used
// for the cue row's media-type icon and the fade/stop logic.
func mediaTypeFromMimetype(mimetype string) string {
	switch {
	case mimetype == "application/x-cutepi-endpoint":
		return "endpoint"
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

// EffectiveCueDuration returns the duration shown to the operator. A valid
// trim window takes precedence; otherwise use an explicitly stored duration,
// then the probed media duration. New cues leave cueDuration at zero, so they
// still display the real media length instead of 00:00:00.000.
func EffectiveCueDuration(cue Cue) int {
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

func GetCuesheet() (cuesheet Cuesheet, err error) {
	var cues []Cue
	// No waveform: a long clip's peaks are hundreds of KB, and every render
	// of the sheet, Now Playing and the selection read the whole sheet.
	// Only the inspector shows one (GetCue).

	query := `
		SELECT ` + cueColumns + `, COALESCE(mediapool.filename, '') AS filename,
			mediapool.source_kind, mediapool.endpoint_url, mediapool.endpoint_title,
			mediapool.mimetype, mediapool.size, mediapool.duration, mediapool.resolution,
			mediapool.thumbnail_pending, mediapool.media_meta,
			mediapool.waveform_pending, mediapool.missing, mediapool.date_added, mediapool.loudness_gain
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
		cues[i].CueDurationFmt = FormatTime(EffectiveCueDuration(cues[i]))
		cues[i].PostWaitFmt = FormatTime(cues[i].PostWait)
		cues[i].MediaType = mediaTypeFromMimetype(cues[i].Mimetype)
	}
	groups, _ := Groups()
	return Cuesheet{Cues: cues, Groups: groups}, nil
}

func GetMediapool() (pool Mediapool, err error) {
	var medias []Media
	// media_id DESC breaks ties: CURRENT_TIMESTAMP only has 1-second
	// resolution, so a multi-file upload can easily register several rows
	// with an identical date_added, which would otherwise leave their
	// relative order (newest-first, per spec) unspecified.
	query := `SELECT media_id, COALESCE(filename,'') AS filename, source_kind, endpoint_url, endpoint_title,
		mimetype, size, duration, resolution, thumbnail_pending, media_meta,
		waveform_pending, missing, date_added, loudness_gain FROM mediapool
		WHERE source_kind = 'file' ORDER BY date_added DESC, media_id DESC`
	err = db.Select(&medias, query)
	if err != nil {
		log.Printf("Error GetMediapool: %v", err)
		return Mediapool{}, err
	}
	return Mediapool{medias}, nil
}

// AnalysedWaveforms lists the media files with a stored waveform, with it
// (GetMediapool leaves waveforms out).
func AnalysedWaveforms() (medias []Media, err error) {
	err = db.Select(&medias, `SELECT m.media_id, m.filename, m.duration, w.peaks AS waveform
		FROM mediapool m JOIN media_waveform w ON w.media_id = m.media_id
		WHERE m.source_kind = 'file' AND w.peaks <> ''`)
	return medias, err
}

// WaveformPeaks is a media file's stored peaks (JSON), "" if none.
func WaveformPeaks(filename string) (string, error) {
	var peaks string
	err := db.Get(&peaks, `SELECT w.peaks FROM media_waveform w JOIN mediapool m ON m.media_id = w.media_id
		WHERE m.filename = ?`, filename)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return peaks, err
}

// AddEndpointCue inserts an endpoint into the sheet using the normal cue
// numbering and ordering rules. It intentionally skips file-only semantics.
func AddEndpointCue(mediaID int, cuePos string, groupID int, first bool) error {
	var title, kind string
	if err := db.QueryRow(`SELECT endpoint_title, source_kind FROM mediapool WHERE media_id = ?`, mediaID).Scan(&title, &kind); err != nil {
		return err
	}
	if kind != "endpoint" {
		return fmt.Errorf("media %d is not an endpoint", mediaID)
	}
	title, err := uniqueCueField("title", title)
	if err != nil {
		return err
	}
	if groupID != 0 {
		var header float64
		if err := db.Get(&header, `SELECT sheet_index FROM cue_group WHERE group_id = ?`, groupID); err != nil {
			return err
		}
		if err := ExpandGroup(groupID); err != nil {
			return err
		}
		var next int
		if err := db.Get(&next, `SELECT COALESCE(MAX(cuePos),0)+1 FROM cuesheet`); err != nil {
			return err
		}
		seat := header + 1
		if !first {
			var last sql.NullFloat64
			_ = db.Get(&last, `SELECT MAX(sheet_index) FROM cuesheet WHERE parent = ?`, groupID)
			if last.Valid {
				seat = last.Float64 + 1
			}
		}
		return insertCueForMediaID(mediaID, title, next, seat, groupID)
	}
	next := 0
	if cuePos == "" {
		if err := db.Get(&next, `SELECT COALESCE(MAX(cuePos),0)+1 FROM cuesheet`); err != nil {
			return err
		}
	} else if n, err := strconv.Atoi(strings.Trim(cuePos, "/")); err == nil {
		next = n
	} else {
		return err
	}
	idx := 0.0
	parent := 0
	if cuePos == "" {
		if err := db.Get(&idx, `SELECT COALESCE(MAX(sheet_index),0)+1000 FROM cuesheet`); err != nil {
			return err
		}
	} else {
		seq, err := loadSheetSequence()
		if err != nil {
			return err
		}
		at := len(seq)
		for i, it := range seq {
			if it.Kind == "cue" && it.CuePos == next {
				at = i
				break
			}
		}
		if at > 0 && at < len(seq) && seq[at].Kind == "cue" && seq[at-1].Kind == "group" {
			var cueParent int
			if err := db.Get(&cueParent, `SELECT parent FROM cuesheet WHERE cuePos = ?`, next); err == nil && cueParent == seq[at-1].GroupID {
				at--
			}
		}
		idx, err = gapSheetIndex(seq, at)
		if err != nil {
			return err
		}
		withNew := append(append(append([]SheetItem{}, seq[:at]...), SheetItem{Kind: "cue", CuePos: next}), seq[at:]...)
		parent, err = gapOwner(withNew, at)
		if err != nil {
			return err
		}
	}
	return insertCueForMediaID(mediaID, title, next, idx, parent)
}

func insertCueForMediaID(mediaID int, title string, pos int, sheetIndex float64, parent int) error {
	// Read settings BEFORE the tx opens: SQLite runs on one connection, so a
	// pool query while the tx holds it deadlocks the whole server.
	autoNumber := GetAutoNumber()
	numStep := GetCueNumStep()
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	positions := []int{}
	if err := tx.Select(&positions, `SELECT cuePos FROM cuesheet WHERE cuePos >= ? ORDER BY cuePos DESC`, pos); err != nil {
		return err
	}
	for _, p := range positions {
		if _, err := tx.Exec(`UPDATE cuesheet SET cuePos=cuePos+1 WHERE cuePos=?`, p); err != nil {
			return err
		}
	}
	num, err := nextCueNum(tx, sheetIndex, autoNumber, numStep)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO cuesheet (cuePos,cueNum,media_id,title,hold,loop,loop_count,sheet_index,parent,autoContinue)
		VALUES (?,?,?,?,0,0,0,?,?,0)`, pos, num, mediaID, title, sheetIndex, parent); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// MarkMissingFiles flags mediapool rows whose source file is no longer on
// disk. Runs once at startup (see main.go): no periodic scan. A file that is
// only temporarily absent is re-flagged as present on the next startup.
func MarkMissingFiles() {
	var filenames []string
	if err := db.Select(&filenames, `SELECT filename FROM mediapool WHERE source_kind = 'file' AND filename IS NOT NULL`); err != nil {
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
		if !ValidColor(value) {
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
	// The anchor cue's current group owns the new folder: grouping cues
	// that sit inside a group nests the new folder there, at the anchor's
	// slot (§12.4 "add to new group at the current position").
	nest := 0
	if atCue > 0 {
		_ = db.Get(&nest, `SELECT parent FROM cuesheet WHERE cuePos = ?`, atCue)
	}
	if err := ValidateGroupParent(0, nest); err != nil {
		return 0, err
	}
	id, err := CreateGroup("New Group", nest)
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

func AddCue(filename string, cuePos string) (err error) {
	title, err := uniqueCueField("title", filename)
	if err != nil {
		return err
	}
	// Read settings BEFORE the tx opens: SQLite runs single-connection, so
	// any pool query while the tx holds the connection deadlocks.
	autoNumber := GetAutoNumber()
	numStep := GetCueNumStep()
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
	// (§12.5): auto-number 1,2,3… / numeric midpoint, or MAX+1.
	newCueNum, err := nextCueNum(tx, newSheetIndex, autoNumber, numStep)
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
	numStep := GetCueNumStep()
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
	newCueNum, err := nextCueNum(tx, seat, autoNumber, numStep)
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
	"geom_x":       true,
	"geom_y":       true,
	"geom_w":       true,
	"geom_h":       true,
	"crop_l":       true,
	"crop_r":       true,
	"crop_t":       true,
	"crop_b":       true,
	"align":        true,
}

// ErrBadCueEdit marks UpdateCue errors that are the caller's fault (a column
// that is not editable, or a value that does not validate), so the edit route
// answers 400 rather than 500.
var ErrBadCueEdit = errors.New("bad cue edit")

type badCueEdit struct{ error }

func (badCueEdit) Is(target error) bool { return target == ErrBadCueEdit }
func (e badCueEdit) Unwrap() error      { return e.error }

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
		return FormatTime(EffectiveCueDuration(cue)), nil
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
	case "geom_x":
		return cue.GeomX, nil
	case "geom_y":
		return cue.GeomY, nil
	case "geom_w":
		return cue.GeomW, nil
	case "geom_h":
		return cue.GeomH, nil
	case "crop_l":
		return cue.CropL, nil
	case "crop_r":
		return cue.CropR, nil
	case "crop_t":
		return cue.CropT, nil
	case "crop_b":
		return cue.CropB, nil
	case "align":
		return cue.Align, nil
	default:
		return "", fmt.Errorf("cue column %q is not editable", col)
	}
}

// cueDBColumn maps an editable column name to its SQL column (only
// fadeCurve differs).
func cueDBColumn(col string) string {
	if col == "fadeCurve" {
		return "fade_curve"
	}
	return col
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

// checkCueNumChange rejects a new number for the cue at cuePos that another
// cue or group already uses; re-saving the cue's own number always passes.
func checkCueNumChange(cuePos int, num string) error {
	cur, err := currentCueNum(cuePos)
	if err != nil {
		return err
	}
	if sameCueNum(cur, num) {
		return nil
	}
	return checkCueNumFree(db, num, cuePos, 0)
}

func UpdateCue(cuePos string, col string, val string) (err error) {
	if !editableCueColumns[col] {
		return badCueEdit{fmt.Errorf("cue column %q is not editable", col)}
	}
	cuePosInt, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}

	// For time columns, parse the input (hh:mm:ss.ms or bare seconds); for
	// boolean columns, normalize the common true/false spellings.
	setVal, err := parseCueColumn(col, val)
	if err != nil {
		return badCueEdit{err}
	}
	if col == "cueNum" {
		if err := checkCueNumChange(cuePosInt, setVal); err != nil {
			return err
		}
	}
	_, err = db.Exec(`
		UPDATE cuesheet
		SET `+cueDBColumn(col)+` = ?
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
		if !ValidColor(c) {
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
	case "opacity":
		f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(val), "%"), 64)
		if err != nil || f < 0 || f > 100 {
			return "", fmt.Errorf("invalid opacity %q (want 0 to 100 %%)", val)
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case "stop_others":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "1", "true", "on", "yes":
			return "1", nil
		case "", "0", "false", "off", "no":
			return "0", nil
		}
		return "", fmt.Errorf("invalid stop_others %q (want on or off)", val)
	case "layer":
		switch v := strings.ToLower(strings.TrimSpace(val)); v {
		case "top", "bottom", "under":
			return v, nil
		case "":
			return "top", nil
		}
		return "", fmt.Errorf("invalid layer %q (want top, bottom or under)", val)
	case "geom_x", "geom_y", "geom_w", "geom_h", "crop_l", "crop_r", "crop_t", "crop_b":
		v := strings.ToLower(strings.TrimSpace(val))
		if v != "" && !validGeom(v) {
			return "", fmt.Errorf("invalid %s %q (want pixels like 960, or a percentage like 50%%)", col, val)
		}
		return v, nil
	case "align":
		return normalizeAlign(val)
	case "fit_mode":
		switch v := strings.TrimSpace(val); v {
		case "fit", "stretch", "fill-width", "fill-height", "fill":
			return v, nil
		default:
			return "", fmt.Errorf("invalid fit_mode %q (want fit, stretch, fill-width, fill-height or fill)", val)
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
		"fadeCurve", "fit_mode", "rotation", "flip", "opacity", "geom_x", "geom_y", "geom_w", "geom_h", "crop_l", "crop_r", "crop_t", "crop_b", "align",
		"schedule_enabled", "schedule_days", "schedule_time_ms", "stop_others", "layer", "layer_under",
	} {
		val, ok := fields[col]
		if !ok {
			continue
		}
		if col == "layer_under" {
			// Given by cue number, stored by identity (survives reorder).
			id, err := cueIDByNum(strings.TrimSpace(val), cuePosInt)
			if err != nil {
				return fmt.Errorf("layer_under: %w", err)
			}
			cols = append(cols, "layer_under = ?")
			args = append(args, id)
			continue
		}
		setVal, err := parseCueColumn(col, val)
		if err != nil {
			return fmt.Errorf("%s: %w", col, err)
		}
		if col == "cueNum" {
			if err := checkCueNumChange(cuePosInt, setVal); err != nil {
				return err
			}
		}
		cols = append(cols, cueDBColumn(col)+" = ?")
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

// cueIDByNum resolves a cue number to its cue_id for Layer "under" ("" = none).
// A cue cannot go under itself.
func cueIDByNum(num string, selfPos int) (int, error) {
	if num == "" {
		return 0, nil
	}
	var r struct {
		ID  int `db:"cue_id"`
		Pos int `db:"cuePos"`
	}
	if err := db.Get(&r, `SELECT cue_id, cuePos FROM cuesheet WHERE cueNum = ?`, num); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("there is no cue %q", num)
		}
		return 0, err
	}
	if r.Pos == selfPos {
		return 0, fmt.Errorf("a cue cannot go under itself")
	}
	return r.ID, nil
}

// CuePosByID is the current position of the cue with this cue_id (0 if gone).
func CuePosByID(id int) int {
	if id <= 0 {
		return 0
	}
	var pos int
	if err := db.Get(&pos, `SELECT cuePos FROM cuesheet WHERE cue_id = ?`, id); err != nil {
		return 0
	}
	return pos
}

// SetCueLayerUnderPos points the cue at cuePos's "under" layer at the cue at
// underPos (show import re-links them after every cue is in).
func SetCueLayerUnderPos(cuePos, underPos int) error {
	_, err := db.Exec(`UPDATE cuesheet SET layer_under = COALESCE((SELECT cue_id FROM cuesheet WHERE cuePos = ?), 0)
		WHERE cuePos = ?`, underPos, cuePos)
	return err
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
	// Two passes (negate, then assign): after a drag reorder the visual
	// order differs from cuePos order, so a direct UPDATE can collide with a
	// row that still holds the target position (UNIQUE cuePos).
	for i, c := range remainingCues {
		newPos := i + 1
		oldToNew[c.CuePos] = newPos
		if c.CuePos != newPos {
			if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = -cuePos WHERE cuePos = ?`, c.CuePos); err != nil {
				return err
			}
		}
	}
	for i, c := range remainingCues {
		newPos := i + 1
		if c.CuePos != newPos {
			if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = ?`, newPos, -c.CuePos); err != nil {
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

// normalizeAlign validates a picture alignment: at most one horizontal word
// (left, center, right) and one vertical (top, middle, bottom), stored as
// "<h> <v>" with either part possibly absent.
func normalizeAlign(val string) (string, error) {
	var h, v string
	for _, w := range strings.Fields(strings.ToLower(val)) {
		switch w {
		case "left", "center", "right":
			if h != "" {
				return "", fmt.Errorf("invalid align %q (one of left, center, right)", val)
			}
			h = w
		case "top", "middle", "bottom":
			if v != "" {
				return "", fmt.Errorf("invalid align %q (one of top, middle, bottom)", val)
			}
			v = w
		default:
			return "", fmt.Errorf("invalid align %q (want left/center/right and top/middle/bottom)", val)
		}
	}
	return strings.TrimSpace(h + " " + v), nil
}
