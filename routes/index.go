package routes

import (
	"encoding/json"
	"fmt"
	"io"
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

// safe adapts fn as a time.AfterFunc callback that runs on the goSafe
// goroutine protection chain (exact deadline fires replace tick loops).
func safe(fn func()) func() {
	return func() { goSafe(fn) }
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
		// Cache-bust by the thumbnail's mtime: a refreshed thumbnail keeps
		// its name, and images are cached for an hour (CachePolicy).
		if thumbnail != "" {
			if st, err := os.Stat(filepath.Join(config.ThumbnailLocation(), m.Filename+".jpg")); err == nil {
				thumbnail += "?v=" + strconv.FormatInt(st.ModTime().UnixNano(), 36)
			}
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
	if cue.SourceKind == "endpoint" {
		return []MediaRow{
			{Label: "Source", Value: "Live page (rendered by WPE)"},
			{Label: "URL", Value: cue.EndpointURL},
			{Label: "Video", Value: "Display size, 30 fps; no audio"},
		}
	}
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
	// A genuinely silent file is re-probed once; Reprobed is persisted so it is not probed again.
	isAV := strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "audio/")
	if isAV && info.Audio == nil && !info.Reprobed {
		if meta, perr := media.Probe(filepath.Join(config.MediaLocation(), cue.Filename)); perr == nil && meta.Info != nil {
			info = *meta.Info
			info.Reprobed = true
			unmarshalErr = false
			if raw, jerr := json.Marshal(info); jerr == nil {
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
	// Stills get a curated set: raw probe values read as broken on images
	// (container "png_pipe", a bogus "25 fps" frame rate the format implies
	// but a still never has), so show what an operator can use instead.
	if strings.HasPrefix(cue.Mimetype, "image/") {
		return imageInfoRows(cue, info)
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

// imageInfoRows is the Media tab for stills: type, resolution, pixel
// format and file size — never container internals, frame rates or
// bitrates, which are meaningless (or actively wrong) for a still.
func imageInfoRows(cue ctp.Cue, info media.MediaInfo) []MediaRow {
	rows := make([]MediaRow, 0, 4)
	add := func(label, value string) {
		if value != "" && value != "0" {
			rows = append(rows, MediaRow{Label: label, Value: value})
		}
	}
	codec := cue.MediaType
	res, pixFmt := cue.Resolution, ""
	if v := info.Video; v != nil {
		if v.Codec != "" {
			codec = v.Codec
		}
		if v.Width > 0 && v.Height > 0 {
			res = fmt.Sprintf("%d x %d", v.Width, v.Height)
		}
		pixFmt = v.PixFmt
	}
	add("Type", friendlyImageKind(codec))
	add("Resolution", res)
	add("Pixel format", pixFmt)
	if cue.Size > 0 {
		rows = append(rows, MediaRow{Label: "File size", Value: formatSize(cue.Size)})
	}
	if len(rows) == 0 {
		return []MediaRow{
			{Label: "Type", Value: cue.Mimetype},
			{Label: "Codec", Value: cue.MediaType},
			{Label: "Resolution", Value: cue.Resolution},
		}
	}
	return rows
}

// friendlyImageKind turns a still codec ("png", "mjpeg") into operator
// words ("PNG image", "JPEG image").
func friendlyImageKind(codec string) string {
	switch strings.ToLower(codec) {
	case "png":
		return "PNG image"
	case "mjpeg", "jpeg", "jpg":
		return "JPEG image"
	case "gif":
		return "GIF image"
	case "bmp":
		return "BMP image"
	case "webp":
		return "WebP image"
	case "":
		return "Image"
	default:
		return strings.ToUpper(codec) + " image"
	}
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
		"AudioDevice":   config.Audio().Device,
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

// TypeIcon maps a media kind to an ftl-themes icon-pack symbol id
// (assets/icons/icons.svg#icon-<id>); the templates render it as
// <svg class="icon"><use/></svg>. The pack ships per-theme overrides,
// so the same markup reterms under every shared theme.
func TypeIcon(kind string) string {
	switch kind {
	case "video":
		return "video"
	case "audio":
		return "music-note"
	case "image":
		return "image"
	case "endpoint":
		return "clock"
	default:
		return "file"
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
		// Dead-reckoning flag for the widget clock: the client advances
		// the displayed time from PositionRaw while playing, so the timer
		// tracks real time instead of lagging a server round trip behind.
		"Playing": gsp.CurrentPlaying() != "" && !gsp.IsPaused(),
		"Paused":  gsp.CurrentPlaying() != "" && gsp.IsPaused(),
		// Tests button state for every client (§12.10).
		"TestShowing": gsp.TestShowing(),
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
				cuesheet.Cues[i].WaitPct = waitPct(w, time.Now().UnixMilli())
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

// armNextCue prerolls the next cue into gsp's warm slot (deck-style double
// buffer). Video/image cues preroll on fakesink — silent and unpainted, so
// they no longer need an audio-only gate. Audio prerolls on the real audio
// sink, silent until it plays. Arming waits ~1.2s so the just-fired cue's
// own decode settles and never competes for CPU mid-fade; a stale arm
// (generation moved) is dropped by gsp.Warm itself.
func armNextCue(gen uint64, pos int) {
	if pos <= 0 {
		return
	}
	next, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		return
	}
	if next.SourceKind == "endpoint" {
		return
	}
	goSafe(func() {
		time.Sleep(1200 * time.Millisecond)
		if gsp.Generation() != gen {
			return
		}
		armStarted := time.Now()
		// Stills never warm: a single frame prerolls to instant EOS and the
		// activation flush-seek never re-prerolls it (5 s stall, then
		// teardown). Cold stills load in ~300 ms anyway — warm buys nothing.
		if strings.HasPrefix(next.Mimetype, "image/") {
			return
		}
		if err := gsp.Warm(next.Filename, cueOpts(next, false)); err != nil {
			logs.Printf(logs.GSPWarm, "prewarm %q: %v", next.Filename, err)
		} else {
			logs.Printf(logs.GSPWarm, "prewarmed %q in %.0fms", next.Filename, time.Since(armStarted).Seconds()*1000)
		}
	})
}

// loadAndPlayCue builds LoadOpts for a cue and plays it. Shared by the cue
// transport and automation (auto-continue, queue-after-fade). keepBackground
// is only true for slideshow image slides: they ride ON the soundtrack
// instead of restarting it.
// play route and the fade-then-play path.
// cueOpts maps a cue row onto the playback options the pipeline honours.
// Single source of truth for every transport that fires a cue (HTTP, remote
// protocols, auto-continue chains, the scheduler, preload arming).
func cueOpts(cue ctp.Cue, keepBackground bool) gsp.LoadOpts {
	return gsp.LoadOpts{
		CuePos:   cue.CuePos,
		InPoint:  float64(cue.PosStart) / 1000,
		OutPoint: float64(cue.PosEnd) / 1000,
		Hold: cue.Hold && (strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "image/")) ||
			// A blank display duration means indefinitely: with no timer to
			// end it and no hold to park it, a still would EOS-teardown on
			// its first frame (the inspector promises "blank = indefinitely").
			(strings.HasPrefix(cue.Mimetype, "image/") && cue.CueDuration == 0) ||
			// An animated image shorter than its display duration stays on
			// its last frame until the timer ends the cue, not black (§6.1.3).
			(strings.HasPrefix(cue.Mimetype, "image/") && gsp.IsAnimated(cue.Filename)),
		Loop:           cue.Loop,
		LoopCount:      cue.LoopCount,
		Volume:         cue.Volume,
		LoudnessGain:   cue.LoudnessGain,
		Rate:           cue.Rate,
		Balance:        cue.Balance,
		Mute:           cue.Mute,
		FadeIn:         cue.FadeIn,
		FadeCurve:      cue.FadeCurve,
		FitMode:        cue.FitMode,
		Rotation:       cue.Rotation,
		Flip:           cue.Flip,
		KeepBackground: keepBackground,
		WarmPreroll:    true,
		// Fade-stop others (§6.5): this cue starts at once and whatever is
		// on screen fades out over it for FadeOut ms.
		Crossfade: cue.FadeOut,
		Opacity:   cue.Opacity / 100,
		GeomX:     cue.GeomX,
		GeomY:     cue.GeomY,
		GeomW:     cue.GeomW,
		GeomH:     cue.GeomH,
		CropL:     cue.CropL,
		CropR:     cue.CropR,
		CropT:     cue.CropT,
		CropB:     cue.CropB,
	}
}

func loadAndPlayCue(cue ctp.Cue) error {
	return loadAndPlayCueKeep(cue, false)
}

// loadCueSource puts a cue's source on the wall with opts: its media file,
// or for a live page the WPE renderer (plus the availability watch that
// swaps in the panic image if the page goes away). Every transport that
// fires a cue (GO, slideshow, Awards, the scheduler) loads through here so
// a live cue never reaches the file loader.
func loadCueSource(cue ctp.Cue, opts gsp.LoadOpts) error {
	if cue.SourceKind != "endpoint" {
		return gsp.LoadWithOpts(cue.Filename, opts)
	}
	if err := gsp.LoadEndpointWithOpts(liveLabel(cue), cue.EndpointURL, opts); err != nil {
		return err
	}
	watchEndpointAvailability(cue, gsp.Generation())
	return nil
}

// liveLabel is what Now Playing shows for a live cue ("Live: <label>").
func liveLabel(cue ctp.Cue) string {
	if cue.Title != "" {
		return cue.Title
	}
	return cue.EndpointTitle
}

func loadAndPlayCueKeep(cue ctp.Cue, keepBackground bool) error {
	if err := loadCueSource(cue, cueOpts(cue, keepBackground)); err != nil {
		ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
		return err
	}
	ctp.SetCueResult(cue.CuePos, ctp.CueResultOK) // fired; cleared to error on end-hook trouble
	gsp.SetCuePos(cue.CuePos)
	gsp.Play()
	logs.Emit(logs.AuditEvent{Event: "cue_start", Pos: cue.CuePos, Title: cue.Title})
	// A set display duration ends the still on a timer (then
	// auto-continues like any other end). A blank duration holds the frame
	// indefinitely instead (cueOpts forces hold): without that, the still
	// would EOS-teardown on its first frame. The generation guard drops
	// stale timers when the operator acts meanwhile.
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

// watchEndpointAvailability checks that the page remains reachable. The
// TimerPi page owns its WebSocket/fallback update behavior; this probe only
// detects a dead or unreachable HTTP page (WPE would otherwise just render
// its own error page on the wall) so CuTePi can show its fallback.
func watchEndpointAvailability(cue ctp.Cue, generation uint64) {
	goSafe(func() {
		mark := markTransport()
		delay := 2 * time.Second
		for {
			time.Sleep(delay)
			delay = 5 * time.Second
			if gsp.Generation() != generation || !mark.unchanged() {
				return // replaced, stopped or panicked: nothing to watch
			}
			if err := probeEndpoint(cue.EndpointURL); err != nil {
				logs.PrintfWarn(logs.GSPPipeStopped, "live page %q unavailable: %v", cue.EndpointURL, err)
				gsp.FailEndpoint(generation)
				return
			}
		}
	})
}

// probeEndpoint reports whether a live page answers with a non-error status.
func probeEndpoint(pageURL string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(pageURL)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// recoverLiveCue runs when the live cue at pos (fired as generation) loses
// its page: mark the cue failed, show the panic holding image (black when
// none is set), and bring the page back once it answers again — retrying
// with backoff from 1s to 30s, and only while no operator action (Stop,
// Panic, another cue) has superseded the fallback.
func recoverLiveCue(pos int, _ string, generation uint64) {
	if pos == 0 || gsp.Generation() != generation {
		return
	}
	ctp.SetCueResult(pos, ctp.CueResultError)
	if cue, err := ctp.GetCue(strconv.Itoa(pos)); err != nil || cue.SourceKind != "endpoint" {
		return
	}
	mark := showLiveFallback()
	delay := time.Second
	var attempt func()
	attempt = func() {
		if !mark.unchanged() {
			return // the operator moved on: Stop, Panic or another cue
		}
		next := func() {
			delay = min(delay*2, 30*time.Second)
			time.AfterFunc(delay, safe(attempt))
		}
		fresh, err := ctp.GetCue(strconv.Itoa(pos))
		if err != nil || fresh.SourceKind != "endpoint" {
			return // cue deleted or replaced meanwhile
		}
		// Reload only once the page answers: WPE renders a dead page as its
		// own error screen, which must never replace the fallback.
		if probeEndpoint(fresh.EndpointURL) != nil {
			next()
			return
		}
		if err := loadAndPlayCueKeep(fresh, false); err != nil {
			logs.PrintfWarn(logs.GSPPipeStopped, "live page %q: reload failed: %v", fresh.EndpointURL, err)
			mark = showLiveFallback()
			next()
			return
		}
		logs.Printf(logs.GSPPipeStopped, "live page %q back on air", fresh.EndpointURL)
	}
	time.AfterFunc(delay, safe(attempt))
}

// showLiveFallback cuts to the panic holding image exactly as Panic does
// (the armed standby, else a fresh load, else black) and returns a mark of
// the transport state it left.
func showLiveFallback() transportMark {
	_ = remotePanic()
	return markTransport()
}

// transportMark snapshots the operator-decision counters. Generation is not
// enough here: Stop and Panic do not move it, and an operator Stop during an
// outage must cancel the live page's recovery.
type transportMark struct{ halts, loads uint64 }

func markTransport() transportMark { return transportMark{gsp.Halts(), gsp.Loads()} }

func (m transportMark) unchanged() bool { return m == markTransport() }

// waitState is the live wait countdown (§12.2): which cue is waiting, what
// kind of wait, and when it ends (unix ms). The auto-continue chain writes
// it from 250ms ticks; the cuesheet render and the per-second version bump
// carry it to clients, so a hung cue never masquerades as a deliberate wait.
type waitState struct {
	CuePos     int
	Kind       string // "pre" | "post"
	EndsAt     int64  // unix ms: when the chain fires
	PhaseStart int64  // unix ms: this wait phase's start (progress fill)
	PhaseEnd   int64  // unix ms: this wait phase's end
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

// waitPct is how far through its current phase a wait is (0..100), for the
// progress fill behind the PreWait/PostWait numerals.
func waitPct(w waitState, now int64) int {
	span := w.PhaseEnd - w.PhaseStart
	if span <= 0 {
		return 100
	}
	pct := (now - w.PhaseStart) * 100 / span
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return int(pct)
}

// CurrentWait returns the active wait (zero value when none).
func CurrentWait() waitState {
	waitMu.Lock()
	defer waitMu.Unlock()
	return waitNow
}

// autoContinueFrom implements per-cue auto-continue: a cue flagged
// autoContinue plays the next cue in sheet order after it ends. The ending
// cue's postWait and the next cue's preWait pause the chain (waits are only
// meaningful between auto-continuing cues). The whole advance is skipped if
// the operator does anything else during waits, so a triggered cue can never
// interrupt a newer decision.
func autoContinueFrom(endingPos int) {
	// Awards sessions never chain: a member with AutoContinue set still
	// waits for the operator's next GO.
	if awardsSuppressAutoContinue(endingPos) {
		return
	}
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
	// Single absolute deadline: the cue's postWait plus (when the next cue
	// itself auto-continues) the next cue's preWait. The old shape waited
	// them as two 250ms tick loops, so a 5s wait could land 250ms late per
	// leg; now the tick loop is display-only and one exact timer fires.
	post := time.Duration(cue.PostWait) * time.Millisecond
	var pre time.Duration
	if nextCue.AutoContinue {
		pre = time.Duration(nextCue.PreWait) * time.Millisecond
	}
	gen := gsp.Generation()
	fire := func() {
		// Generation guard: fires only if the playback decision state is
		// unchanged since arming. Any operator action during the wait (load,
		// stop, panic — including re-triggering the SAME cue, which leaves
		// cuePos equal but bumps the generation) disarms the chain, so a
		// triggered cue can never interrupt a newer decision.
		if gsp.Generation() != gen {
			return
		}
		// Re-fetch: the operator may have edited the next cue during the
		// waits; the wait-time snapshot would play stale values.
		if fresh, ferr := ctp.GetCue(strconv.Itoa(next)); ferr == nil {
			nextCue = fresh
		}
		if lerr := loadAndPlayCue(nextCue); lerr != nil {
			log.Printf("auto-continue: loading next cue %d failed: %v", next, lerr)
			ctp.SetCueResult(next, ctp.CueResultError)
			return
		}
		_ = ctp.SetCue(strconv.Itoa(next))
		awardsSelectionSync()
		// Preload whatever follows the chained cue (audio-only; see
		// armNextCue) with the same GO-instantly contract the operator has.
		if nn, nerr := ctp.NextCuePos(next); nerr == nil && nn != 0 {
			armNextCue(gsp.Generation(), nn)
		}
	}
	if post+pre <= 0 {
		fire()
		return
	}
	fireAt := time.Now().Add(post + pre)
	goSafe(func() {
		// Exact fire: a timer armed to the absolute deadline.
		time.AfterFunc(post+pre, safe(fire))
		// Countdown display only: a live WHAT-is-waiting state (§12.2) for
		// the pills, stepping post→pre at the phase boundary. Never fires.
		postStart := time.Now()
		preStart := fireAt.Add(-pre)
		setWait(waitState{CuePos: endingPos, Kind: "post", EndsAt: fireAt.UnixMilli(),
			PhaseStart: postStart.UnixMilli(), PhaseEnd: preStart.UnixMilli()})
		defer clearWait()
		lastBump := time.Time{}
		for {
			time.Sleep(250 * time.Millisecond)
			if gsp.Generation() != gen {
				return
			}
			remaining := time.Until(fireAt)
			if remaining <= 0 {
				return
			}
			if remaining < pre {
				setWait(waitState{CuePos: next, Kind: "pre", EndsAt: fireAt.UnixMilli(),
					PhaseStart: preStart.UnixMilli(), PhaseEnd: fireAt.UnixMilli()})
			}
			if time.Since(lastBump) >= time.Second {
				lastBump = time.Now()
				ctp.NotifyCuesheetChanged()
			}
		}
	})
}

// renderCuesheet fetches the cuesheet, tags the currently-playing cue, and
// renders the partial. Shared by every action that returns the cuesheet HTML.
func renderCuesheet(c *gin.Context) {
	cuesheet, err := ctp.GetCuesheet()
	if err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	enrichCuesheetWithPlayback(&cuesheet)
	c.HTML(http.StatusOK, "cuesheet.html", gin.H{
		"Cuesheet":  cuesheet,
		"Rows":      sheetRowsWithSelection(&cuesheet),
		"GoBar":     computeGoBar(&cuesheet),
		"GoAdvance": ctp.GetGoAdvance(),
	})
}

func Index(rg *gin.RouterGroup) {
	gsp.SetEndpointFailureHook(recoverLiveCue)
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}

		cuesheet, err := ctp.GetCuesheet()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
		if gid, gerr := ctp.SelectedGroupPos(); gerr == nil && gid > 0 {
			if gi, err := groupInspectorData(gid); err == nil {
				data["GroupInspector"] = gi
			}
		}
		data["ShowMode"] = ctp.GetShowMode()
		data["title"] = "CuTePi"
		c.HTML(http.StatusOK, "index.html", data)
	})

	// Standalone mediapool partial endpoint. Used to re-render the panel after
	// a media item is deleted or a direct fetch refresh is wanted.
	rg.GET("/mediapool", func(c *gin.Context) {
		mediapool, err := mediapoolView()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{
			"Mediapool": mediapool,
		})
	})
}

// importMedia probes, verifies playability, moves srcPath into the media
// directory under filename, and registers it. The single shared import path
// for uploads, youtube downloads, and .CTP show imports — every caller must
// reject a source that fails probe/verify/registration before it lands in
// the pool. On failure srcPath is removed and the media directory is left
// exactly as it was: a file being replaced (same name re-uploaded) is parked
// aside first and restored if the move or registration fails, so live cues
// never lose their source to a failed import.
//
// The returned error is user-facing (upload, youtube and show-import
// responses): the staging path is rewritten to filename and server
// directories are stripped, so no temp/media path leaks (O5).
func importMedia(filename, srcPath string) (err error) {
	defer func() { err = importError(err, filename, srcPath) }()
	if !safeMediaName(filename) {
		os.Remove(srcPath)
		return fmt.Errorf("invalid filename %q", filename)
	}
	// Replacing the file under the running clip would swap its source
	// mid-show (delete and rename refuse this too).
	if gsp.CurrentPlaying() == filename && mediaFileExists(filename) {
		os.Remove(srcPath)
		return fmt.Errorf("can't replace the clip that is playing")
	}
	meta, err := media.Probe(srcPath)
	if err != nil {
		os.Remove(srcPath)
		return err
	}
	// The playback engine must decode it, not just ffmpeg (§5.7): ffprobe
	// reads formats this system's GStreamer may lack (ASF, AVIF, JPEG XL).
	if err := gsp.CheckDecodable(srcPath); err != nil {
		os.Remove(srcPath)
		return fmt.Errorf("cannot be played: %w", err)
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

	backup := ""
	if srcPath != destPath {
		if _, err := os.Lstat(destPath); err == nil {
			backup = fmt.Sprintf("%s.cutepi-replaced-%d", destPath, time.Now().UnixNano())
			if err := os.Rename(destPath, backup); err != nil {
				os.Remove(srcPath)
				return fmt.Errorf("setting aside existing %q: %w", filename, err)
			}
		}
		if err := moveIntoPlace(srcPath, destPath); err != nil {
			os.Remove(srcPath)
			os.Remove(destPath) // a partial cross-device copy
			restoreReplaced(backup, destPath)
			return err
		}
	}
	if err := ctp.RegisterMedia(filename, info.Size(), meta, title); err != nil {
		os.Remove(destPath)
		restoreReplaced(backup, destPath)
		return err
	}
	if backup != "" {
		if err := os.Remove(backup); err != nil {
			logs.PrintfWarn("IMPORT", "removing replaced copy %q: %v", backup, err)
		}
	}
	return nil
}

// restoreReplaced moves a set-aside original back over dest (no-op when
// nothing was replaced).
func restoreReplaced(backup, dest string) {
	if backup == "" {
		return
	}
	if err := os.Rename(backup, dest); err != nil {
		logs.PrintfWarn("IMPORT", "restoring %q from %q: %v", dest, backup, err)
	}
}
