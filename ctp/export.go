package ctp

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ExportCue is the JSON representation of one cue for .CTP show export /
// import. Media-time fields (waveform, thumbnail state) are not exported; the
// referenced file is carried in the ZIP instead. Group membership is exported
// once Cue Groups exist; the manifest reserves that shape via file layout.
type ExportCue struct {
	CuePos          int      `json:"cuePos,omitempty"`
	SheetIndex      *float64 `json:"sheetIndex,omitempty"`
	ScheduleEnabled bool     `json:"scheduleEnabled,omitempty"`
	ScheduleDays    int      `json:"scheduleDays,omitempty"`
	ScheduleTimeMs  int      `json:"scheduleTimeMs,omitempty"`
	LayerUnderPos   int      `json:"layerUnderPos,omitempty"`
	Filename        string   `json:"filename"`
	SourceKind      string   `json:"sourceKind,omitempty"`
	EndpointURL     string   `json:"endpointUrl,omitempty"`
	EndpointTitle   string   `json:"endpointTitle,omitempty"`
	CueNum          string   `json:"cueNum"`
	Title           string   `json:"title"`
	PosStart        int      `json:"posStart"`
	PosEnd          int      `json:"posEnd"`
	PreWait         int      `json:"preWait"`
	CueDuration     int      `json:"cueDuration"`
	PostWait        int      `json:"postWait"`
	Hold            bool     `json:"hold"`
	Loop            bool     `json:"loop"`
	LoopCount       int      `json:"loopCount"`
	AutoContinue    bool     `json:"autoContinue"`
	Color           string   `json:"color"`
	Parent          int      `json:"parent"`
	FadeOut         int      `json:"fadeOut"`
	FadeAction      string   `json:"fadeAction"`
	FadeCurve       string   `json:"fadeCurve"`
	FitMode         string   `json:"fitMode"`
	Rotation        int      `json:"rotation"`
	Flip            string   `json:"flip"`
	Opacity         *float64 `json:"opacity,omitempty"` // nil in older shows means fully opaque
	GeomX           string   `json:"geomX,omitempty"`
	GeomY           string   `json:"geomY,omitempty"`
	GeomW           string   `json:"geomW,omitempty"`
	GeomH           string   `json:"geomH,omitempty"`
	CropL           string   `json:"cropL,omitempty"`
	CropR           string   `json:"cropR,omitempty"`
	CropT           string   `json:"cropT,omitempty"`
	CropB           string   `json:"cropB,omitempty"`
	Align           string   `json:"align,omitempty"`
	StopOthers      *bool    `json:"stopOthers,omitempty"` // absent (older shows) = true
	Layer           string   `json:"layer,omitempty"`      // "" = top
	LayerUnder      string   `json:"layerUnder,omitempty"` // under: the cue number it goes beneath
	Volume          float64  `json:"volume"`
	FadeIn          int      `json:"fadeIn"`
	Rate            float64  `json:"rate"`
	Balance         float64  `json:"balance"`
	Mute            bool     `json:"mute"`
}

