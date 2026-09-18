package routes

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"

	"github.com/gin-gonic/gin"
)

// goSafe runs fn on its own goroutine, converting a panic into a logged
// error instead of a process kill. Gin's Recovery middleware only covers
// the request goroutine; these goroutines outlive it and run mid-show
// (fades, auto-continue timers), where a panic would abort the whole
// appliance.
func goSafe(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("background task panic recovered: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

type MediapoolItem struct {
	Filename  string
	Size      string
	Mimetype  string
	Thumbnail string
	DateAdded string
	Missing   bool
}

func formatSize(bytes int) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// formatClock renders a duration in seconds as mm:ss, or h:mm:ss once it
// reaches an hour, for the "Now Playing" widget.
func formatClock(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds + 0.5)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// mediapoolView loads the mediapool from the DB and maps it to the shape
// mediapool.html expects. Shared by the index page and the upload/youtube
// handlers, which re-render the same partial after adding media.
func mediapoolView() ([]MediapoolItem, error) {
	pool, err := ctp.GetMediapool()
	if err != nil {
		return nil, err
	}
	mediapool := make([]MediapoolItem, 0, len(pool.Medias))
	for _, m := range pool.Medias {
		thumbnail := "/thumbnails/" + url.PathEscape(m.Filename) + ".jpg"
		if m.ThumbnailPending {
			thumbnail = ""
		}
		mediapool = append(mediapool, MediapoolItem{
			Filename:  m.Filename,
			Size:      formatSize(m.Size),
			Mimetype:  m.Mimetype,
			Thumbnail: thumbnail,
			DateAdded: m.DateAdded.Local().Format("2006-01-02 15:04"),
			Missing:   m.Missing,
		})
	}
	return mediapool, nil
}

// nowplayingData is the template data for the mediainfo.html partial, shared
// by the index page (initial server render) and GET /api/nowplaying (the full
// re-render triggered by the change-detection poller). The Filename/Position/
// Duration keys keep the widget accurate from the very first paint, before the
// first poll.
// PaletteColour is one entry of the cue colour picker: name + hex.
type PaletteColour struct {
	Name string
	Hex  string
}

// cuePalette is the fixed set of 12 named cue colours offered in the inspector
// dropdown, kept in rainbow order (spectrum first, then the neutral tones) —
// a slice, not a map, because template map ranges iterate alphabetically.
var cuePalette = []PaletteColour{
	{"Red", "#dc3545"},
	{"Orange", "#fd7e14"},
	{"Yellow", "#ffc107"},
	{"Green", "#28a745"},
	{"Teal", "#20c997"},
	{"Cyan", "#17a2b8"},
	{"Blue", "#007bff"},
	{"Indigo", "#6610f2"},
	{"Purple", "#a855f7"},
	{"Pink", "#e83e8c"},
	{"Brown", "#795548"},
	{"Grey", "#6c757d"},
}

// MediaRow is one label/value pair rendered in the Cue Inspector's Media tab.
type MediaRow struct {
	Label string
	Value string
}

// mediaInfoRows flattens the stored media_meta JSON into a presentation list
// (VLC/ProPresenter-style codec detail). Empty values are skipped; a media
// file without stored detail falls back to the base Media columns.
func mediaInfoRows(cue ctp.Cue) []MediaRow {
	rows := make([]MediaRow, 0, 14)
	add := func(label, value string) {
		if value != "" && value != "0" {
			rows = append(rows, MediaRow{Label: label, Value: value})
		}
	}
	var info media.MediaInfo
	unmarshalErr := json.Unmarshal([]byte(cue.MediaInfo), &info) != nil
	// Stale or pre-audio-import meta: if the stored JSON came up without an
	// audio stream, re-probe the actual file once and persist the fresh meta,
	// so the Media tab reflects the real streams even for old imports.
	// ponytail: a genuinely silent video re-probes on every inspector open;
	// add a persisted sentinel if such files turn up enough to matter.
	isAV := strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "audio/")
	if isAV && info.Audio == nil {
		if meta, perr := media.Probe(filepath.Join(config.MediaLocation(), cue.Filename)); perr == nil && meta.Info != nil {
			info = *meta.Info
			unmarshalErr = false
			if raw, jerr := json.Marshal(meta.Info); jerr == nil {
				if uerr := ctp.UpdateMediaMeta(cue.Filename, string(raw)); uerr != nil {
					log.Printf("mediaInfoRows: storing refreshed meta for %q: %v", cue.Filename, uerr)
				}
			}
		}
	}
	if unmarshalErr {
		return []MediaRow{
			{Label: "Type", Value: cue.Mimetype},
			{Label: "Codec", Value: cue.MediaType},
			{Label: "Resolution", Value: cue.Resolution},
		}
	}
	add("Container", info.Container)
	if info.Duration > 0 {
		rows = append(rows, MediaRow{Label: "Duration", Value: ctp.FormatTime(int(info.Duration * 1000))})
	}
	if info.OverallBitrate > 0 {
		add("Overall bitrate", fmt.Sprintf("%d kbps", info.OverallBitrate/1000))
	}
	if v := info.Video; v != nil {
		add("Video codec", strings.TrimSpace(v.Codec+" "+v.Profile))
		if v.Width > 0 && v.Height > 0 {
			add("Resolution", fmt.Sprintf("%d x %d", v.Width, v.Height))
		}
		if v.FPS > 0 {
			add("Frame rate", fmt.Sprintf("%.3f fps", v.FPS))
		}
		add("Pixel format", v.PixFmt)
		add("Colour space", v.Color)
		if v.Bitrate > 0 {
			add("Video bitrate", fmt.Sprintf("%d kbps", v.Bitrate/1000))
		}
	}
	if a := info.Audio; a != nil {
		add("Audio codec", a.Codec)
		if a.Layout != "" {
			add("Channels", a.Layout)
		} else if a.Channels > 0 {
			add("Channels", fmt.Sprintf("%d", a.Channels))
		}
		if a.SampleRate > 0 {
			add("Sample rate", fmt.Sprintf("%d Hz", a.SampleRate))
		}
		if a.Bitrate > 0 {
			add("Audio bitrate", fmt.Sprintf("%d kbps", a.Bitrate/1000))
		}
	}
	return rows
}

func inspectorData() gin.H {
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		return gin.H{"Cue": ctp.Cue{}, "Selected": false, "MediaDuration": 0, "Palette": cuePalette, "Pool": replacementPool()}
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		return gin.H{"Cue": ctp.Cue{}, "Selected": false, "MediaDuration": 0, "Palette": cuePalette, "Pool": replacementPool(), "ScheduleDay": 0}
	}
	return gin.H{
		"Cue":           cue,
		"Selected":      true,
		"MediaDuration": cue.Duration,
		"Palette":       cuePalette,
		"Pool":          replacementPool(),
		"MediaRows":     mediaInfoRows(cue),
		"ScheduleDay":   schedDayNum(cue.ScheduleDays),
	}
}

