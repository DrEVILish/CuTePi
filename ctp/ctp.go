package ctp

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Codec            string    `db:"codec"`
	MediaTitle       string    `db:"media_title"`
	ThumbnailPending bool      `db:"thumbnail_pending"`
	Waveform         string    `db:"waveform"` // JSON array of amplitude peaks (0..1), "" if unanalysed
	WaveformPending  bool      `db:"waveform_pending"`
	DateAdded        time.Time `db:"date_added"`
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
	Color          string  `db:"color"`
	Parent         int     `db:"parent"`
	FadeOut        int     `db:"fadeOut"` // ms; fade & stop other cues over this time
	FadeAction     string  `db:"fadeAction"`
	AutoFollow     bool    `db:"autoFollow"`
	Volume         float64 `db:"volume"` // per-cue master gain in dB; 0 = 0dB
	PreWaitFmt     string
	CueDurationFmt string
	PostWaitFmt    string
	MediaType      string // "video" | "audio" | "image" | "other", for the row icon
	Selected       bool
	Playing        bool // true if this cue is the currently playing file
	PlayPos        int  // ms into the playing clip (progress bar) when Playing
	PlayDur        int  // ms total duration of the playing clip
}

type Cuesheet struct {
	Cues []Cue
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
	if cue.PosEnd > cue.PosStart {
		return cue.PosEnd - cue.PosStart
	}
	if cue.CueDuration > 0 {
		return cue.CueDuration
	}
	if cue.Duration > 0 {
		return int(cue.Duration*1000 + 0.5)
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
	var next int
	err = db.Get(&next, `SELECT cuePos FROM cuesheet WHERE cuePos > ? ORDER BY cuePos ASC LIMIT 1;`, cur)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.Printf("Error getting the next cue position: %v", err)
		return err
	}
	return setSelectedCuePos(next)
}

// AutoFollowSelect advances the selection to the next existing cue after the
// cue that just finished (position endingPos), but only if that cue's
// autoFollow flag is set. It's the app-layer side of the AutoFollow feature,
// called from the gsp end-of-cue hook. If no later cue exists the selection
// is left as-is.
func AutoFollowSelect(endingPos int) {
	cue, err := GetCue(strconv.Itoa(endingPos))
	if err != nil || !cue.AutoFollow {
		return
	}
	var next int
	err = db.Get(&next, `SELECT cuePos FROM cuesheet WHERE cuePos > ? ORDER BY cuePos ASC LIMIT 1;`, endingPos)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("Error getting next cue for autofollow: %v", err)
		return
	}
	_ = setSelectedCuePos(next)
}