// ExportCues lists every cue (sheet order) as an ExportCue plus the currently
// selected cue position, for building a .CTP manifest.
func ExportCues() (cues []ExportCue, selected int, err error) {
	type row struct {
		CuePos          int     `db:"cuePos"`
		SheetIndex      float64 `db:"sheet_index"`
		ScheduleEnabled bool    `db:"schedule_enabled"`
		ScheduleDays    int     `db:"schedule_days"`
		ScheduleTimeMs  int     `db:"schedule_time_ms"`
		LayerUnderPos   int     `db:"layer_under_pos"`
		CueNum          string  `db:"cueNum"`
		Title           string  `db:"title"`
		Filename        string  `db:"filename"`
		SourceKind      string  `db:"source_kind"`
		EndpointURL     string  `db:"endpoint_url"`
		EndpointTitle   string  `db:"endpoint_title"`
		PosStart        int     `db:"posStart"`
		PosEnd          int     `db:"posEnd"`
		PreWait         int     `db:"preWait"`
		CueDuration     int     `db:"cueDuration"`
		PostWait        int     `db:"postWait"`
		Hold            bool    `db:"hold"`
		Loop            bool    `db:"loop"`
		LoopCount       int     `db:"loop_count"`
		AutoContinue    bool    `db:"autoContinue"`
		Color           string  `db:"color"`
		Parent          int     `db:"parent"`
		FadeOut         int     `db:"fadeOut"`
		FadeAction      string  `db:"fadeAction"`
		FadeCurve       string  `db:"fade_curve"`
		FitMode         string  `db:"fit_mode"`
		Rotation        int     `db:"rotation"`
		Flip            string  `db:"flip"`
		Opacity         float64 `db:"opacity"`
		GeomX           string  `db:"geom_x"`
		GeomY           string  `db:"geom_y"`
		GeomW           string  `db:"geom_w"`
		GeomH           string  `db:"geom_h"`
		CropL           string  `db:"crop_l"`
		CropR           string  `db:"crop_r"`
		CropT           string  `db:"crop_t"`
		CropB           string  `db:"crop_b"`
		Align           string  `db:"align"`
		StopOthers      bool    `db:"stop_others"`
		Layer           string  `db:"layer"`
		LayerUnder      string  `db:"layer_under_num"`
		Volume          float64 `db:"volume"`
		FadeIn          int     `db:"fadeIn"`
		Rate            float64 `db:"rate"`
		Balance         float64 `db:"balance"`
		Mute            bool    `db:"mute"`
	}
	var rows []row
	err = db.Select(&rows, `
		SELECT cuesheet.cuePos, cuesheet.sheet_index, cuesheet.schedule_enabled, cuesheet.schedule_days, cuesheet.schedule_time_ms,
 COALESCE((SELECT u.cuePos FROM cuesheet u WHERE u.cue_id = cuesheet.layer_under), 0) AS layer_under_pos,
 cuesheet.cueNum, cuesheet.title, COALESCE(mediapool.filename,'') AS filename,
			mediapool.source_kind, mediapool.endpoint_url, mediapool.endpoint_title,
			cuesheet.posStart, cuesheet.posEnd, cuesheet.preWait,
			cuesheet.cueDuration, cuesheet.postWait, cuesheet.hold,
			cuesheet.loop, cuesheet.loop_count, cuesheet.autoContinue,
			cuesheet.color, cuesheet.parent, cuesheet.fadeOut,
			cuesheet.fadeAction, cuesheet.fade_curve, cuesheet.fit_mode, cuesheet.rotation, cuesheet.flip, cuesheet.opacity, cuesheet.geom_x, cuesheet.geom_y, cuesheet.geom_w, cuesheet.geom_h, cuesheet.crop_l, cuesheet.crop_r, cuesheet.crop_t, cuesheet.crop_b, cuesheet.align, cuesheet.volume, cuesheet.fadeIn, cuesheet.rate, cuesheet.balance, cuesheet.mute,
			cuesheet.stop_others, cuesheet.layer,
			COALESCE((SELECT u.cueNum FROM cuesheet u WHERE u.cue_id = cuesheet.layer_under), '') AS layer_under_num
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		ORDER BY cuesheet.sheet_index, cuesheet.cuePos
	`)
	if err != nil {
		log.Printf("Error ExportCues: %v", err)
		return nil, 0, err
	}
	cues = make([]ExportCue, 0, len(rows))
	for _, r := range rows {
		cues = append(cues, ExportCue{
			CuePos: r.CuePos, SheetIndex: &r.SheetIndex,
			ScheduleEnabled: r.ScheduleEnabled, ScheduleDays: r.ScheduleDays, ScheduleTimeMs: r.ScheduleTimeMs, LayerUnderPos: r.LayerUnderPos,
			Filename:      r.Filename,
			SourceKind:    r.SourceKind,
			EndpointURL:   r.EndpointURL,
			EndpointTitle: r.EndpointTitle,
			CueNum:        r.CueNum,
			Title:         r.Title,
			PosStart:      r.PosStart,
			PosEnd:        r.PosEnd,
			PreWait:       r.PreWait,
			CueDuration:   r.CueDuration,
			PostWait:      r.PostWait,
			Hold:          r.Hold,
			Loop:          r.Loop,
			LoopCount:     r.LoopCount,
			AutoContinue:  r.AutoContinue,
			Color:         r.Color,
			Parent:        r.Parent,
			FadeOut:       r.FadeOut,
			FadeAction:    r.FadeAction,
			FadeCurve:     r.FadeCurve,
			FitMode:       r.FitMode,
			Opacity:       &r.Opacity,
			GeomX:         r.GeomX,
			GeomY:         r.GeomY,
			GeomW:         r.GeomW,
			GeomH:         r.GeomH,
			CropL:         r.CropL,
			CropR:         r.CropR,
			CropT:         r.CropT,
			CropB:         r.CropB,
			Align:         r.Align,
			StopOthers:    &r.StopOthers,
			Layer:         r.Layer,
			LayerUnder:    r.LayerUnder,
			Rotation:      r.Rotation,
			Flip:          r.Flip,
			Volume:        r.Volume,
			FadeIn:        r.FadeIn,
			Rate:          r.Rate,
			Balance:       r.Balance,
			Mute:          r.Mute,
		})
	}
	selected, err = SelectedCuePos()
	if err != nil {
		log.Printf("Error reading selected cue: %v", err)
	}
	return cues, selected, nil
}