// schedDayNum converts the schedule_days bitmask back to a 1=Mon..7=Sun day
// number for the inspector dropdown (0 = unset/multi-bit, never written by
// the UI which sets exactly one bit).
func schedDayNum(bitmask int) int {
	for d := 1; d <= 7; d++ {
		if bitmask == 1<<(d-1) {
			return d
		}
	}
	return 0
}

// replacementPool is the media pool subset whose source files exist on disk,
// offered as re-link targets for cues whose source has gone missing.
func replacementPool() []MediapoolItem {
	items, err := mediapoolView()
	if err != nil {
		return nil
	}
	ok := make([]MediapoolItem, 0, len(items))
	for _, it := range items {
		if !it.Missing {
			ok = append(ok, it)
		}
	}
	return ok
}

// typeIcon maps the cached MediaType kind to a Bootstrap icon class used for
// the cue row's media-type glyph.

// AssetStamp returns a value that changes with every deployment (the running
// binary's mtime), used as a versioned query string on the CSS/JS URLs in
// header/footer so browsers drop stale cached files without a hard refresh.
func AssetStamp() string {
	exe, err := os.Executable()
	if err != nil {
		return "0"
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "0"
	}
	return strconv.FormatInt(info.ModTime().Unix(), 36)
}

func TypeIcon(kind string) string {
	switch kind {
	case "video":
		return "bi-film"
	case "audio":
		return "bi-music-note-beamed"
	case "image":
		return "bi-image"
	default:
		return "bi-file-earmark"
	}
}

// displayTime renders milliseconds as a clock string for the cue progress bar
// (::title). Shares formatClock's formatting by converting to seconds.
func DisplayTime(ms int) string {
	return formatClock(float64(ms) / 1000)
}

