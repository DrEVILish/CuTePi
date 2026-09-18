package ctp

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
)

// ExportCue is the JSON representation of one cue for .CTP show export /
// import. Media-time fields (waveform, thumbnail state) are not exported; the
// referenced file is carried in the ZIP instead. Group membership is exported
// once Cue Groups exist; the manifest reserves that shape via file layout.
type ExportCue struct {
	Filename     string  `json:"filename"`
	CueNum       string  `json:"cueNum"`
	Title        string  `json:"title"`
	PosStart     int     `json:"posStart"`
	PosEnd       int     `json:"posEnd"`
	PreWait      int     `json:"preWait"`
	CueDuration  int     `json:"cueDuration"`
	PostWait     int     `json:"postWait"`
	Hold         bool    `json:"hold"`
	Loop         bool    `json:"loop"`
	LoopCount    int     `json:"loopCount"`
	AutoContinue bool    `json:"autoContinue"`
	Color        string  `json:"color"`
	Parent       int     `json:"parent"`
	FadeOut      int     `json:"fadeOut"`
	FadeAction   string  `json:"fadeAction"`
	FadeCurve    string  `json:"fadeCurve"`
	FitMode      string  `json:"fitMode"`
	Rotation     int     `json:"rotation"`
	Flip         string  `json:"flip"`
	Volume       float64 `json:"volume"`
	FadeIn       int     `json:"fadeIn"`
	Rate         float64 `json:"rate"`
	Balance      float64 `json:"balance"`
	Mute         bool    `json:"mute"`
}