// MediaRegistered reports whether filename is already in the media pool.
func MediaRegistered(filename string) (bool, error) {
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM mediapool WHERE filename = ?`, filename); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ExportGroup is one cue_group row in a .CTP manifest (format v2+). Older
// manifests (v1) simply have no groups key: their cue Parent values then map
// to nothing and import releases every cue to the top level, which is also
// what pre-group builds did.
type ExportGroup struct {
	SheetIndex    *float64 `json:"sheetIndex,omitempty"`
	GroupID       int      `json:"groupId"`
	Name          string   `json:"name"`
	ParentGroupID int      `json:"parentGroupId"`
	Collapse      bool     `json:"collapse"`
	Slideshow     bool     `json:"slideshow"`
	AwardsMode    bool     `json:"awardsMode"`
	Shuffle       bool     `json:"shuffle"`
	Loop          bool     `json:"loop"`
	FadeMS        int      `json:"fadeMs"`
	DurationMS    int      `json:"durationMs"`
	CueNum        string   `json:"cueNum"`
	Color         string   `json:"color"`
}

// ExportGroups includes visual order independently of group identity and parent.
func ExportGroups() ([]ExportGroup, error) {
	var groups []Group
	if err := db.Select(&groups, `SELECT * FROM cue_group ORDER BY group_id`); err != nil {
		log.Printf("Error ExportGroups: %v", err)
		return nil, err
	}
	out := make([]ExportGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, ExportGroup{
			SheetIndex:    &g.SheetIndex,
			GroupID:       g.GroupID,
			Name:          g.Name,
			ParentGroupID: g.ParentGroupID,
			Collapse:      g.Collapse,
			Slideshow:     g.Slideshow,
			AwardsMode:    g.AwardsMode,
			Shuffle:       g.Shuffle,
			Loop:          g.Loop,
			FadeMS:        g.FadeMS,
			DurationMS:    g.DurationMS,
			CueNum:        g.CueNum,
			Color:         g.Color,
		})
	}
	return out, nil
}

// CueCount returns the number of cues currently in the cuesheet (used to
// offset positions when appending an imported show).
func CueCount() (int, error) {
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM cuesheet`); err != nil {
		return 0, err
	}
	return n, nil
}