// progressPct returns the clamped percentage (0-100) of elapsed over total,
// used to size the per-cue progress bar fill.
func ProgressPct(pos, dur int) int {
	if dur <= 0 {
		return 0
	}
	pct := (pos * 100) / dur
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func nowplayingData() gin.H {
	pos := gsp.CurrentPosition()
	dur := gsp.CurrentDuration()
	rem := dur - pos
	if rem < 0 {
		rem = 0
	}
	data := gin.H{
		"Filename":    gsp.CurrentPlaying(),
		"Position":    formatClock(pos),
		"PositionRaw": pos,
		"Duration":    formatClock(dur),
		"DurationRaw": dur,
		"Remaining":   formatClock(rem),
	}
	// Merged header (§5.2): the partial also carries the GO cluster and the
	// playing cue's identity, so one version-guarded render keeps the whole
	// bar truthful.
	data["GoAdvance"] = ctp.GetGoAdvance()
	if sheet, err := ctp.GetCuesheet(); err == nil {
		if idx, err := ctp.SelectUnitIndex(); err == nil {
			if units, err := ctp.SelectUnits(); err == nil && idx >= 0 && idx < len(units) {
				u := units[idx]
			describe := func(u ctp.SelectUnit) (string, string, string) {
				if u.IsGroup {
					for _, g := range sheet.Groups {
						if g.GroupID == u.GroupID {
							return g.CueNum, g.Name, g.Color
						}
					}
				} else {
					for _, cue := range sheet.Cues {
						if cue.CuePos == u.CuePos {
							return cue.CueNum, cue.Title, cue.Color
						}
					}
				}
				return "", "", ""
			}
			data["GoNum"], data["GoTitle"], data["GoColor"] = describe(u)
			data["GoHasSel"] = true
			if idx+1 < len(units) {
				data["GoNextNum"], data["GoNextTitle"], _ = describe(units[idx+1])
				data["GoHasNext"] = true
			}
			}
		}
		if playingPos := gsp.CurrentCuePos(); playingPos != 0 {
			for _, cue := range sheet.Cues {
				if cue.CuePos == playingPos {
					data["PlayingCueNum"] = cue.CueNum
					data["PlayingCueTitle"] = cue.Title
					break
				}
			}
		}
	}
	return data
}

// enrichCuesheetWithPlayback marks the currently-playing cue (by filename,
// matching the active gsp pipeline) and fills its per-cue progress-bar data
// (PlayPos/PlayDur in ms). Only one cue plays at a time.
func enrichCuesheetWithPlayback(cuesheet *ctp.Cuesheet) {
	// Wait countdown (§12.2): tag the cue a chain wait is counting down on.
	if w := CurrentWait(); w.CuePos != 0 {
		left := (w.EndsAt - time.Now().UnixMilli()) / 1000
		if left < 0 {
			left = 0
		}
		for i := range cuesheet.Cues {
			if cuesheet.Cues[i].CuePos == w.CuePos {
				cuesheet.Cues[i].WaitKind = w.Kind
				cuesheet.Cues[i].WaitLeftS = int(left)
				break
			}
		}
	}
	// Match by cue position, not filename: two cues can reference the same
	// media file, and filename matching highlights the wrong row.
	playingPos := gsp.CurrentCuePos()
	if playingPos == 0 {
		return
	}
	pos := gsp.CurrentPosition()
	dur := gsp.CurrentDuration()
	for i := range cuesheet.Cues {
		if cuesheet.Cues[i].CuePos == playingPos {
			cuesheet.Cues[i].Playing = true
			cuesheet.Cues[i].PlayPos = int(pos * 1000)
			cuesheet.Cues[i].PlayDur = int(dur * 1000)
			break
		}
	}
}

// loadAndPlayCue builds LoadOpts for a cue and plays it. Shared by the cue
// play route and the fade-then-play path.
func loadAndPlayCue(cue ctp.Cue) error {
	opts := gsp.LoadOpts{
		InPoint:      float64(cue.PosStart) / 1000,
		OutPoint:     float64(cue.PosEnd) / 1000,
		Hold:         cue.Hold && (strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "image/")),
		Loop:         cue.Loop,
		LoopCount:    cue.LoopCount,
		Volume:       cue.Volume,
		LoudnessGain: cue.LoudnessGain,
		Rate:         cue.Rate,
		Balance:      cue.Balance,
		Mute:         cue.Mute,
		FadeIn:       cue.FadeIn,
		FadeCurve:    cue.FadeCurve,
		FitMode:      cue.FitMode,
		Rotation:     cue.Rotation,
		Flip:         cue.Flip,
	}
	if err := gsp.LoadWithOpts(cue.Filename, opts); err != nil {
		ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
		return err
	}
	ctp.SetCueResult(cue.CuePos, ctp.CueResultOK) // fired; cleared to error on end-hook trouble
	gsp.SetCuePos(cue.CuePos)
	gsp.Play()
	logs.Emit(logs.AuditEvent{Event: "cue_start", Pos: cue.CuePos, Title: cue.Title})
	// Still images never reach end-of-stream, so a set display duration
	// ends the cue on a timer (then auto-continues like any other end).
	// Hold keeps the frame up and only advances the chain. The generation
	// guard drops stale timers when the operator acts meanwhile.
	if strings.HasPrefix(cue.Mimetype, "image/") && cue.CueDuration > 0 {
		gen, pos, hold := gsp.Generation(), cue.CuePos, cue.Hold
		dur := time.Duration(cue.CueDuration) * time.Millisecond
		goSafe(func() {
			time.Sleep(dur)
			if gsp.Generation() != gen {
				return
			}
			if !hold {
				gsp.Stop()
			}
			autoContinueFrom(pos)
		})
	}
	return nil
}