// PrevCue moves the selection to the previous existing cue position before
// the currently selected one. It never moves below position 1 (0 = nothing
// selected, so there is nothing to go "previous" from).
func PrevCue() (err error) {
	cur, err := SelectedCuePos()
	if err != nil {
		return err
	}
	if cur <= 1 {
		return nil
	}
	var prev int
	err = db.Get(&prev, `SELECT cuePos FROM cuesheet WHERE cuePos < ? ORDER BY cuePos DESC LIMIT 1;`, cur)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.Printf("Error getting the previous cue position: %v", err)
		return err
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
		ORDER BY cuesheet.cuePos
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

	for i := range cues {
		if cues[i].CuePos == selected {
			cues[i].Selected = true
		}
		cues[i].PreWaitFmt = FormatTime(cues[i].PreWait)
		cues[i].CueDurationFmt = FormatTime(effectiveCueDuration(cues[i]))
		cues[i].PostWaitFmt = FormatTime(cues[i].PostWait)
		cues[i].MediaType = mediaTypeFromMimetype(cues[i].Mimetype)
	}
	return Cuesheet{cues}, nil
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

func AddCue(filename string, cuePos string) (err error) {
	title := filename
	for suffix := 2; ; suffix++ {
		var exists int
		if err := db.Get(&exists, `SELECT COUNT(*) FROM cuesheet WHERE title = ?`, title); err != nil {
			return err
		}
		if exists == 0 {
			break
		}
		title = fmt.Sprintf("%s (%d)", filename, suffix)
	}
	if cuePos == "" {
		var result sql.Result
		result, err = db.Exec(`
  		INSERT INTO cuesheet (cuePos, cueNum, media_id, title)
  		SELECT
  			(SELECT COALESCE(MAX(cuePos), 0) + 1 FROM cuesheet) AS cuePos,
  			(SELECT COALESCE(MAX(cueNum), 0) + 1 FROM cuesheet) AS cueNum,
  			mp.media_id,
			:title AS title
  		FROM
  			(SELECT media_id FROM mediapool WHERE filename = :filename) AS mp;
		`, sql.Named("filename", filename), sql.Named("title", title))
		if err != nil {
			log.Printf("Error adding cue: %v", err)
			return err // Log the error instead of panicking
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			return fmt.Errorf("media %q not found", filename)
		}
		bumpCuesheetVersion()
	} else {
		cuePosInt, atoiErr := strconv.Atoi(cuePos)
		if atoiErr != nil {
			return atoiErr
		}

		tx, err := db.Beginx()
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// Bump the target position and everything after it up by one to make
		// space for the new cue (>= because cuePos names the position the new
		// cue should land on, so the cue currently sitting there must move
		// too). This can't be a single set-based UPDATE: SQLite enforces the
		// cuePos UNIQUE constraint per-row as it processes the statement, so
		// bumping e.g. position 1 to 2 while position 2 is still occupied (not
		// yet bumped to 3) fails with a transient UNIQUE violation depending on
		// internal row order. Updating highest-position-first, one row at a
		// time, guarantees each target slot is vacated before it's claimed.
		var positions []int
		err = tx.Select(&positions, `SELECT cuePos FROM cuesheet WHERE cuePos >= ? ORDER BY cuePos DESC`, cuePosInt)
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

		// insert new cue at the new cuePos position
		var result sql.Result
		result, err = tx.Exec(`
  		INSERT INTO cuesheet (cuePos, cueNum, media_id, title)
  		SELECT
  			:cuePos AS cuePos,
  			(SELECT COALESCE(MAX(cueNum), 0) + 1 FROM cuesheet) AS cueNum,
  			mp.media_id,
			:title AS title
  		FROM
  			(SELECT media_id FROM mediapool WHERE filename = :filename) AS mp;
	`, sql.Named("cuePos", cuePosInt), sql.Named("filename", filename), sql.Named("title", title))
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
	bumpCuesheetVersion()
	return nil
}

// editableCueColumns allow-lists which cuesheet columns may be updated via
// UpdateCue, since column names cannot be parameterized as bind values and
// col otherwise comes straight from a URL path segment.
var editableCueColumns = map[string]bool{
	"cueNum":      true,
	"title":       true,
	"posStart":    true,
	"posEnd":      true,
	"preWait":     true,
	"cueDuration": true,
	"postWait":    true,
	"hold":        true,
	"loop":        true,
	"color":       true,
	"parent":      true,
	"fadeOut":     true,
	"fadeAction":  true,
	"autoFollow":  true,
	"volume":      true,
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
	case "color":
		return cue.Color, nil
	case "parent":
		return strconv.Itoa(cue.Parent), nil
	case "fadeOut":
		return FormatTime(cue.FadeOut), nil
	case "fadeAction":
		return cue.FadeAction, nil
	case "autoFollow":
		return strconv.FormatBool(cue.AutoFollow), nil
	case "volume":
		return strconv.FormatFloat(cue.Volume, 'f', -1, 64), nil
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
	var setVal string
	switch col {
	case "posStart", "posEnd", "preWait", "cueDuration", "postWait", "fadeOut":
		ms, perr := ParseTime(val)
		if perr != nil {
			return fmt.Errorf("invalid time value for %s: %w", col, perr)
		}
		setVal = strconv.Itoa(ms)
	case "hold", "loop", "autoFollow":
		b, berr := parseBool(val)
		if berr != nil {
			return fmt.Errorf("invalid %s value: %w", col, berr)
		}
		if b {
			setVal = "1"
		} else {
			setVal = "0"
		}
	case "fadeAction":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "peers", "list", "all":
			setVal = strings.ToLower(strings.TrimSpace(val))
		default:
			return fmt.Errorf("invalid fadeAction %q (want peers, list or all)", val)
		}
	case "parent":
		if _, perr := strconv.Atoi(val); perr != nil {
			return fmt.Errorf("invalid parent %q (want an integer)", val)
		}
		setVal = val
	case "color":
		c := strings.TrimSpace(val)
		if c != "" && !strings.HasPrefix(c, "#") {
			return fmt.Errorf("invalid color %q (want a #rrggbb hex value)", val)
		}
		setVal = c
	case "volume":
		// Per-cue master gain in dB; 0 = 0dB. Clamped to the slider's range.
		f, perr := strconv.ParseFloat(val, 64)
		if perr != nil || math.IsNaN(f) {
			return fmt.Errorf("invalid volume %q (want a dB value)", val)
		}
		if f < -60 {
			f = -60
		}
		if f > 12 {
			f = 12
		}
		setVal = strconv.FormatFloat(f, 'f', -1, 64)
	default:
		setVal = val
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

// ReorderCues reindexes the cuesheet into the order given by the caller's
// ordered list of cuePos values. The client sends the full ordered list
// (e.g. after a drag-and-drop) and the server reindexes cuePos to 1..N in
// one transaction, using a +1000 offset to avoid the UNIQUE constraint
// during the reassignment. The persisted selection (if any) is remapped to
// the moved cue's new position so the highlight follows the row.
func ReorderCues(order []int) error {
	if len(order) == 0 {
		return nil
	}
	length, err := cuesheetLength()
	if err != nil {
		return err
	}
	if len(order) != length {
		return fmt.Errorf("reorder: expected %d positions, got %d", length, len(order))
	}
	// Validate that the order contains exactly the existing positions with no
	// duplicates and no missing values.
	existing := make([]int, 0, length)
	if err := db.Select(&existing, `SELECT cuePos FROM cuesheet ORDER BY cuePos;`); err != nil {
		return err
	}
	existingSet := make(map[int]bool, length)
	for _, v := range existing {
		existingSet[v] = true
	}
	seen := make(map[int]bool, length)
	for _, v := range order {
		if !existingSet[v] {
			return fmt.Errorf("reorder: unknown cuePos %d", v)
		}
		if seen[v] {
			return fmt.Errorf("reorder: duplicate cuePos %d", v)
		}
		seen[v] = true
	}
	// Remember the currently selected position so it can be remapped after the
	// reindex (selection is a cuePos value, not a logical identity).
	selected, _ := SelectedCuePos()
	selectedNew := 0
	for idx, oldPos := range order {
		if oldPos == selected {
			selectedNew = idx + 1
			break
		}
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const offset = 1000
	// Move every row to a temporary high range to free the 1..N slots.
	for _, oldPos := range order {
		if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = cuePos + ? WHERE cuePos = ?;`, offset, oldPos); err != nil {
			return err
		}
	}
	// Assign the new compact positions 1..N.
	for idx, oldPos := range order {
		newPos := idx + 1
		tmpPos := oldPos + offset
		if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = ?;`, newPos, tmpPos); err != nil {
			return err
		}
	}
	if selectedNew != 0 {
		if _, err := tx.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeySelectedCue, strconv.Itoa(selectedNew)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
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
	result, err := tx.Exec(`DELETE FROM cuesheet WHERE cuePos = ?`, cuePosInt)
	if err != nil {
		log.Printf("Error deleting cue from cuesheet: %v", err)
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil
	}

	// Compact the internal playback order without colliding with the UNIQUE
	// cuePos constraint while SQLite updates individual rows.
	if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = -cuePos WHERE cuePos > ?`, cuePosInt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE cuesheet SET cuePos = -cuePos - 1 WHERE cuePos < 0`); err != nil {
		return err
	}
	newSelected := selected
	if selected == cuePosInt {
		newSelected = 0
	} else if selected > cuePosInt {
		newSelected--
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

// moveCue swaps the cue at cuePos with the cue at neighbor, adjusting the
// persisted selection if it references either position. Both directions use
// a temporary negative value to avoid the cuePos UNIQUE constraint during
// the swap.
func moveCue(cuePosInt, neighbor int) error {
	selected, _ := SelectedCuePos()
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Use a temporary negative value to avoid UNIQUE constraint violation
	// during the swap.
	if _, err = tx.Exec(`UPDATE cuesheet SET cuePos = -1 WHERE cuePos = ?;`, neighbor); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = ?;`, neighbor, cuePosInt); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE cuesheet SET cuePos = ? WHERE cuePos = -1;`, cuePosInt); err != nil {
		return err
	}
	if selected == cuePosInt {
		if _, err = tx.Exec(`UPDATE state SET value = ? WHERE key = ?`, strconv.Itoa(neighbor), stateKeySelectedCue); err != nil {
			return err
		}
	} else if selected == neighbor {
		if _, err = tx.Exec(`UPDATE state SET value = ? WHERE key = ?`, strconv.Itoa(cuePosInt), stateKeySelectedCue); err != nil {
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
	pos, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	if pos <= 1 {
		return nil // Already at top
	}
	return moveCue(pos, pos-1)
}

// MoveCueDown moves the cue at cuePos down by one (swaps with the cue below).
func MoveCueDown(cuePos string) error {
	pos, err := strconv.Atoi(cuePos)
	if err != nil {
		return err
	}
	length, err := cuesheetLength()
	if err != nil {
		return err
	}
	if pos >= length {
		return nil // Already at bottom
	}
	return moveCue(pos, pos+1)
}

// RegisterMedia inserts a newly-uploaded file (already saved to the media
// directory as filename) into the mediapool, using the given probed
// metadata. The thumbnail is left pending for the background worker.
func RegisterMedia(filename string, size int64, meta media.Metadata, title string) (err error) {
	_, err = db.Exec(`
		INSERT INTO mediapool (filename, mimetype, size, duration, resolution, codec, media_title, thumbnail_pending, waveform_pending)
		VALUES (:filename, :mimetype, :size, :duration, :resolution, :codec, :media_title, 1, 1);
	`,
		sql.Named("filename", filename),
		sql.Named("mimetype", meta.Mimetype),
		sql.Named("size", size),
		sql.Named("duration", meta.Duration),
		sql.Named("resolution", meta.Resolution),
		sql.Named("codec", meta.Codec),
		sql.Named("media_title", title),
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
	_, err = db.Exec(`
		DELETE FROM mediapool
		WHERE filename = :filename;
	`, sql.Named("filename", filename))
	if err != nil {
		log.Printf("Error deleting cue from mediapool: %v", err)
		return err
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
	if _, err = db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeySelectedCue, "0"); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}