// uniqueCueField returns value with a numeric suffix appended until it no
// longer collides with an existing row. field must be a string literal
// ("title") chosen by the caller, never user input.
func uniqueCueField(field, value string) (string, error) {
	switch field {
	case "title":
	default:
		return "", fmt.Errorf("uniqueCueField: unsafe field %q", field)
	}
	current := value
	for suffix := 2; ; suffix++ {
		var exists int
		if err := db.Get(&exists, fmt.Sprintf(`SELECT COUNT(*) FROM cuesheet WHERE %s = ?`, field), current); err != nil {
			return "", err
		}
		if exists == 0 {
			return current, nil
		}
		current = fmt.Sprintf("%s (%d)", value, suffix)
	}
}

// AddCueFull inserts a cue with all its settings restored (import path).
// appendingStillToEnd, if nonzero, overrides the cue's exported cuePos and
// assigns the next free position (append mode). Returns the assigned cuePos.
func AddCueFull(c ExportCue) (int, error) {
	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	pos, err := importCueTx(tx, c)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	bumpCuesheetVersion()
	return pos, nil
}

// sanitizeColor drops a colour that is not a CSS hex value: imported .CTP
// files are foreign input and a bad colour must not reach a style attribute.
func sanitizeColor(c string) string {
	if ValidColor(c) {
		return c
	}
	return ""
}

// importOpacity preserves explicit zero while defaulting older manifests.
func importOpacity(v *float64) float64 {
	if v == nil || *v < 0 || *v > 100 {
		return 100
	}
	return *v
}