// waitState is the live wait countdown (§12.2): which cue is waiting, what
// kind of wait, and when it ends (unix ms). The auto-continue chain writes
// it from 250ms ticks; the cuesheet render and the per-second version bump
// carry it to clients, so a hung cue never masquerades as a deliberate wait.
type waitState struct {
	CuePos int
	Kind   string // "pre" | "post"
	EndsAt int64  // unix ms
}

var (
	waitMu  sync.Mutex
	waitNow waitState
)

func setWait(ws waitState) {
	waitMu.Lock()
	waitNow = ws
	waitMu.Unlock()
	ctp.NotifyCuesheetChanged()
}

func clearWait() {
	waitMu.Lock()
	had := waitNow.CuePos != 0
	waitNow = waitState{}
	waitMu.Unlock()
	if had {
		ctp.NotifyCuesheetChanged()
	}
}

// CurrentWait returns the active wait (zero value when none).
func CurrentWait() waitState {
	waitMu.Lock()
	defer waitMu.Unlock()
	return waitNow
}

// waitTicks sleeps for d in 250ms steps while keeping waitNow live, bumping
// the cuesheet version ~1/s so clients' countdown pills advance. Aborts
// early (returns false) when gen moves — the operator did something else.
func waitTicks(d time.Duration, cuePos int, kind string, gen uint64) bool {
	if d <= 0 {
		return true
	}
	setWait(waitState{CuePos: cuePos, Kind: kind, EndsAt: time.Now().Add(d).UnixMilli()})
	defer clearWait()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var waited time.Duration
	lastBump := time.Now()
	for {
		select {
		case <-tick.C:
			waited += 250 * time.Millisecond
			if gsp.Generation() != gen {
				return false
			}
			if time.Since(lastBump) >= time.Second {
				lastBump = time.Now()
				ctp.NotifyCuesheetChanged()
			}
			if waited >= d {
				return true
			}
		}
	}
}

// autoContinueFrom implements per-cue auto-continue: a cue flagged
// autoContinue plays the next cue in sheet order after it ends. The ending
// cue's postWait and the next cue's preWait pause the chain (waits are only
// meaningful between auto-continuing cues). The whole advance is skipped if
// the operator does anything else during waits, so a triggered cue can never
// interrupt a newer decision.
func autoContinueFrom(endingPos int) {
	cue, err := ctp.GetCue(strconv.Itoa(endingPos))
	if err != nil || !cue.AutoContinue {
		return
	}
	next, err := ctp.NextCuePos(endingPos)
	if err != nil || next == 0 {
		return
	}
	nextCue, err := ctp.GetCue(strconv.Itoa(next))
	if err != nil {
		return
	}
	// Post-wait only here: the next cue's pre-wait is waited separately
	// below. Adding it here too waited preWait twice.
	delay := time.Duration(cue.PostWait) * time.Millisecond
	gen := gsp.Generation()
	goSafe(func() {
		// Ticked wait: keeps the countdown state live so clients can show
		// WHAT is waiting and for how long (§12.2), not just silence.
		if !waitTicks(delay, endingPos, "post", gen) {
			return
		}
		// Generation guard: fires only if the playback decision state is
		// unchanged since arming. Any operator action during the wait (load,
		// stop, panic — including re-triggering the SAME cue, which leaves
		// cuePos equal but bumps the generation) disarms the chain, so a
		// triggered cue can never interrupt a newer decision.
		if gsp.Generation() != gen {
			return
		}
		if nextCue.PreWait > 0 && nextCue.AutoContinue {
			if !waitTicks(time.Duration(nextCue.PreWait)*time.Millisecond, next, "pre", gen) {
				return
			}
			if gsp.Generation() != gen {
				return
			}
		}
		// Re-fetch: the operator may have edited the next cue during the
		// waits; the pre-wait snapshot would play stale values.
		if fresh, err := ctp.GetCue(strconv.Itoa(next)); err == nil {
			nextCue = fresh
		}
		if err := loadAndPlayCue(nextCue); err != nil {
			log.Printf("auto-continue: loading next cue %d failed: %v", next, err)
			ctp.SetCueResult(next, ctp.CueResultError)
			return
		}
		_ = ctp.SetCue(strconv.Itoa(next))
	})
}