// ExportCues lists every cue (sheet order) as an ExportCue plus the currently
// selected cue position, for building a .CTP manifest.
func ExportCues() (cues []ExportCue, selected int, err error) {
	type row struct {
		CueNum       string `db:"cueNum"`
		Title        string `db:"title"`
		Filename     string `db:"filename"`
		PosStart     int    `db:"posStart"`
		PosEnd       int    `db:"posEnd"`
		PreWait      int    `db:"preWait"`
		CueDuration  int    `db:"cueDuration"`
		PostWait     int    `db:"postWait"`
		Hold         bool   `db:"hold"`
		Loop         bool   `db:"loop"`
		LoopCount    int    `db:"loop_count"`
		AutoContinue bool   `db:"autoContinue"`
		Color        string `db:"color"`
		Parent       int    `db:"parent"`
		FadeOut      int    `db:"fadeOut"`
		FadeAction   string `db:"fadeAction"`
		FadeCurve    string `db:"fade_curve"`
		FitMode      string `db:"fit_mode"`
		Rotation     int    `db:"rotation"`
		Flip         string `db:"flip"`
		Volume       float64
		FadeIn       int     `db:"fadeIn"`
		Rate         float64 `db:"rate"`
		Balance      float64 `db:"balance"`
		Mute         bool    `db:"mute"`
	}
	var rows []row
	err = db.Select(&rows, `
		SELECT cuesheet.cueNum, cuesheet.title, mediapool.filename,
			cuesheet.posStart, cuesheet.posEnd, cuesheet.preWait,
			cuesheet.cueDuration, cuesheet.postWait, cuesheet.hold,
			cuesheet.loop, cuesheet.loop_count, cuesheet.autoContinue,
			cuesheet.color, cuesheet.parent, cuesheet.fadeOut,
			cuesheet.fadeAction, cuesheet.fade_curve, cuesheet.fit_mode, cuesheet.rotation, cuesheet.flip, cuesheet.volume, cuesheet.fadeIn, cuesheet.rate, cuesheet.balance, cuesheet.mute
		FROM cuesheet
		LEFT JOIN mediapool ON cuesheet.media_id = mediapool.media_id
		ORDER BY cuesheet.cuePos
	`)
	if err != nil {
		log.Printf("Error ExportCues: %v", err)
		return nil, 0, err
	}
	cues = make([]ExportCue, 0, len(rows))
	for _, r := range rows {
		cues = append(cues, ExportCue{
			Filename:     r.Filename,
			CueNum:       r.CueNum,
			Title:        r.Title,
			PosStart:     r.PosStart,
			PosEnd:       r.PosEnd,
			PreWait:      r.PreWait,
			CueDuration:  r.CueDuration,
			PostWait:     r.PostWait,
			Hold:         r.Hold,
			Loop:         r.Loop,
			LoopCount:    r.LoopCount,
			AutoContinue: r.AutoContinue,
			Color:        r.Color,
			Parent:       r.Parent,
			FadeOut:      r.FadeOut,
			FadeAction:   r.FadeAction,
			FadeCurve:    r.FadeCurve,
			FitMode:      r.FitMode,
			Rotation:     r.Rotation,
			Flip:         r.Flip,
			Volume:       r.Volume,
			FadeIn:       r.FadeIn,
			Rate:         r.Rate,
			Balance:      r.Balance,
			Mute:         r.Mute,
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
	GroupID       int    `json:"groupId"`
	Name          string `json:"name"`
	ParentGroupID int    `json:"parentGroupId"`
	Collapse      bool   `json:"collapse"`
	Slideshow     bool   `json:"slideshow"`
	Shuffle       bool   `json:"shuffle"`
	Loop          bool   `json:"loop"`
	FadeMS        int    `json:"fadeMs"`
	DurationMS    int    `json:"durationMs"`
	CueNum        string `json:"cueNum"`
	Color         string `json:"color"`
}

// ExportGroups lists every cue group ordered by id. Creation order equals id
// order (a group can only be created inside an existing parent), so parents
// always precede their children — ImportGroups relies on that to remap
// nested parent ids.
func ExportGroups() ([]ExportGroup, error) {
	var groups []Group
	if err := db.Select(&groups, `SELECT * FROM cue_group ORDER BY group_id`); err != nil {
		log.Printf("Error ExportGroups: %v", err)
		return nil, err
	}
	out := make([]ExportGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, ExportGroup{
			GroupID:       g.GroupID,
			Name:          g.Name,
			ParentGroupID: g.ParentGroupID,
			Collapse:      g.Collapse,
			Slideshow:     g.Slideshow,
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

// ImportGroups recreates the manifest's groups and returns the exported-id →
// new-id map. Parents are created before children (ExportGroups' ordering
// guarantee), so a child's parent_group_id resolves through the map as it is
// filled. On a mid-way failure the groups created so far are removed — the
// sheet must not keep half an import's group tree.
func ImportGroups(groups []ExportGroup) (map[int]int, error) {
	idMap := make(map[int]int, len(groups))
	for _, eg := range groups {
		newID, err := CreateGroup(eg.Name, idMap[eg.ParentGroupID])
		if err != nil {
			ImportGroupsRollback(idMap)
			return nil, err
		}
		idMap[eg.GroupID] = newID
		g := Group{
			GroupID:       newID,
			Name:          eg.Name,
			ParentGroupID: idMap[eg.ParentGroupID],
			Collapse:      eg.Collapse,
			Slideshow:     eg.Slideshow,
			Shuffle:       eg.Shuffle,
			Loop:          eg.Loop,
			FadeMS:        eg.FadeMS,
			DurationMS:    eg.DurationMS,
			CueNum:        eg.CueNum,
			Color:         eg.Color,
		}
		if err := UpdateGroup(g); err != nil {
			ImportGroupsRollback(idMap)
			return nil, err
		}
	}
	return idMap, nil
}

// ImportGroupsRollback removes the groups created by a failed ImportGroups.
func ImportGroupsRollback(idMap map[int]int) {
	for _, newID := range idMap {
		_ = DeleteGroup(newID) // best-effort; DeleteGroup also releases members
	}
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
// ("title" or "cueNum") chosen by the caller, never user input.
func uniqueCueField(field, value string) (string, error) {
	switch field {
	case "title", "cueNum":
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
func AddCueFull(c ExportCue) (cuePos int, err error) {
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
	// Match AddCue's title de-duplication: imports into a sheet that already
	// holds a title get a numeric suffix.
	title, err := uniqueCueField("title", c.Title)
	if err != nil {
		return 0, err
	}

	if cuePos, err = nextFreeCuePos(); err != nil {
		return 0, err
	}

	cueNum, err := uniqueCueField("cueNum", c.CueNum)
	if err != nil {
		return 0, err
	}

	_, err = db.Exec(`
			INSERT INTO cuesheet (cuePos, cueNum, media_id, title, posStart, posEnd,
			preWait, cueDuration, postWait, hold, loop, loop_count, color,
			parent, fadeOut, fadeAction, fade_curve, fit_mode, rotation, flip, autoContinue, volume, fadeIn, rate, balance, mute, sheet_index)
		SELECT :cuePos, :cueNum, mp.media_id, :title, :posStart, :posEnd,
			:preWait, :cueDuration, :postWait, :hold, :loop, :loop_count, :color,
			:parent, :fadeOut, :fadeAction, :fadeCurve, :fitMode, :rotation, :flip, :autoContinue, :volume, :fadeIn, :rate, :balance, :mute, :cuePos * 1000.0
		FROM (SELECT media_id FROM mediapool WHERE filename = :filename) AS mp
	`,
		sql.Named("cuePos", cuePos),
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
		sql.Named("color", c.Color),
		sql.Named("parent", c.Parent),
		sql.Named("fadeOut", c.FadeOut),
		sql.Named("fadeAction", c.FadeAction),
		sql.Named("fadeCurve", c.FadeCurve),
		sql.Named("fitMode", c.FitMode),
		sql.Named("rotation", c.Rotation),
		sql.Named("flip", c.Flip),
		sql.Named("autoContinue", boolInt(c.AutoContinue)),
		sql.Named("volume", c.Volume),
		sql.Named("fadeIn", c.FadeIn),
		sql.Named("rate", rate),
		sql.Named("balance", balance),
		sql.Named("mute", boolInt(c.Mute)),
		sql.Named("filename", c.Filename),
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
		if err := db.Get(&headerIdx, `SELECT sheet_index FROM cue_group WHERE group_id = ?`, c.Parent); err == nil {
			var members int
			_ = db.Get(&members, `SELECT COUNT(*) FROM cuesheet WHERE parent = ? AND cuePos != ?`, c.Parent, cuePos)
			_, _ = db.Exec(`UPDATE cuesheet SET sheet_index = ? WHERE cuePos = ?`,
				headerIdx+float64(members+1), cuePos)
		}
	}
	bumpCuesheetVersion()
	return cuePos, nil
}

// nextFreeCuePos returns one past the highest cuePos.
func nextFreeCuePos() (int, error) {
	var max sql.NullInt64
	if err := db.Get(&max, `SELECT MAX(cuePos) FROM cuesheet`); err != nil {
		return 0, err
	}
	return int(max.Int64) + 1, nil
}

// SelectedCuePosFor re-targets the selection after an import, remapping the
// exported position: append mode must offset it past pre-existing cues.
func SelectedCuePosFor(exported, exportedTotal, appendedOffset int) {
	if exported <= 0 || exported > exportedTotal {
		return
	}
	_ = setSelectedCuePos(exported + appendedOffset) // best-effort
}