func importCueTx(tx *sqlx.Tx, c ExportCue) (cuePos int, err error) {
	mediaID := 0
	if c.SourceKind == "endpoint" {
		mediaID, err = importEndpointTx(tx, c.EndpointTitle, c.EndpointURL)
		if err != nil {
			return 0, err
		}
		c.PosStart, c.PosEnd, c.CueDuration, c.Hold, c.Loop, c.LoopCount, c.AutoContinue = 0, 0, 0, false, false, 0, false
	} else {
		if err = tx.Get(&mediaID, `SELECT media_id FROM mediapool WHERE source_kind = 'file' AND filename = ?`, c.Filename); err != nil {
			return 0, fmt.Errorf("media %q not found", c.Filename)
		}
	}
	if c.ScheduleDays < 0 || c.ScheduleDays > 127 || c.ScheduleTimeMs < 0 || c.ScheduleTimeMs >= 86400000 || (c.ScheduleEnabled && c.ScheduleDays == 0) {
		return 0, fmt.Errorf("invalid cue schedule")
	}
	if c.Rate == 0 {
		c.Rate = 1
	} // manifests predating playback rate
	rate, err := parseCueColumn("rate", strconv.FormatFloat(c.Rate, 'f', -1, 64))
	if err != nil {
		return 0, err
	}
	balance, err := parseCueColumn("balance", strconv.FormatFloat(c.Balance, 'f', -1, 64))
	if err != nil {
		return 0, err
	}
	if c.FadeIn < 0 {
		return 0, fmt.Errorf("invalid fadeIn")
	}
	stopOthers := 1
	if c.StopOthers != nil && !*c.StopOthers {
		stopOthers = 0
	}
	layer, err := parseCueColumn("layer", c.Layer)
	if err != nil {
		return 0, err
	}
	// Match AddCue's title de-duplication: imports into a sheet that already
	// holds a title get a numeric suffix.
	title, err := importUniqueTitle(tx, c.Title)
	if err != nil {
		return 0, err
	}

	if cuePos, err = importNextPos(tx); err != nil {
		return 0, err
	}

	cueNum, err := freeCueNum(tx, c.CueNum)
	if err != nil {
		return 0, err
	}

	_, err = tx.Exec(`
			INSERT INTO cuesheet (cuePos, cueNum, media_id, title, posStart, posEnd,
			preWait, cueDuration, postWait, hold, loop, loop_count, color,
			parent, fadeOut, fadeAction, fade_curve, fit_mode, rotation, flip, opacity, geom_x, geom_y, geom_w, geom_h, crop_l, crop_r, crop_t, crop_b, align, autoContinue, volume, fadeIn, rate, balance, mute, sheet_index, stop_others, layer, schedule_enabled, schedule_days, schedule_time_ms)
		VALUES (:cuePos, :cueNum, :mediaID, :title, :posStart, :posEnd,
			:preWait, :cueDuration, :postWait, :hold, :loop, :loop_count, :color,
			:parent, :fadeOut, :fadeAction, :fadeCurve, :fitMode, :rotation, :flip, :opacity, :geomX, :geomY, :geomW, :geomH, :cropL, :cropR, :cropT, :cropB, :align, :autoContinue, :volume, :fadeIn, :rate, :balance, :mute, :cuePos * 1000.0, :stopOthers, :layer, :scheduleEnabled, :scheduleDays, :scheduleTimeMs)
	`,
		sql.Named("cuePos", cuePos),
		sql.Named("mediaID", mediaID),
		sql.Named("cueNum", cueNum),
		sql.Named("title", title),
		sql.Named("posStart", c.PosStart),
		sql.Named("posEnd", c.PosEnd),
		sql.Named("preWait", c.PreWait),
		sql.Named("cueDuration", c.CueDuration),
		sql.Named("postWait", c.PostWait),
		sql.Named("hold", boolInt(c.Hold)),
		sql.Named("loop", boolInt(c.Loop)),
		sql.Named("loop_count", c.LoopCount),
		sql.Named("color", sanitizeColor(c.Color)),
		sql.Named("parent", c.Parent),
		sql.Named("fadeOut", c.FadeOut),
		sql.Named("fadeAction", c.FadeAction),
		sql.Named("fadeCurve", c.FadeCurve),
		sql.Named("fitMode", c.FitMode),
		sql.Named("rotation", c.Rotation),
		sql.Named("flip", c.Flip),
		sql.Named("opacity", importOpacity(c.Opacity)),
		sql.Named("geomX", c.GeomX),
		sql.Named("geomY", c.GeomY),
		sql.Named("geomW", c.GeomW),
		sql.Named("geomH", c.GeomH),
		sql.Named("cropL", c.CropL),
		sql.Named("cropR", c.CropR),
		sql.Named("cropT", c.CropT),
		sql.Named("cropB", c.CropB),
		sql.Named("align", c.Align),
		sql.Named("autoContinue", boolInt(c.AutoContinue)),
		sql.Named("volume", c.Volume),
		sql.Named("fadeIn", c.FadeIn),
		sql.Named("rate", rate),
		sql.Named("balance", balance),
		sql.Named("mute", boolInt(c.Mute)),
		sql.Named("stopOthers", stopOthers),
		sql.Named("layer", layer),
		sql.Named("scheduleEnabled", boolInt(c.ScheduleEnabled)),
		sql.Named("scheduleDays", c.ScheduleDays),
		sql.Named("scheduleTimeMs", c.ScheduleTimeMs),
	)
	if err != nil {
		log.Printf("Error adding imported cue: %v", err)
		return 0, err
	}
	// Imported members must sort directly under their group header:
	// cuePos*1000 would strand them anywhere relative to the headers.
	// Seat each member right after its header (fractional offsets keep
	// import order; headers sit on whole sheetIndexStep multiples).
	if c.Parent != 0 {
		var headerIdx float64
		if err := tx.Get(&headerIdx, `SELECT sheet_index FROM cue_group WHERE group_id = ?`, c.Parent); err != nil {
			return 0, err
		}
		var members int
		if err := tx.Get(&members, `SELECT COUNT(*) FROM cuesheet WHERE parent = ? AND cuePos != ?`, c.Parent, cuePos); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE cuesheet SET sheet_index = ? WHERE cuePos = ?`, headerIdx+float64(members+1), cuePos); err != nil {
			return 0, err
		}
	}
	return cuePos, nil
}

// ImportShow applies the complete cuesheet change in one database transaction.
func ImportShow(groups []ExportGroup, cues []ExportCue, selected int, overwrite bool) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	baseIndex := 0.0
	if !overwrite {
		if err = tx.Get(&baseIndex, `SELECT COALESCE(MAX(sheet_index),0) FROM (SELECT sheet_index FROM cuesheet UNION ALL SELECT sheet_index FROM cue_group)`); err != nil {
			return err
		}
	}
	if overwrite {
		for _, q := range []string{`DELETE FROM cuesheet`, `DELETE FROM cue_group`} {
			if _, err = tx.Exec(q); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`DELETE FROM state WHERE key = ?`, stateKeySelectedSet); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO state (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, stateKeySelectedCue, "0"); err != nil {
			return err
		}
	}
	ids := make(map[int]int, len(groups))
	for _, g := range groups {
		name := g.Name
		if name == "" {
			name = "New Group"
		}
		var max float64
		if err = tx.Get(&max, `SELECT COALESCE(MAX(sheet_index),0) FROM (SELECT sheet_index FROM cuesheet UNION ALL SELECT sheet_index FROM cue_group)`); err != nil {
			return err
		}
		num, e := freeCueNum(tx, g.CueNum)
		if e != nil {
			return e
		}
		res, e := tx.Exec(`INSERT INTO cue_group (name,parent_group_id,sheet_index,collapse,slideshow,awards_mode,shuffle,loop,fade_ms,duration_ms,cue_num,color) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, name, 0, max+sheetIndexStep, boolInt(g.Collapse), boolInt(g.Slideshow), boolInt(g.AwardsMode), boolInt(g.Shuffle), boolInt(g.Loop), g.FadeMS, g.DurationMS, num, sanitizeColor(g.Color))
		if e != nil {
			return e
		}
		id, e := res.LastInsertId()
		if e != nil {
			return e
		}
		if _, duplicate := ids[g.GroupID]; duplicate {
			return fmt.Errorf("duplicate group id %d", g.GroupID)
		}
		ids[g.GroupID] = int(id)
	}
	if err := importGroupParents(tx, groups, ids); err != nil {
		return err
	}
	positions := make(map[string]int, len(cues))
	oldPositions := make(map[int]int, len(cues))
	newPositions := make([]int, 0, len(cues))
	type link struct {
		pos    int
		num    string
		oldPos int
	}
	var links []link
	for _, c := range cues {
		c.Parent = ids[c.Parent]
		pos, e := importCueTx(tx, c)
		if e != nil {
			return e
		}
		positions[c.CueNum] = pos
		newPositions = append(newPositions, pos)
		if c.CuePos > 0 {
			if _, duplicate := oldPositions[c.CuePos]; duplicate {
				return fmt.Errorf("duplicate cue position %d", c.CuePos)
			}
			oldPositions[c.CuePos] = pos
		}
		if c.LayerUnderPos > 0 || c.LayerUnder != "" {
			links = append(links, link{pos, c.LayerUnder, c.LayerUnderPos})
		}
	}
	for _, l := range links {
		target := positions[l.num]
		if l.oldPos > 0 {
			target = oldPositions[l.oldPos]
		}
		if target > 0 {
			if _, err = tx.Exec(`UPDATE cuesheet SET layer_under = COALESCE((SELECT cue_id FROM cuesheet WHERE cuePos = ?), 0) WHERE cuePos = ?`, target, l.pos); err != nil {
				return err
			}
		}
	}
	if err := importSheetOrder(tx, groups, cues, ids, newPositions, baseIndex); err != nil {
		return err
	}
	chosen := 0
	if selected < 0 {
		chosen = -ids[-selected]
	} else if pos := oldPositions[selected]; pos > 0 {
		chosen = pos
	} else if len(oldPositions) == 0 && selected > 0 && selected <= len(newPositions) {
		chosen = newPositions[selected-1]
	}
	if chosen != 0 || overwrite {
		if _, err = tx.Exec("DELETE FROM state WHERE key = ?", stateKeySelectedSet); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO state (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", stateKeySelectedCue, strconv.Itoa(chosen)); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

func importUniqueTitle(tx *sqlx.Tx, title string) (string, error) {
	current := title
	for suffix := 2; ; suffix++ {
		var exists int
		if err := tx.Get(&exists, `SELECT COUNT(*) FROM cuesheet WHERE title = ?`, current); err != nil {
			return "", err
		}
		if exists == 0 {
			return current, nil
		}
		current = fmt.Sprintf("%s (%d)", title, suffix)
	}
}
func importNextPos(tx *sqlx.Tx) (int, error) {
	var max sql.NullInt64
	if err := tx.Get(&max, `SELECT MAX(cuePos) FROM cuesheet`); err != nil {
		return 0, err
	}
	return int(max.Int64) + 1, nil
}
func importEndpointTx(tx *sqlx.Tx, title, rawURL string) (int, error) {
	title = strings.TrimSpace(title)
	u, err := validateEndpointURL(rawURL)
	if err != nil {
		return 0, err
	}
	// A key in an imported URL (a show from before keys were kept apart)
	// becomes a stored credential, never part of the cue.
	u, key := splitScreenKey(u)
	if key != "" {
		if _, err := tx.Exec(`INSERT INTO endpoint_secret (url, secret) VALUES (?, ?)
			ON CONFLICT(url) DO UPDATE SET secret = excluded.secret`, u.String(), key); err != nil {
			return 0, err
		}
	}
	if title == "" {
		title = u.Host
	}
	var id int
	err = tx.QueryRow(`INSERT INTO mediapool (filename,source_kind,endpoint_url,endpoint_title,mimetype,size,duration,resolution,thumbnail_pending,waveform,waveform_pending,missing,media_meta) VALUES (NULL,?,?,?,?,0,0,?,0,?,0,0,?) RETURNING media_id`, "endpoint", u.String(), title, "application/x-cutepi-endpoint", "", "", "").Scan(&id)
	return id, err
}

func importGroupParents(tx *sqlx.Tx, groups []ExportGroup, ids map[int]int) error {
	parents := make(map[int]int, len(groups))
	for _, g := range groups {
		parents[g.GroupID] = g.ParentGroupID
	}
	for _, g := range groups {
		seen := map[int]bool{g.GroupID: true}
		for p, depth := g.ParentGroupID, 1; p != 0; depth++ {
			if seen[p] || depth > 8 {
				return fmt.Errorf("invalid group nesting")
			}
			parent, exists := parents[p]
			if !exists {
				return fmt.Errorf("missing parent group %d", p)
			}
			seen[p] = true
			p = parent
		}
		if _, err := tx.Exec("UPDATE cue_group SET parent_group_id = ? WHERE group_id = ?", ids[g.ParentGroupID], ids[g.GroupID]); err != nil {
			return err
		}
	}
	return nil
}

// New manifests carry both cue and header indices; old manifests retain their legacy placement.
func importSheetOrder(tx *sqlx.Tx, groups []ExportGroup, cues []ExportCue, ids map[int]int, positions []int, base float64) error {
	type item struct {
		index float64
		group bool
		id    int
	}
	var items []item
	add := func(index *float64, group bool, id int) error {
		if index == nil {
			return nil
		}
		if math.IsNaN(*index) || math.IsInf(*index, 0) {
			return fmt.Errorf("invalid sheet index")
		}
		items = append(items, item{*index, group, id})
		return nil
	}
	for _, g := range groups {
		if err := add(g.SheetIndex, true, ids[g.GroupID]); err != nil {
			return err
		}
	}
	for i, c := range cues {
		if err := add(c.SheetIndex, false, positions[i]); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return nil
	}
	if len(items) != len(groups)+len(cues) {
		return fmt.Errorf("incomplete sheet order")
	}
	minimum := items[0].index
	for _, row := range items {
		minimum = min(minimum, row.index)
	}
	for _, row := range items {
		index := base + sheetIndexStep + (row.index - minimum)
		if math.IsInf(index, 0) {
			return fmt.Errorf("sheet index out of range")
		}
		q := "UPDATE cuesheet SET sheet_index = ? WHERE cuePos = ?"
		if row.group {
			q = "UPDATE cue_group SET sheet_index = ? WHERE group_id = ?"
		}
		if _, err := tx.Exec(q, index, row.id); err != nil {
			return err
		}
	}
	return nil
}
