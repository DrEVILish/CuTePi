package routes

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"
	"CuTePi/ws"
	qrcode "github.com/skip2/go-qrcode"
)

// RestartEnv marks a process spawned by /api/restart to take over this
// server's port after the parent exits.
const RestartEnv = "CUTEPI_RESTART_WAIT"

// restartServer and shutdownServer implement POST /api/restart and
// /api/shutdown. They are package-level so tests can swap in harmless
// stand-ins (invoking the real ones would kill the test process).
var (
	restartServer  = restartProcess
	shutdownServer = shutdownProcess
	startTime      = time.Now()
)

func mustInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func intOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// splitHhMm parses an "HH:MM" string into hour and minute integers.
func splitHhMm(v string) (int, int, error) {
	hh, mm, _, err := splitHhMmSs(v)
	return hh, mm, err
}

// splitHhMmSs parses "HH:MM" or "HH:MM:SS" (the time input's step=1 value)
// into hour, minute and second integers. Seconds default to 0.
func splitHhMmSs(v string) (int, int, int, error) {
	var hh, mm, ss int
	var err error
	if len(v) == 5 && v[2] == ':' {
		if hh, err = strconv.Atoi(v[:2]); err != nil || hh < 0 || hh > 23 {
			return 0, 0, 0, fmt.Errorf("invalid hour")
		}
		if mm, err = strconv.Atoi(v[3:]); err != nil || mm < 0 || mm > 59 {
			return 0, 0, 0, fmt.Errorf("invalid minute")
		}
		return hh, mm, 0, nil
	}
	if len(v) == 8 && v[2] == ':' && v[5] == ':' {
		if hh, err = strconv.Atoi(v[:2]); err != nil || hh < 0 || hh > 23 {
			return 0, 0, 0, fmt.Errorf("invalid hour")
		}
		if mm, err = strconv.Atoi(v[3:5]); err != nil || mm < 0 || mm > 59 {
			return 0, 0, 0, fmt.Errorf("invalid minute")
		}
		if ss, err = strconv.Atoi(v[6:]); err != nil || ss < 0 || ss > 59 {
			return 0, 0, 0, fmt.Errorf("invalid second")
		}
		return hh, mm, ss, nil
	}
	return 0, 0, 0, fmt.Errorf("invalid HH:MM[:SS] format")
}

// builtinTestPatterns is the curated GStreamer videotestsrc set offered in
// the Tests modal (§12.10); custom pool items ride on top of it.
var builtinTestPatterns = []struct {
	Name  string
	Label string
}{
	{"smpte-rp-219", "SMPTE bars"},
	{"smpte100", "SMPTE 100"},
	{"snow", "Snow"},
	{"black", "Black"},
	{"white", "White"},
	{"red", "Red"},
	{"green", "Green"},
	{"blue", "Blue"},
	{"checkers-1", "Checkers fine"},
	{"checkers-4", "Checkers coarse"},
	{"circle", "Circle"},
	{"blink", "Blink"},
	{"solid", "Solid"},
}

// shutdownProcess terminates this process after a short grace so the HTTP
// response making the request can flush first. SIGTERM is handled in
// main.go, which closes the DB and exits cleanly.
func shutdownProcess(grace time.Duration) {
	go func() {
		time.Sleep(grace)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
}

// restartProcess restarts this server. Under systemd the correct restart is
// `systemctl restart <unit>` (the re-exec'd child would be an unmanaged orphan
// holding the port). Outside systemd it spawns a detached copy that waits for
// this process to exit (freeing the port) before starting.
func restartProcess(grace time.Duration) error {
	if unit := systemdUnit(); unit != "" {
		cmd := exec.Command("systemctl", "restart", unit)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), RestartEnv+"=1")
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	shutdownProcess(grace)
	return nil
}

// systemdUnit returns the name of the systemd unit running this process, or ""
// when not running under a unit. systemd sets INVOCATION_ID for unit processes
// and CUTEPI_SERVICE can be used to pin a non-default service name.
func systemdUnit() string {
	if os.Getenv("INVOCATION_ID") == "" {
		return ""
	}
	if s, ok := os.LookupEnv("CUTEPI_SERVICE"); ok && s != "" {
		return s
	}
	return "cutepi"
}

