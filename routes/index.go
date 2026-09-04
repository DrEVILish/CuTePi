package routes

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/gsp"
)

type MediapoolItem struct {
	Filename  string
	Size      string
	Mimetype  string
	Thumbnail string
	DateAdded string
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
		})
	}
	return mediapool, nil
}

// nowplayingData is the template data for the mediainfo.html partial, shared
// by the index page (initial server render) and GET /api/nowplaying (the full
// re-render triggered by the change-detection poller). The Filename/Position/
// Duration keys keep the widget accurate from the very first paint, before the
// first poll.
// cuePalette is the fixed set of 12 named cue colours offered in the inspector
// dropdown. Kept as an ordered map so the template can present names with hexes.
var cuePalette = map[string]string{
	"Red":    "#dc3545",
	"Orange": "#fd7e14",
	"Yellow": "#ffc107",
	"Green":  "#28a745",
	"Teal":   "#20c997",
	"Cyan":   "#17a2b8",
	"Blue":   "#007bff",
	"Indigo": "#6610f2",
	"Purple": "#a855f7",
	"Pink":   "#e83e8c",
	"Brown":  "#795548",
	"Grey":   "#6c757d",
}

func inspectorData() gin.H {
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		return gin.H{"Cue": ctp.Cue{}, "Selected": false, "MediaDuration": 0, "Palette": cuePalette}
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		return gin.H{"Cue": ctp.Cue{}, "Selected": false, "MediaDuration": 0, "Palette": cuePalette}
	}
	return gin.H{
		"Cue":           cue,
		"Selected":      true,
		"MediaDuration": cue.Duration,
		"Palette":       cuePalette,
	}
}

// typeIcon maps the cached MediaType kind to a Bootstrap icon class used for
// the cue row's media-type glyph.
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
	return gin.H{
		"Filename":    gsp.CurrentPlaying(),
		"Position":    formatClock(pos),
		"PositionRaw": pos,
		"Duration":    formatClock(gsp.CurrentDuration()),
		"DurationRaw": gsp.CurrentDuration(),
	}
}

// enrichCuesheetWithPlayback marks the currently-playing cue (by filename,
// matching the active gsp pipeline) and fills its per-cue progress-bar data
// (PlayPos/PlayDur in ms). Only one cue plays at a time.
func enrichCuesheetWithPlayback(cuesheet *ctp.Cuesheet) {
	cur := gsp.CurrentPlaying()
	if cur == "" {
		return
	}
	pos := gsp.CurrentPosition()
	dur := gsp.CurrentDuration()
	for i := range cuesheet.Cues {
		if cuesheet.Cues[i].Filename == cur {
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
		InPoint:  float64(cue.PosStart) / 1000,
		OutPoint: float64(cue.PosEnd) / 1000,
		Hold:     cue.Hold && (strings.HasPrefix(cue.Mimetype, "video/") || strings.HasPrefix(cue.Mimetype, "image/")),
		Volume:   cue.Volume,
	}
	if err := gsp.LoadWithOpts(cue.Filename, opts); err != nil {
		return err
	}
	gsp.SetCuePos(cue.CuePos)
	gsp.Play()
	return nil
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
	c.HTML(http.StatusOK, "cuesheet.html", gin.H{"Cuesheet": cuesheet})
}

func Index(rg *gin.RouterGroup) {
	// AutoFollow: when a cue reaches its end (and its autoFollow flag is set),
	// advance the server-side selection to the next cue. Registered once at
	// startup, before any route serves traffic.
	gsp.SetCueEndHook(func(pos int) { ctp.AutoFollowSelect(pos) })

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
		data["Inspector"] = inspectorData()
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