// renderCuesheet fetches the cuesheet, tags the currently-playing cue, and
// renders the partial. Shared by every action that returns the cuesheet HTML.
func renderCuesheet(c *gin.Context) {
	cuesheet, err := ctp.GetCuesheet()
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}
	enrichCuesheetWithPlayback(&cuesheet)
	c.HTML(http.StatusOK, "cuesheet.html", gin.H{
		"Cuesheet": cuesheet,
		"Rows":     sheetRowsWithSelection(&cuesheet),
		"GoBar":    computeGoBar(&cuesheet),
		"GoAdvance": ctp.GetGoAdvance(),
	})
}

func Index(rg *gin.RouterGroup) {
	// Auto-continue: when a cue with the autoContinue flag reaches its end,
	// wait its postWait, then play the next cue in sheet order (waiting the
	// next cue's preWait too if it is itself auto-continuing). Loop always
	// wins: gsp only fires the end hook once a finite loop count is exhausted.
	gsp.SetCueEndHook(func(pos int) {
		if cue, err := ctp.GetCue(strconv.Itoa(pos)); err == nil {
			logs.Emit(logs.AuditEvent{Event: "cue_end", Pos: pos, Title: cue.Title})
		}
		autoContinueFrom(pos)
	})

	rg.GET("/", func(c *gin.Context) {
		mediapool, err := mediapoolView()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}

		cuesheet, err := ctp.GetCuesheet()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		enrichCuesheetWithPlayback(&cuesheet)

		data := nowplayingData()
		data["Mediapool"] = mediapool
		data["Cuesheet"] = cuesheet
		data["Rows"] = sheetRowsWithSelection(&cuesheet)
		data["GoBar"] = computeGoBar(&cuesheet)
		data["GoAdvance"] = ctp.GetGoAdvance()
		data["Inspector"] = inspectorData()
		data["ShowMode"] = ctp.GetShowMode()
		data["title"] = "CuTePi"
		c.HTML(http.StatusOK, "index.html", data)
	})

	// Standalone mediapool partial endpoint. Used to re-render the panel after
	// a media item is deleted or a direct fetch refresh is wanted.
	rg.GET("/mediapool", func(c *gin.Context) {
		mediapool, err := mediapoolView()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{
			"Mediapool": mediapool,
		})
	})
}

// importMedia probes, verifies playability, registers, and moves srcPath
// into the media directory under filename. On failure it removes srcPath and
// returns the error, leaving no trace behind. The single shared import path
// for uploads, youtube downloads, and .CTP show imports — every caller must
// reject a source that fails probe/verify/registration before it lands in
// the pool. Imports always probe srcPath; after renaming into place the
// path is final, which is why (unlike the upload path) to-be-imported cues
// write directly to destPath and pass that path here.
func importMedia(filename, srcPath string) error {
	meta, err := media.Probe(srcPath)
	if err != nil {
		os.Remove(srcPath)
		return err
	}
	if meta.Kind == media.KindVideo || meta.Kind == media.KindAudio {
		if err := media.VerifyPlayable(srcPath); err != nil {
			os.Remove(srcPath)
			return err
		}
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		os.Remove(srcPath)
		return err
	}
	title := strings.TrimSuffix(filename, filepath.Ext(filename))
	destPath := filepath.Join(config.MediaLocation(), filename)
	if err := os.Rename(srcPath, destPath); err != nil {
		os.Remove(srcPath)
		return err
	}
	if err := ctp.RegisterMedia(filename, info.Size(), meta, title); err != nil {
		os.Remove(destPath)
		return err
	}
	return nil
}
