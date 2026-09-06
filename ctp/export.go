package ctp

import (
	"database/sql"
	"fmt"
	"log"
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
	Volume       float64 `json:"volume"`
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
		Volume       float64
	}
	var rows []row
	err = db.Select(&rows, `
		SELECT cuesheet.cueNum, cuesheet.title, mediapool.filename,
			cuesheet.posStart, cuesheet.posEnd, cuesheet.preWait,
			cuesheet.cueDuration, cuesheet.postWait, cuesheet.hold,
			cuesheet.loop, cuesheet.loop_count, cuesheet.autoContinue,
			cuesheet.color, cuesheet.parent, cuesheet.fadeOut,
			cuesheet.fadeAction, cuesheet.volume
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
			Volume:       r.Volume,
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

// CueCount returns the number of cues currently in the cuesheet (used to
// offset positions when appending an imported show).
func CueCount() (int, error) {
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM cuesheet`); err != nil {
		return 0, err
	}
	return n, nil
}

// AddCueFull inserts a cue with all its settings restored (import path).
// appendingStillToEnd, if nonzero, overrides the cue's exported cuePos and
// assigns the next free position (append mode). Returns the assigned cuePos.
func AddCueFull(c ExportCue) (cuePos int, err error) {
	// Match AddCue's title de-duplication: imports into a sheet that already
	// holds a title get a numeric suffix.
	title := c.Title
	for suffix := 2; ; suffix++ {
		var exists int
		if err := db.Get(&exists, `SELECT COUNT(*) FROM cuesheet WHERE title = ?`, title); err != nil {
			return 0, err
		}
		if exists == 0 {
			break
		}
		title = fmt.Sprintf("%s (%d)", title, suffix)
	}

	if cuePos, err = nextFreeCuePos(); err != nil {
		return 0, err
	}

	cueNum := c.CueNum
	for suffix := 2; ; suffix++ {
		var exists int
		if err := db.Get(&exists, `SELECT COUNT(*) FROM cuesheet WHERE cueNum = ?`, cueNum); err != nil {
			return 0, err
		}
		if exists == 0 {
			break
		}
		cueNum = fmt.Sprintf("%s (%d)", c.CueNum, suffix)
	}

	_, err = db.Exec(`
		INSERT INTO cuesheet (cuePos, cueNum, media_id, title, posStart, posEnd,
			preWait, cueDuration, postWait, hold, loop, loop_count, color,
			parent, fadeOut, fadeAction, autoContinue, volume)
		SELECT :cuePos, :cueNum, mp.media_id, :title, :posStart, :posEnd,
			:preWait, :cueDuration, :postWait, :hold, :loop, :loop_count, :color,
			:parent, :fadeOut, :fadeAction, :autoContinue, :volume
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
		sql.Named("autoContinue", boolInt(c.AutoContinue)),
		sql.Named("volume", c.Volume),
		sql.Named("filename", c.Filename),
	)
	if err != nil {
		log.Printf("Error adding imported cue: %v", err)
		return 0, err
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