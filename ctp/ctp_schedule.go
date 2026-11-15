package ctp

import (
	"errors"
	"time"
)

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
	CueID         int     `db:"cue_id"` // stable identity: cuePos changes on reorder
	CuePos        int     `db:"cuePos"`
	Title         string  `db:"title"`
	Filename      string  `db:"filename"`
	SourceKind    string  `db:"source_kind"`
	EndpointURL   string  `db:"endpoint_url"`
	EndpointTitle string  `db:"endpoint_title"`
	ScheduleMs    int     `db:"schedule_time_ms"`
	PosStart      int     `db:"posStart"`
	PosEnd        int     `db:"posEnd"`
	Hold          bool    `db:"hold"`
	Loop          bool    `db:"loop"`
	LoopCount     int     `db:"loop_count"`
	Volume        float64 `db:"volume"`
	LoudnessGain  float64 `db:"loudness_gain"`
	Rate          float64 `db:"rate"`
	Balance       float64 `db:"balance"`
	Mute          bool    `db:"mute"`
	FadeIn        int     `db:"fadeIn"`
	FadeCurve     string  `db:"fade_curve"`
	FitMode       string  `db:"fit_mode"`
	Rotation      int     `db:"rotation"`
	Flip          string  `db:"flip"`
	Opacity       float64 `db:"opacity"`
	GeomX         string  `db:"geom_x"`
	GeomY         string  `db:"geom_y"`
	GeomW         string  `db:"geom_w"`
	GeomH         string  `db:"geom_h"`
	CropL         string  `db:"crop_l"`
	CropR         string  `db:"crop_r"`
	CropT         string  `db:"crop_t"`
	CropB         string  `db:"crop_b"`
	Align         string  `db:"align"`
	StopOthers    bool    `db:"stop_others"`
	Layer         string  `db:"layer"`
	LayerUnder    int     `db:"layer_under"`
	Mimetype      string  `db:"mimetype"`
}

// AsCue rebuilds the cue-level view of a scheduled row (the subset the
// transport honours: filename, trim, playback settings) so one opts builder
// serves both a fetched cue and a fetched schedule. Not a DB round trip:
// the fire path re-fetches a fresh ctp.Cue when it fires, which is what
// keeps late edits honest.
func (s ScheduleInfo) AsCue() Cue {
	return Cue{
		Media:      Media{Filename: s.Filename, SourceKind: s.SourceKind, EndpointURL: s.EndpointURL, EndpointTitle: s.EndpointTitle, Mimetype: s.Mimetype, LoudnessGain: s.LoudnessGain},
		CuePos:     s.CuePos,
		Title:      s.Title,
		PosStart:   s.PosStart,
		PosEnd:     s.PosEnd,
		Hold:       s.Hold,
		Loop:       s.Loop,
		LoopCount:  s.LoopCount,
		Volume:     s.Volume,
		Rate:       s.Rate,
		Balance:    s.Balance,
		Mute:       s.Mute,
		FadeIn:     s.FadeIn,
		FadeCurve:  s.FadeCurve,
		FitMode:    s.FitMode,
		Rotation:   s.Rotation,
		Flip:       s.Flip,
		Opacity:    s.Opacity,
		GeomX:      s.GeomX,
		GeomY:      s.GeomY,
		GeomW:      s.GeomW,
		GeomH:      s.GeomH,
		CropL:      s.CropL,
		CropR:      s.CropR,
		CropT:      s.CropT,
		CropB:      s.CropB,
		Align:      s.Align,
		StopOthers: s.StopOthers,
		Layer:      s.Layer,
		LayerUnder: s.LayerUnder,
	}
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
	best := time.Duration(1 << 62)
	for _, r := range rows {
		for d := 0; d <= 7; d++ {
			if r.Days&(1<<((todayBit+d)%7)) == 0 {
				continue
			}
			date := midnight.AddDate(0, 0, d)
			at := time.Date(date.Year(), date.Month(), date.Day(), r.TimeMs/3600000, (r.TimeMs/60000)%60, (r.TimeMs/1000)%60, (r.TimeMs%1000)*int(time.Millisecond), now.Location())
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

// GetScheduledCuesSince returns the cues due after from (exclusive, whole
// seconds) through the next second, allowing preroll before a timed start.
// The lower edge is the one-second due window, stretched back
// to from when the scheduler was stalled. The span never crosses midnight:
// from is clamped to the start of now's day.
func GetScheduledCuesSince(from, now time.Time) ([]ScheduleInfo, error) {
	day := int(now.Weekday())
	if day == 0 {
		day = 7 // Sunday = bit6
	}
	timeMs := now.Hour()*3600*1000 + now.Minute()*60*1000 + now.Second()*1000 + now.Nanosecond()/int(time.Millisecond)
	fromMs := -1 // start of day
	if y, d := from.YearDay(), from.Year(); y == now.YearDay() && d == now.Year() {
		fromMs = from.Hour()*3600*1000 + from.Minute()*60*1000 + from.Second()*1000
	}
	if fromMs > timeMs-1000 {
		fromMs = timeMs - 1000
	}
	var rows []ScheduleInfo
	err := db.Select(&rows, `
		SELECT c.cue_id, c.cuePos, c.title, COALESCE(m.filename, '') AS filename, m.source_kind, m.endpoint_url, m.endpoint_title, c.schedule_time_ms, c.posStart, c.posEnd,
			c.hold, c.loop, c.loop_count, c.volume, m.loudness_gain,
			c.rate, c.balance, c.mute, c.fadeIn, c.fade_curve,
			c.fit_mode, c.rotation, c.flip, c.opacity, c.geom_x, c.geom_y, c.geom_w, c.geom_h, c.crop_l, c.crop_r, c.crop_t, c.crop_b, c.align,
			c.stop_others, c.layer, c.layer_under,
			m.mimetype
		FROM cuesheet c
		JOIN mediapool m ON c.media_id = m.media_id
		WHERE c.schedule_enabled = 1
		AND ((c.schedule_days & ?) = ?)
		AND c.schedule_time_ms <= ? + 1000
		AND c.schedule_time_ms > ?
		AND (c.last_played_at = 0 OR c.last_played_at < ?)
		ORDER BY c.schedule_time_ms ASC, c.sheet_index ASC`,
		1<<(day-1), 1<<(day-1), timeMs, fromMs, now.UnixMilli())
	return rows, err
}