func Api(rg *gin.RouterGroup) {
	registerThemeRoutes(rg)
	rg.GET("/ws", func(c *gin.Context) {
		ws.Handle(c.Writer, c.Request)
	})
	// Full HTML render of the "Now Playing" widget. Used for the initial page
	// render and by the change-detection poller (public/src/ui.js) only when
	// GET /api/nowplaying/status reports a change.
	rg.GET("/nowplaying", func(c *gin.Context) {
		c.HTML(http.StatusOK, "mediainfo.html", nowplayingData())
	})

	// Lightweight change-detection endpoint polled every 500ms by the Now
	// Playing widget. Returns a monotonic server-side version counter that
	// gsp bumps on real playback state changes and position ticks, plus a
	// "changed" flag computed against the version the client last saw. The
	// widget only re-renders (via GET /api/nowplaying) when changed is true,
	// so idle/paused widgets stop being re-rendered on every poll. Per-client
	// tracking lives entirely in the client; the server keeps no per-client
	// state, so any number of concurrent clients work.
	rg.GET("/nowplaying/status", func(c *gin.Context) {
		clientVersion := c.Query("version")
		gsp.CurrentPosition()
		// The header partial carries the GO cluster too, so selection and
		// cuesheet changes must re-render it — the version string couples
		// both counters (string pair, so neither can mask the other).
		version := fmt.Sprintf("%d.%d", gsp.StateVersion(), ctp.CuesheetVersion())
		c.JSON(http.StatusOK, gin.H{
			"changed": version != clientVersion,
			"version": version,
		})
	})
	rg.GET("/cuesheet", func(c *gin.Context) {
		renderCuesheet(c)
	})
	rg.GET("/cuesheet/status", func(c *gin.Context) {
		var clientVersion uint64
		if raw := c.Query("version"); raw != "" {
			if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
				clientVersion = v
			}
		}
		version := ctp.CuesheetVersion()
		c.JSON(http.StatusOK, gin.H{"changed": version != clientVersion, "version": version})
	})

	rg.GET("/qr", func(c *gin.Context) {
		scheme := "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
		if proto := c.Request.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		host := c.Request.Host
		if host == "" {
			host = fmt.Sprintf("localhost:%d", config.Port())
		}
		target := fmt.Sprintf("%s://%s/upload", scheme, host)
		if override := c.Query("url"); override != "" {
			target = override
		}
		png, err := qrcode.Encode(target, qrcode.Medium, 256)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Data(http.StatusOK, "image/png", png)
	})

	rg.POST("/play", func(c *gin.Context) {
		logs.Printf(logs.RTEPlay, "PLAY")
		gsp.Play()
		c.Status(http.StatusOK)
	})
	// Alias used by the spacebar keyboard shortcut (see public/src/ui.js /
	// cuesheet.html's #spaceBar trigger).
	rg.POST("/cue/play", func(c *gin.Context) {
		logs.Printf(logs.RTEPlayAlias, "PLAY (spacebar)")
		gsp.Play()
		c.Status(http.StatusOK)
	})
	rg.POST("/pause", func(c *gin.Context) {
		logs.Printf(logs.RTEPause, "PAUSE")
		gsp.Pause()
		c.Status(http.StatusOK)
	})
	rg.POST("/togglePause", func(c *gin.Context) {
		logs.Printf(logs.RTEToggle, "togglePause")
		gsp.TogglePause()
		c.Status(http.StatusOK)
	})

	// Seek: move the active clip to the given absolute position (seconds).
	rg.POST("/seek", func(c *gin.Context) {
		seconds, err := strconv.ParseFloat(c.PostForm("position"), 64)
		if err != nil || math.IsNaN(seconds) {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "position must be a number of seconds"})
			return
		}
		gsp.Seek(seconds)
		c.Status(http.StatusOK)
	})

	// Loop: enable/disable loop-at-end for the current clip (also the default).
	// Volume: set the per-cue master gain of the currently-playing cue, live.
	// Persisted on the cue so it is remembered for next time.
	rg.POST("/volume", func(c *gin.Context) {
		v, err := strconv.ParseFloat(c.PostForm("volume"), 64)
		if err != nil || math.IsNaN(v) {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "volume must be a number"})
			return
		}
		applied := gsp.SetVolume(v)
		if pos := gsp.CurrentCuePos(); pos > 0 {
			_ = ctp.UpdateCue(strconv.Itoa(pos), "volume", strconv.FormatFloat(applied, 'f', -1, 64))
		}
		c.Status(http.StatusOK)
	})
	rg.POST("/fadeOut", func(c *gin.Context) {
		logs.Printf(logs.RTEFadeOut, "fadeOut")
		gsp.Stop()
		c.Status(http.StatusOK)
	})
	rg.POST("/panic", func(c *gin.Context) {
		logs.Printf(logs.RTEPanic, "!!PANIC!!")
		// Holding image (§12.9): PANIC cuts to a configured full-frame image
		// instead of dead black. Any failure falls back to a plain panic.
		if hold := ctp.GetPanicHoldImage(); hold != "" {
			if err := gsp.LoadWithOpts(hold, gsp.LoadOpts{Hold: true}); err == nil {
				c.Status(http.StatusOK)
				return
			}
			logs.Printf(logs.RTEPanic, "panic hold image %q unavailable - cutting to black", hold)
		}
		gsp.Panic()
		c.Status(http.StatusOK)
	})
	rg.POST("/clear", func(c *gin.Context) {
		logs.Printf(logs.RTEClear, "Clear CueSheet")
		err := ctp.ClearCueSheet()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})
	rg.POST("/stop", func(c *gin.Context) {
		logs.Printf(logs.RTEStop, "STOP")
		gsp.Stop()
		c.Status(http.StatusOK)
	})

	// Fade & stop the active clip over the given duration (ms); no duration
	// means "stop now". Mirrors the fade-to-black used when a subsequent cue
	// triggers with its own fadeOut set.
	rg.POST("/fade", func(c *gin.Context) {
		durMs := 0
		if raw := c.PostForm("duration"); raw != "" {
			ms, perr := strconv.Atoi(raw)
			if perr != nil || ms < 0 {
				c.String(http.StatusBadRequest, "invalid duration")
				return
			}
			durMs = ms
		}
		gsp.FadeAndStop(durMs)
		c.Status(http.StatusOK)
	})

	rg.POST("/test/*pattern", func(c *gin.Context) {
		pattern := strings.TrimPrefix(c.Param("pattern"), "/")
		if pattern == "" {
			pattern = "smpte-rp-219"
		}
		if err := gsp.ShowTest(pattern); err != nil {
			logs.Printf(logs.RTETest, "show test failed pattern=%q error=%v", pattern, err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		logs.Printf(logs.RTETest, "Show test pattern=%q", pattern)
		c.Status(http.StatusOK)
	})

	// GET /api/testpatterns: what the Tests modal lists — built-ins plus the
	// operator's pinned pool items.
	rg.GET("/testpatterns", func(c *gin.Context) {
		builtin := make([]gin.H, 0, len(builtinTestPatterns))
		for _, p := range builtinTestPatterns {
			builtin = append(builtin, gin.H{"name": p.Name, "label": p.Label})
		}
		c.JSON(http.StatusOK, gin.H{"builtin": builtin, "custom": ctp.TestPatterns()})
	})

	// Pin/unpin a pool item as a custom test pattern (media tile menu).
	rg.POST("/testpattern/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		on := strings.TrimSpace(c.PostForm("on")) == "1"
		var err error
		if on {
			err = ctp.AddTestPattern(filename)
		} else {
			err = ctp.RemoveTestPattern(filename)
		}
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Direct Play from Mediapool
	rg.POST("/play/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		if ok, merr := ctp.MediaRegistered(filename); merr != nil || !ok {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		logs.Printf(logs.RTEDirect, "Direct Play%s", filename)
		gain, err := ctp.MediaLoudnessGain(filename)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		if err := gsp.LoadWithOpts(filename, gsp.LoadOpts{Loop: config.Loop(), LoudnessGain: gain}); err != nil {
			logs.Printf(logs.RTEDirect, "direct play failed filename=%q error=%v", filename, err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		gsp.Play()
		c.Status(http.StatusOK)
	})

	// Load from Mediapool (loads the file into the pipeline without
	// necessarily playing). Referenced by the MediaPool dropdown "Load"
	// action.
	rg.POST("/load/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		if ok, merr := ctp.MediaRegistered(filename); merr != nil || !ok {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		logs.Printf(logs.RTELoad, "Load%s", filename)
		gain, err := ctp.MediaLoudnessGain(filename)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		if err := gsp.LoadWithOpts(filename, gsp.LoadOpts{Loop: config.Loop(), LoudnessGain: gain}); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

	addCue := func(c *gin.Context) {
		filename := c.Param("filename")
		cuePos := c.Param("cuePos")
		cuePos = strings.Trim(cuePos, "/")

		logs.Printf(logs.RTEAddCue, "add cue filename=%q position=%q", filename, cuePos)
		// ?group=<id> adds straight into the group (media drop onto a
		// header means JOIN); &first=1 seats it as the first member.
		var err error
		if gid, gerr := strconv.Atoi(c.Query("group")); gerr == nil && gid != 0 {
			err = ctp.AddCueToGroup(filename, gid, c.Query("first") == "1")
		} else {
			err = ctp.AddCue(filename, cuePos)
		}
		if err != nil {
			logs.Printf(logs.RTEAddCue, "add cue failed filename=%q position=%q error=%v", filename, cuePos, err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	}
	rg.POST("/cue/add/:filename", addCue)
	rg.POST("/cue/add/:filename/*cuePos", addCue)

	rg.DELETE("/media/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		if ok, merr := ctp.MediaRegistered(filename); merr != nil || !ok {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		logs.Printf(logs.RTEDelete, "Delete%s", filename)

		if gsp.CurrentPlaying() == filename {
			logs.Printf(logs.RTEDeleteBusy, "Can't delete currently playing file")
			c.Status(http.StatusConflict)
			return
		}
		err := ctp.Delete(filename)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		os.Remove(filepath.Join(config.MediaLocation(), filename))
		os.Remove(filepath.Join(config.ThumbnailLocation(), filename+".jpg"))

		// Re-render the mediapool so the deleted tile is removed from the DOM.
		// Without a body the hx-target/hx-swap on the delete dropdown item
		// would remove the wrong element (the <li> the button lives in) or
		// leave a stale tile.
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

	// Flags a media item's thumbnail for regeneration; picked up by the
	// background thumbnail worker.
	rg.POST("/media/:filename/refreshThumbnail", func(c *gin.Context) {
		filename := c.Param("filename")
		if err := ctp.RequestThumbnailRefresh(filename); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

	// Requests (re)analysis of a media file's waveform (amplitude peaks used
	// by the Cue Inspector trim timeline). Picked up by the background worker.
	rg.POST("/media/:filename/analyse", func(c *gin.Context) {
		filename := c.Param("filename")
		if err := ctp.RequestWaveformAnalysis(filename); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

	// Peak envelope for exactly one [from,to) window of a media file, at the
	// requested bucket count. The trim timeline falls back to this when the
	// stored envelope is too coarse to fill the screen at the current zoom.
	rg.GET("/media/:filename/wave", func(c *gin.Context) {
		filename := filepath.Base(c.Param("filename"))
		from, ferr := strconv.ParseFloat(c.Query("from"), 64)
		to, terr := strconv.ParseFloat(c.Query("to"), 64)
		bins, berr := strconv.Atoi(c.Query("bins"))
		if ferr != nil || terr != nil || berr != nil || !(to > from) {
			c.String(http.StatusBadRequest, "from/to/bins required")
			return
		}
		if bins < 16 || bins > media.MaxWaveformBins {
			c.String(http.StatusBadRequest, "bins out of range")
			return
		}
		src := filepath.Join(config.MediaLocation(), filename)
		if _, err := os.Stat(src); err != nil {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		peaks, err := media.GeneratePeaksWindow(src, from, to, bins)
		if err != nil {
			logs.PrintfWarn("WAVE", "window %v-%v of %s: %v", from, to, filename, err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, peaks)
	})

	// Header connection tooltip (broadcast-pin hover): server + client facts.
	rg.GET("/serverinfo", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"clients":     ws.ClientCount(),
			"live":        ws.ClientCount() > 0,
			"uptimeS":     int(time.Since(startTime).Seconds()),
			"pollMs":      config.PollInterval(),
		})
	})

	// GO-advance toggle (GO bar checkbox): persisted in state, applies to
	// the next GO. Unchecked checkboxes post nothing, so an absent field
	// means "off".
	rg.POST("/setting/goadvance", func(c *gin.Context) {
		if err := ctp.SetGoAdvance(c.PostForm("advance") != ""); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Edit/Show mode toggle (footer): Show mode arms scheduled triggers and
	// test-pattern output. Empty field means Edit.
	rg.POST("/setting/showmode", func(c *gin.Context) {
		if err := ctp.SetShowMode(c.PostForm("showmode") != ""); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Clear every cue's health result (§12.3) — pre-show reset.
	rg.POST("/cue/clearresults", func(c *gin.Context) {
		if err := ctp.ClearCueResults(); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		renderCuesheet(c)
	})

	// Ctrl+A: select every rendered (visible) cue.
	rg.POST("/cue/selectall", func(c *gin.Context) {
		sheet, err := ctp.GetCuesheet()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		rows := ctp.FlattenSheet(&sheet)
		anchor, _ := ctp.SelectedCuePos()
		set := make([]int, 0, len(rows))
		for _, r := range rows {
			if r.Cue != nil && r.Cue.CuePos != anchor {
				set = append(set, r.Cue.CuePos)
			}
		}
		if anchor == 0 && len(set) > 0 {
			anchor = set[0]
			set = set[1:]
		}
		if err := ctp.SetSelection(anchor, set); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Bulk edit (§12.4): one operation over many cues, one transaction.
	rg.POST("/cue/bulk", func(c *gin.Context) {
		var body struct {
			Op        string `json:"op"`
			Value     string `json:"value"`
			Positions []int  `json:"positions"`
			Groups    []int  `json:"groups"` // selected group blocks (§12.4 delete)
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid bulk payload: "+err.Error())
			return
		}
		if err := ctp.BulkEdit(body.Op, body.Value, body.Positions); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		// Deleting whole groups (§5.4): folder, subgroups and every member
		// cue. Runs after the cue op so one request covers a mixed set.
		for _, gid := range body.Groups {
			if err := ctp.DeleteGroupWithCues(gid); err != nil {
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
				return
			}
		}
		renderCuesheet(c)
	})

	// Bulk "move selection into a NEW group" (§12.4).
	rg.POST("/cue/bulkgroupnew", func(c *gin.Context) {
		var body struct {
			Positions []int `json:"positions"`
			At        int   `json:"at"` // right-clicked cue: the new group takes its slot
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid payload: "+err.Error())
			return
		}
		if _, err := ctp.BulkGroupNewAt(body.Positions, body.At); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// The one drag-drop endpoint (§5.4): a literal move to the gap the drop
	// line showed. Membership is the hovered band's — nothing inferred
	// (§5.4): the client sends parent (group id or 0) and, for member-slot
	// drops, after = the cue the line sits under.
	rg.POST("/sheet/drop", func(c *gin.Context) {
		var body struct {
			Cues       []int  `json:"cues"`
			Group      *int   `json:"group"`
			BeforeKind string `json:"beforeKind"` // "cue" | "group" | "end"
			BeforeID   int    `json:"beforeId"`
			Join       bool   `json:"join"`
			ForceTop   bool   `json:"forceTop"` // gap drop on a group boundary: stay top-level
			JoinFirst  bool   `json:"joinFirst"` // expanded-header drop: first cue in group (§5.4)
			Parent     *int   `json:"parent"`    // explicit band membership (§5.4)
			After      int    `json:"after"`     // member-slot drop: insert after this cue
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid drop payload: "+err.Error())
			return
		}
		if err := ctp.SheetDrop(body.Cues, intOrZero(body.Group), body.BeforeKind, body.BeforeID, body.Join, body.ForceTop, body.JoinFirst, body.Parent, body.After); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Next scheduled auto-fire for the GO flash: schedules only fire in
	// Show mode, so Edit mode reports disarmed and the button stays calm.
	rg.GET("/schedule/next", func(c *gin.Context) {
		if !ctp.GetShowMode() {
			c.JSON(http.StatusOK, gin.H{"armed": false})
			return
		}
		dueIn, num, title, ok := ctp.NextSchedule(time.Now())
		if !ok {
			c.JSON(http.StatusOK, gin.H{"armed": true, "none": true})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"armed":    true,
			"dueInSec": int(dueIn.Seconds()),
			"num":      num,
			"title":    title,
		})
	})

	// Renumber every cue 5, 10, 15… in sheet order (§12.5) — explicit
	// operator action, rewrites hand-set numbers.
	rg.POST("/cue/renumber", func(c *gin.Context) {
		if err := ctp.RenumberCues(); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		renderCuesheet(c)
	})

	// Auto-numbering toggle (§12.5).
	rg.POST("/setting/autonumber", func(c *gin.Context) {
		if err := ctp.SetAutoNumber(c.PostForm("on") != ""); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Panic holding image (§12.9): filename from the media pool; empty
	// clears. Set from a tile's context menu or the Settings modal.
	rg.POST("/setting/panichold", func(c *gin.Context) {
		filename := strings.TrimSpace(c.PostForm("filename"))
		if filename != "" {
			if ok, err := ctp.MediaRegistered(filename); err != nil || !ok {
				c.String(http.StatusBadRequest, "media %q is not in the pool", filename)
				return
			}
		}
		if err := ctp.SetPanicHoldImage(filename); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	rg.GET("/settings", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"port":         config.Port(),
			"pollInterval": config.PollInterval(),
			"loop":         gsp.Loop(),
			"authEnabled":  config.HasAuth(), // never return the password itself
			"panicHold":    ctp.GetPanicHoldImage(),
			"autoNumber":   ctp.GetAutoNumber(),
			"goAdvance":    ctp.GetGoAdvance(),
			"showMode":     ctp.GetShowMode(),
		})
	})

	rg.POST("/settings", func(c *gin.Context) {
		var body struct {
			Port          int    `json:"port" form:"port"`
			PollInterval  int    `json:"pollInterval" form:"pollInterval"`
			Loop          *bool  `json:"loop" form:"loop"`
			Password      string `json:"password" form:"password"`
			ClearPassword bool   `json:"clearPassword" form:"clearPassword"`
		}
		if err := c.ShouldBind(&body); err != nil {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": err.Error()})
			return
		}
		if body.PollInterval > 0 {
			if err := config.SetPollInterval(body.PollInterval); err != nil {
				c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": err.Error()})
				return
			}
		}
		if body.Port > 0 {
			if err := config.SetPort(body.Port); err != nil {
				c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": err.Error()})
				return
			}
		}
		if body.Loop != nil {
			gsp.SetLoop(*body.Loop)
		}
		// A blank password means "unchanged" (forms always send the field);
		// the explicit clear checkbox disables auth.
		if body.ClearPassword {
			config.SetAuthPassword("")
		} else if pw := strings.TrimSpace(body.Password); pw != "" {
			config.SetAuthPassword(pw)
		}
		c.JSON(http.StatusOK, gin.H{
			"port":         config.Port(),
			"pollInterval": config.PollInterval(),
			"loop":         gsp.Loop(),
			"authEnabled":  config.HasAuth(),
			"message":      "Port changes require a server restart to take effect.",
		})
	})

	// Restart / Shutdown the server process itself. Both respond first (so
	// the browser gets a clean 200) and act shortly after (grace), letting
	// the response flush before the process exits.
	rg.POST("/restart", func(c *gin.Context) {
		logs.Printf(logs.RTERestart, "Server restart requested")
		if err := restartServer(300 * time.Millisecond); err != nil {
			logs.Printf(logs.RTERestart, "restart failed: %v", err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusOK)
	})
	rg.POST("/shutdown", func(c *gin.Context) {
		logs.Printf(logs.RTEShutdown, "Server shutdown requested")
		shutdownServer(300 * time.Millisecond)
		c.Status(http.StatusOK)
	})

	rg.POST("/cue/next", func(c *gin.Context) {
		logs.Printf(logs.RTECueNext, "Select Next Cue (down)")
		var err error
		if c.Query("extend") == "1" {
			err = ctp.ExtendStep(1) // Shift+Down: grow/shrink selection
		} else {
			err = ctp.SelectStep(1)
		}
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})
	rg.POST("/cue/prev", func(c *gin.Context) {
		logs.Printf(logs.RTECuePrev, "Select Prev Cue (up)")
		var err error
		if c.Query("extend") == "1" {
			err = ctp.ExtendStep(-1) // Shift+Up: grow/shrink selection
		} else {
			err = ctp.SelectStep(-1)
		}
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})

	// Play a specific cue by position
	rg.POST("/cue/:cuePos/play", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTECuePlay, "Play Cue%s", cuePos)
		cue, err := ctp.GetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		// "Fade & Stop Others Over Time": when the incoming cue carries a
		// fadeOut duration and another clip is currently up, fade that
		// outgoing clip (audio + fade-to-black) over the duration, then stop
		// it, then start this cue. A single active pipeline means the fade
		// must complete before the new clip can display, so the new cue is
		// queued until the fade finishes.
		// ponytail: peer/list/all scope is stored per cue but single-pipeline
		// playback makes the current file the only meaningful "other"; the
		// scope column is accepted for a future multi-layer output.
		if wasPlaying := gsp.CurrentPlaying() != ""; wasPlaying && cue.FadeOut > 0 {
			goSafe(func() {
				gsp.FadeAndStop(cue.FadeOut)
				// Identity guard (same shape as the slideshow runner): if the
				// operator started something else while the fade ran, the
				// queued load must not clobber their newer choice. FadeAndStop
				// leaves cuePos 0, so the non-zero case is a fresh play.
				if cur := gsp.CurrentCuePos(); cur != 0 && cur != cue.CuePos {
					return
				}
				if err := loadAndPlayCue(cue); err != nil {
					logs.Printf(logs.RTECuePlay, "queued play failed pos=%d error=%v", cue.CuePos, err)
				}
			})
			c.Status(http.StatusOK)
			return
		}
		if err := loadAndPlayCue(cue); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

	// Play the currently selected cue (what spacebar acts on). If nothing is
	// selected, fall back to resuming whatever is loaded - also the previous
	// spacebar behaviour.
	rg.POST("/cue/selected/play", func(c *gin.Context) {
		// GO fires the selected unit. A selected GROUP triggers its playlist
		// action (§6.4) — the old fallback played bare transport state and
		// silently ignored folder selections.
		if gid, gerr := ctp.SelectedGroupPos(); gerr == nil && gid > 0 {
			if g, err := ctp.GetGroup(gid); err == nil {
				playGroup(g)
				if ctp.GetGoAdvance() {
					_ = ctp.SelectStep(1)
				}
				renderCuesheet(c)
				return
			}
		}
		pos, err := ctp.SelectedCuePos()
		if err != nil || pos == 0 {
			gsp.Play()
			c.Status(http.StatusOK)
			return
		}
		cue, err := ctp.GetCue(strconv.Itoa(pos))
		if err != nil {
			gsp.Play()
			c.Status(http.StatusOK)
			return
		}
		if err := loadAndPlayCue(cue); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		if ctp.GetGoAdvance() {
			_ = ctp.SelectStep(1)
		}
		// The body is the cuesheet partial: the GO button swaps it (live
		// selection advance), while Space's hx-swap="none" trigger ignores
		// the body and picks the change up from the poller/WS.
		renderCuesheet(c)
	})

	// Cue Inspector: renders the details (trim In/Out, Hold, playback) of the
	// currently selected cue. Fetched on load and re-fetched by the client
	// whenever the cuesheet re-renders, so it always mirrors server state. It
	// is selection-dependent, so never let caches serve it for the wrong cue.
	rg.GET("/cue/inspector", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		// A selected group gets its own inspector so the auto-follow refresh
		// (and any manual fetch) shows the right panel content.
		if gid, gerr := ctp.SelectedGroupPos(); gerr == nil && gid != 0 {
			renderGroupInspector(c, gid)
			return
		}
		c.HTML(http.StatusOK, "cueinspector.html", inspectorData())
	})

	// Cue Inspector save: writes the trim In/Out times and the Hold toggle,
	// then re-renders the inspector partial.
	rg.PUT("/cue/inspector/:cuePos", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		in := strings.TrimSpace(c.PostForm("posStart"))
		out := strings.TrimSpace(c.PostForm("posEnd"))
		hold := c.PostForm("hold") != ""
		if in == "" {
			in = "0"
		}
		if out == "" {
			out = "0"
		}
		inMS, inErr := ctp.ParseTime(in)
		outMS, outErr := ctp.ParseTime(out)
		if inErr != nil || outErr != nil || (inMS > 0 && outMS > 0 && outMS <= inMS) {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{
				"error": "trim Out must be after trim In",
			})
			return
		}
		loop := c.PostForm("loop") != ""
		autoCont := c.PostForm("autoContinue") != ""
		// Batched save: every column goes through ONE validated, single-statement
		// update (one version bump / WS sync), so clients can never refetch a
		// half-committed trim state between the individual column writes.
		fields := map[string]string{
			"posStart":     in,
			"posEnd":       out,
			"hold":         strconv.FormatBool(hold),
			"loop":         strconv.FormatBool(loop),
			"autoContinue": strconv.FormatBool(autoCont),
			"color":        strings.TrimSpace(c.PostForm("color")),
		}
		for _, col := range []string{"volume", "rate", "balance", "fadeIn", "preWait", "postWait", "fadeCurve", "cueDuration", "fit_mode", "rotation", "flip"} {
			if val, present := c.GetPostForm(col); present {
				fields[col] = strings.TrimSpace(val)
			}
		}
		// Images have no audio fields; keep their settings when saving colour.
		if _, present := c.GetPostForm("volume"); present {
			fields["mute"] = c.PostForm("mute")
		}
		// The dropdown always submits a value; "" is the "None" choice, so it is
		// written as-is and clears the stored colour.
		if lc := strings.TrimSpace(c.PostForm("loopCount")); lc != "" {
			fields["loop_count"] = lc
		}
		if fa := strings.TrimSpace(c.PostForm("fadeAction")); fa != "" {
			fields["fadeAction"] = fa
		}
		// fadeOut is always submitted; an empty field clears the fade time.
		fo := strings.TrimSpace(c.PostForm("fadeOut"))
		if fo == "" {
			fo = "0"
		}
		fields["fadeOut"] = fo
		// Recurring schedule block (day number + HH:MM[:SS] time). Present
		// only when the inspector renders the block; an unchecked box clears
		// enabled but keeps day/time so re-enabling restores them.
		if dayStr, present := c.GetPostForm("schedule_days"); present {
			_, on := c.GetPostForm("schedule_enabled")
			if !on {
				fields["schedule_enabled"] = "0"
			} else {
				day, derr := strconv.Atoi(strings.TrimSpace(dayStr))
				if derr != nil || day < 1 || day > 7 {
					c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "pick a schedule day"})
					return
				}
				hh, mm, ss, terr := splitHhMmSs(strings.TrimSpace(c.PostForm("schedule_time")))
				if terr != nil {
					c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "invalid schedule time: " + terr.Error()})
					return
				}
				fields["schedule_enabled"] = "1"
				fields["schedule_days"] = strconv.Itoa(1 << (day - 1))
				fields["schedule_time_ms"] = strconv.Itoa((hh*3600 + mm*60 + ss) * 1000)
			}
		}
		if err := ctp.UpdateCueFields(cuePos, fields); err != nil {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": err.Error()})
			return
		}
		cue, err := ctp.GetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		if gsp.CurrentCuePos() == cue.CuePos {
			gsp.SetMute(cue.Mute)
			gsp.SetVolume(cue.Volume)
			gsp.SetBalance(cue.Balance)
			gsp.SetRate(cue.Rate)
		}
		c.HTML(http.StatusOK, "cueinspector.html", gin.H{
			"Cue":           cue,
			"Selected":      true,
			"MediaDuration": cue.Duration,
			"Palette":       cuePalette,
			"MediaRows":     mediaInfoRows(cue),
			"ScheduleDay":   schedDayNum(cue.ScheduleDays),
			"Pool":          replacementPool(),
		})
	})

	// Re-link a cue to a different media item. Used to repair a cue whose
	// original source file went missing (the inspector's warning box).
	rg.PUT("/cue/:cuePos/replace", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		filename := strings.TrimSpace(c.PostForm("filename"))
		if filename == "" {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "a replacement media file is required"})
			return
		}
		if err := ctp.ReplaceCueMedia(cuePos, filename); err != nil {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.HTML(http.StatusOK, "cueinspector.html", inspectorData())
	})

	// Bulk reorder: client sends the full ordered list of cuePos values
	// (as produced by a drag-and-drop). Server reindexes 1..N atomically.
	// Move cue up

	rg.POST("/cue/:cuePos/move/up", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTEUp, "Move Cue Up%s", cuePos)
		err := ctp.MoveSheetCue(mustInt(cuePos), -1)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})

	// Move cue down
	rg.POST("/cue/:cuePos/move/down", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTEDown, "Move Cue Down%s", cuePos)
		err := ctp.MoveSheetCue(mustInt(cuePos), 1)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})

	rg.POST("/cue/:cuePos", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTEUse, "Selected%s", cuePos)
		// Multi-selection modifiers (§12.4): ?extend=1 ranges from the
		// anchor over VISIBLE units in sheet order (headers included),
		// ?toggle=1 flips membership. Plain clicks stay single-select.
		pos, perr := strconv.Atoi(cuePos)
		if perr == nil {
			if c.Query("extend") == "1" {
				if err := ctp.ExtendSelection(pos, 0); err == nil {
					renderCuesheet(c)
					return
				}
			} else if c.Query("toggle") == "1" {
				anchor, _ := ctp.SelectedCuePos()
				set := ctp.SelectedSet()
				out := set[:0]
				had := false
				for _, p := range set {
					if p == pos {
						had = true
						continue
					}
					out = append(out, p)
				}
				if !had && pos != anchor {
					out = append(out, pos)
				}
				if anchor == 0 {
					anchor = pos
				}
				if err := ctp.SetSelection(anchor, out); err == nil {
					renderCuesheet(c)
					return
				}
			}
		}
		err := ctp.SetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})

	rg.POST("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		logs.Printf(logs.RTEEdit, "Edit%s of CueNo%s", col, cuePos)
		cue, err := ctp.GetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		val, err := ctp.CueColumnValue(cue, col)
		if err != nil {
			c.HTML(http.StatusBadRequest, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.HTML(http.StatusOK, "cueeditcol.html", gin.H{
			"cuePos": cuePos,
			"col":    col,
			"val":    val,
			"action": "/api/cue/" + cuePos + "/edit/" + col,
		})
	})
	rg.PUT("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		val := c.PostForm("val")
		logs.Printf(logs.RTEUpdate, "Update %v Column %v Value %v", cuePos, col, val)
		err := ctp.UpdateCue(cuePos, col, val)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		renderCuesheet(c)
	})
	rg.DELETE("/cue/:cuePos", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTERemove, "Remove Cue%s", cuePos)
		err := ctp.RemoveCue(cuePos)
		if err != nil {
			logs.Printf(logs.RTERemove, "remove cue failed position=%q error=%v", cuePos, err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		// Re-render the cuesheet so the deleted cue is removed from the DOM.
		// Without a body, the hx-target/hx-swap on the delete button would
		// wipe the cuesheet with an empty response.
		renderCuesheet(c)
	})
	// Schedule: persist a cue's recurring trigger time. The scheduler fires
	// enabled cues whose day-of-week and time-of-day match the wall clock.
	rg.POST("/cue/:cuePos/schedule", func(c *gin.Context) {
		cuePos, err := strconv.Atoi(c.Param("cuePos"))
		if err != nil {
			c.String(http.StatusBadRequest, "invalid cue position")
			return
		}
		var body struct {
			Enabled   bool `json:"enabled"`
			Day       int  `json:"day"`       // 1=Mon .. 7=Sun
			TimeHhMm  string `json:"timeHhMm"` // "HH:MM" or "HH:MM:SS"
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid schedule payload: "+err.Error())
			return
		}
		// Disabling needs no day/time: clear the trigger and return before
		// validation, which only applies to enabling.
		if !body.Enabled {
			if err := ctp.SetCueSchedule(cuePos, false, 1, 0); err != nil {
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
				return
			}
			c.HTML(http.StatusOK, "cueinspector.html", inspectorData())
			return
		}
		if body.Day < 1 || body.Day > 7 || body.TimeHhMm == "" {
			c.String(http.StatusBadRequest, "invalid schedule day or time")
			return
		}
		hh, mm, ss, err := splitHhMmSs(body.TimeHhMm)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid schedule time: "+err.Error())
			return
		}
		if err := ctp.SetCueSchedule(cuePos, body.Enabled, body.Day, hh*3600+mm*60+ss); err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.HTML(http.StatusOK, "cueinspector.html", inspectorData())
	})

}
