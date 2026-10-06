package routes

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// builtinTestPatterns is the curated videotestsrc set offered in the Tests
// modal (§12.10); custom pool items ride on top of it. Names are the exact
// videotestsrc nicks (verified against gst-inspect 1.26) — anything else
// silently falls back to the default smpte.
var builtinTestPatterns = []struct {
	Name  string
	Label string
}{
	{"smpte", "SMPTE bars"},
	{"smpte100", "SMPTE 100%"},
	{"snow", "Snow"},
	{"black", "Black"},
	{"white", "White"},
	{"red", "Red"},
	{"green", "Green"},
	{"blue", "Blue"},
	{"checkers-1", "Checkers 1px"},
	{"checkers-2", "Checkers 2px"},
	{"checkers-4", "Checkers 4px"},
	{"checkers-8", "Checkers 8px"},
	{"circular", "Circle"},
	{"blink", "Blink"},
	{"solid-color", "Solid color"},
	{"bar", "Bar"},
}

// lastTestPattern is what the Tests toggle re-shows; the default SMPTE
// bars until the operator picks another (in-memory only). Handlers run
// concurrently, so it is only touched through get/setLastTestPattern.
var (
	lastTestPatternMu sync.Mutex
	lastTestPattern   = "smpte"
)

func getLastTestPattern() string {
	lastTestPatternMu.Lock()
	defer lastTestPatternMu.Unlock()
	return lastTestPattern
}

func setLastTestPattern(p string) {
	lastTestPatternMu.Lock()
	lastTestPattern = p
	lastTestPatternMu.Unlock()
}

// waveSlots caps concurrent waveform-window decodes (GET .../wave).
var waveSlots = make(chan struct{}, 2)

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
		// The stop job SIGTERMs this whole cgroup, systemctl included, so a
		// blocking `systemctl restart` run inside the request never returns
		// cleanly: the handler answered 500 while the restart went ahead.
		// Answer first, then queue the job (--no-block) after the grace.
		systemctl, err := exec.LookPath("systemctl")
		if err != nil {
			return err
		}
		go func() {
			time.Sleep(grace)
			cmd := exec.Command(systemctl, "--no-block", "restart", unit)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				logs.Printf(logs.RTERestart, "systemctl restart %s: %v", unit, err)
			}
		}()
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

// wifiQREscape escapes a WIFI: QR field: the format's special characters
// (\ ; , : ") must be backslash-escaped or an SSID/password containing them
// produces a code phones mis-parse.
func wifiQREscape(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\', ';', ',', ':', '"':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func Api(rg *gin.RouterGroup) {
	registerThemeRoutes(rg)
	registerGLWallRoutes(rg)
	registerDisplayRoute(rg)
	registerAudioRoute(rg)
	registerDiskRoute(rg)
	registerLiveRoutes(rg)
	rg.GET("/ws", func(c *gin.Context) {
		ws.Handle(c.Writer, c.Request)
	})
	// Full HTML render of the "Now Playing" widget. Used for the initial page
	// render and by the WebSocket-driven refresher (public/src/ui.js) only
	// when GET /api/nowplaying/status reports a change.
	rg.GET("/nowplaying", func(c *gin.Context) {
		c.HTML(http.StatusOK, "mediainfo.html", nowplayingData())
	})

	// Lightweight change-detection endpoint, called once per WebSocket
	// "sync" (no polling). Returns the server-side version (playback state
	// counter + cuesheet counter) plus a "changed" flag computed against the
	// version the client last saw; the widget only re-renders (via GET
	// /api/nowplaying) when changed is true. Per-client tracking lives
	// entirely in the client, so any number of concurrent clients work.
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
		// The reverse proxy reports the client-facing scheme. Any client can
		// send the header, so only the two real values are honoured.
		switch proto := strings.ToLower(strings.TrimSpace(c.Request.Header.Get("X-Forwarded-Proto"))); proto {
		case "http", "https":
			scheme = proto
		}
		host := c.Request.Host
		if host == "" {
			host = fmt.Sprintf("localhost:%d", config.Port())
		}
		target := fmt.Sprintf("%s://%s/upload", scheme, host)
		if c.Query("type") == "wifi" {
			// Wi-Fi join code (Android hostapd 2.x syntax): clients scan it
			// straight into their network list - used by the Network tab.
			ap := config.AP()
			// The join code necessarily carries the hotspot password; like
			// every API route it is behind the operator password when set.
			target = fmt.Sprintf("WIFI:T:WPA;S:%s;P:%s;;", wifiQREscape(ap.SSID), wifiQREscape(ap.Pass))
			if ap.Pass == "" {
				target = fmt.Sprintf("WIFI:T:nopass;S:%s;;", wifiQREscape(ap.SSID))
			}
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
			respondError(c, http.StatusBadRequest, "position must be a number of seconds")
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
			respondError(c, http.StatusBadRequest, "volume must be a number")
			return
		}
		// Persist on the cue whose pipeline took the change (read with it),
		// never on a cue that fired in between.
		applied, pos := gsp.SetCueVolume(v)
		// The live change already applied; failing to remember it on the
		// cue is worth a warning, not a failed request.
		if pos > 0 {
			if err := ctp.UpdateCue(strconv.Itoa(pos), "volume", strconv.FormatFloat(applied, 'f', -1, 64)); err != nil {
				logs.PrintfWarn(logs.RTEEdit, "saving volume on cue %d: %v", pos, err)
			}
		}
		c.Status(http.StatusOK)
	})
	// Shared by ESC and the menu's Fade out: fade the running output to black
	// over the Settings > General ESC fade time, then stop everything
	// including any background soundtrack. Nothing loaded is a plain stop.
	// Returns immediately; the fade runs out in the background like the
	// queued fade-then-play.
	fadeStop := func(c *gin.Context) {
		if gsp.CurrentPlaying() == "" {
			gsp.Stop()
			c.Status(http.StatusOK)
			return
		}
		durMs := ctp.GetEscFadeMs()
		logs.Printf(logs.RTEStop, "ESC fade-stop %dms", durMs)
		loads := gsp.Loads()
		goSafe(func() {
			gsp.FadeAndStop(durMs)
			// A cue fired during the fade is a newer decision; the trailing
			// stop (which also ends the background soundtrack) must not kill it.
			if gsp.Loads() != loads {
				return
			}
			gsp.Stop()
		})
		c.Status(http.StatusOK)
	}
	rg.POST("/fadeOut", func(c *gin.Context) {
		logs.Printf(logs.RTEFadeOut, "fadeOut")
		fadeStop(c)
	})
	rg.POST("/panic", func(c *gin.Context) {
		logs.Printf(logs.RTEPanic, "!!PANIC!!")
		if err := remotePanic(); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusOK)
	})
	rg.POST("/clear", func(c *gin.Context) {
		logs.Printf(logs.RTEClear, "Clear CueSheet")
		err := ctp.ClearCueSheet()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		renderCuesheet(c)
	})
	rg.POST("/stop", func(c *gin.Context) {
		logs.Printf(logs.RTEStop, "STOP")
		gsp.Stop()
		c.Status(http.StatusOK)
	})

	// ESC key: see fadeStop.
	rg.POST("/esc", fadeStop)

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
		// Tests are an Edit-mode tool: Show mode locks the sheet.
		if ctp.GetShowMode() {
			c.String(http.StatusForbidden, "tests are only available in Edit mode")
			return
		}
		pattern := strings.TrimPrefix(c.Param("pattern"), "/")
		if pattern == "" {
			pattern = getLastTestPattern()
		}
		// "toggle" rides the wildcard (gin forbids a static sibling next
		// to /*pattern): the Tests button. On when dark (shows the last
		// pattern), off when a test is showing (stops it). Answers the
		// state so the button tracks it.
		if pattern == "toggle" {
			if gsp.TestShowing() {
				gsp.Stop()
				c.JSON(http.StatusOK, gin.H{"showing": false})
				return
			}
			pattern = getLastTestPattern()
			if err := gsp.ShowTest(pattern); err != nil {
				logs.Printf(logs.RTETest, "show test failed pattern=%q error=%v", pattern, err)
				respondError(c, http.StatusInternalServerError, err.Error())
				return
			}
			logs.Printf(logs.RTETest, "Show test pattern=%q", pattern)
			c.JSON(http.StatusOK, gin.H{"showing": true, "pattern": pattern})
			return
		}
		known := false
		for _, p := range builtinTestPatterns {
			if p.Name == pattern {
				known = true
				break
			}
		}
		if !known {
			c.String(http.StatusBadRequest, "unknown test pattern")
			return
		}
		setLastTestPattern(pattern)
		if err := gsp.ShowTest(pattern); err != nil {
			logs.Printf(logs.RTETest, "show test failed pattern=%q error=%v", pattern, err)
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		logs.Printf(logs.RTETest, "Show test pattern=%q", pattern)
		c.Status(http.StatusOK)
	})

	// Test-pattern label (display resolution and refresh rate), §12.10.
	gsp.SetTestOverlay(ctp.GetTestOverlay())
	rg.POST("/setting/testoverlay", func(c *gin.Context) {
		on := c.PostForm("on") == "1" || c.PostForm("on") == "true"
		if err := ctp.SetTestOverlay(on); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		gsp.SetTestOverlay(on)
		// A pattern on the wall picks the change up at once.
		if gsp.TestShowing() && !ctp.GetShowMode() {
			if err := gsp.ShowTest(getLastTestPattern()); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
		}
		c.Status(http.StatusNoContent)
	})

	// GET /api/testpatterns: what the Tests modal lists — built-ins plus the
	// operator's pinned pool items, plus live toggle state for the button.
	rg.GET("/testpatterns", func(c *gin.Context) {
		builtin := make([]gin.H, 0, len(builtinTestPatterns))
		for _, p := range builtinTestPatterns {
			builtin = append(builtin, gin.H{"name": p.Name, "label": p.Label})
		}
		c.JSON(http.StatusOK, gin.H{
			"builtin": builtin,
			"custom":  ctp.TestPatterns(),
			"showing": gsp.TestShowing(),
			"current": getLastTestPattern(),
			"overlay": ctp.GetTestOverlay(),
		})
	})

	// Pin/unpin a pool item as a custom test pattern (media tile menu).
	rg.POST("/testpattern/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		on := strings.TrimSpace(c.PostForm("on")) == "1"
		var err error
		if on {
			if ok, merr := ctp.MediaRegistered(filename); merr != nil || !ok {
				c.String(http.StatusNotFound, "media not found")
				return
			}
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
		if err := gsp.LoadWithOpts(filename, gsp.DirectOpts(filename, gain)); err != nil {
			logs.Printf(logs.RTEDirect, "direct play failed filename=%q error=%v", filename, err)
			respondError(c, http.StatusInternalServerError, err.Error())
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
		if err := gsp.LoadWithOpts(filename, gsp.DirectOpts(filename, gain)); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		// The pool row is gone either way; a file that can't be removed is
		// orphaned disk space, worth a log line (absent files are fine).
		for _, p := range []string{
			filepath.Join(config.MediaLocation(), filename),
			filepath.Join(config.ThumbnailLocation(), filename+".jpg"),
		} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				logs.PrintfWarn(logs.RTEDelete, "removing %q: %v", p, err)
			}
		}

		// Re-render the mediapool so the deleted tile is removed from the DOM.
		// Without a body the hx-target/hx-swap on the delete dropdown item
		// would remove the wrong element (the <li> the button lives in) or
		// leave a stale tile.
		mediapool, err := mediapoolView()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusOK)
	})

	// Requests (re)analysis of a media file's waveform (amplitude peaks used
	// by the Cue Inspector trim timeline). Picked up by the background worker.
	rg.POST("/media/:filename/analyse", func(c *gin.Context) {
		filename := c.Param("filename")
		if err := ctp.RequestWaveformAnalysis(filename); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
		if ok, merr := ctp.MediaRegistered(filename); merr != nil || !ok {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		src := filepath.Join(config.MediaLocation(), filename)
		if _, err := os.Stat(src); err != nil {
			c.String(http.StatusNotFound, "media not found")
			return
		}
		// Each window is an ffmpeg decode: zooming the trim timeline must
		// not stack decoders against the playing show. Wait for a slot, or
		// give up when the client has moved on.
		select {
		case waveSlots <- struct{}{}:
			defer func() { <-waveSlots }()
		case <-c.Request.Context().Done():
			return
		}
		peaks, err := media.GeneratePeaksWindow(src, from, to, bins)
		if err != nil {
			logs.PrintfWarn("WAVE", "window %v-%v of %s: %v", from, to, filename, err)
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, peaks)
	})

	// Header connection tooltip (broadcast-pin hover): server + client facts.
	rg.GET("/serverinfo", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"clients": ws.ClientCount(),
			"live":    ws.ClientCount() > 0,
			"uptimeS": int(time.Since(startTime).Seconds()),
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
		units, err := ctp.SelectUnits()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		// Ids: +cuePos for cues, -groupID for headers (same as the persisted set).
		ids := make([]int, 0, len(units))
		for _, u := range units {
			if u.IsGroup {
				ids = append(ids, -u.GroupID)
			} else {
				ids = append(ids, u.CuePos)
			}
		}
		anchor, _ := ctp.SelectedCuePos()
		if anchor == 0 {
			if gid, _ := ctp.SelectedGroupPos(); gid != 0 {
				anchor = -gid
			}
		}
		set := make([]int, 0, len(ids))
		for _, id := range ids {
			if id != anchor {
				set = append(set, id)
			}
		}
		if anchor == 0 && len(set) > 0 {
			anchor, set = set[0], set[1:]
		}
		if err := ctp.SetGroupSelection(anchor, set); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		// Deleting whole groups (§5.4): folder, subgroups and every member
		// cue. Runs after the cue op so one request covers a mixed set.
		for _, gid := range body.Groups {
			if err := ctp.DeleteGroupWithCues(gid); err != nil {
				respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
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
			ForceTop   bool   `json:"forceTop"`  // gap drop on a group boundary: stay top-level
			JoinFirst  bool   `json:"joinFirst"` // expanded-header drop: first cue in group (§5.4)
			Parent     *int   `json:"parent"`    // explicit band membership (§5.4)
			After      int    `json:"after"`     // member-slot drop: insert after this cue
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid drop payload: "+err.Error())
			return
		}
		if err := ctp.SheetDrop(body.Cues, intOrZero(body.Group), body.BeforeKind, body.BeforeID, body.Join, body.ForceTop, body.JoinFirst, body.Parent, body.After); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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

	// Sort the sheet by cue number (§12.5), group blocks kept together.
	rg.POST("/cue/sort", func(c *gin.Context) {
		if err := ctp.SortSheetByCueNumber(); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		renderCuesheet(c)
	})
	// Renumber every cue in sheet order: step, 2×step… (1, 2, 3 by default;
	// §12.5) — explicit operator action, rewrites hand-set numbers.
	rg.POST("/cue/renumber", func(c *gin.Context) {
		if err := ctp.RenumberSheet(); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		renderCuesheet(c)
	})

	// Output device from the inspector Audio tab (§5.5): the same
	// system-wide ALSA device as Settings > Audio (CuTePi is the only audio
	// producer), keeping the saved channels/rate. "" = default HDMI embedded.
	rg.POST("/setting/audiodevice", func(c *gin.Context) {
		a := config.Audio()
		if err := config.SetAudio(c.PostForm("device"), a.Channels, a.Rate); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
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
	// Instance rename: hostname + mDNS (<name>.local). Errors fail loudly
	// (the operator must know the address did NOT change); Avahi refresh
	// trouble only warns.
	rg.POST("/setting/hostname", func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("hostname"))
		if err := SetInstanceName(name); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"hostname": InstanceName()})
	})
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
		kickPanicArm()
		c.Status(http.StatusNoContent)
	})
	rg.GET("/settings", func(c *gin.Context) {
		display, audio := config.Display(), config.Audio()
		ap := config.AP()
		c.JSON(http.StatusOK, gin.H{
			"port":         config.Port(),
			"loop":         gsp.Loop(),
			"authEnabled":  config.HasAuth(), // never return the password itself
			"panicHold":    ctp.GetPanicHoldImage(),
			"escFadeMs":    ctp.GetEscFadeMs(),
			"instanceName": InstanceName(),
			"autoNumber":   ctp.GetAutoNumber(),
			"cueNumStep":   ctp.GetCueNumStep(),
			"displayMode":  displayModeLabel(),
			"goAdvance":    ctp.GetGoAdvance(),
			"showMode":     ctp.GetShowMode(),
			"display": gin.H{
				"resolution": display.Resolution,
				"refreshHz":  display.RefreshHz,
				"useEDID":    display.UseEDID,
			},
			"audio": gin.H{
				"device":   audio.Device,
				"channels": audio.Channels,
				"rate":     audio.Rate,
			},
			"ap": gin.H{
				"enabled": ap.Enabled,
				"ssid":    ap.SSID,
				//PASSWORD NEVER RETURNED — only whether it is set
				"passwordSet": ap.Pass != "",
			},
			"remote":       config.Remote(),
			"remoteStatus": RemoteStatus(),
		})
	})

	rg.POST("/settings", func(c *gin.Context) {
		var body struct {
			Port              int    `json:"port" form:"port"`
			Loop              *bool  `json:"loop" form:"loop"`
			Password          string `json:"password" form:"password"`
			ClearPassword     bool   `json:"clearPassword" form:"clearPassword"`
			DisplayResolution string `json:"displayResolution" form:"displayResolution"`
			DisplayRefresh    int    `json:"displayRefresh" form:"displayRefresh"`
			DisplayUseEDID    bool   `json:"displayUseEDID" form:"displayUseEDID"`
			AudioDevice       string `json:"audioDevice" form:"audioDevice"`
			AudioChannels     string `json:"audioChannels" form:"audioChannels"`
			AudioRate         int    `json:"audioRate" form:"audioRate"`
			APSSID            string `json:"apSSID" form:"apSSID"`
			APPass            string `json:"apPass" form:"apPass"`
			APEnabled         *bool  `json:"apEnabled" form:"apEnabled"`
			EscFadeMs         *int   `json:"escFadeMs" form:"escFadeMs"`
			EscFade           string `json:"escFade" form:"escFade"` // time text ("1s", "0:01.5", "500ms"); wins over escFadeMs
			CueNumStep        string `json:"cueNumStep" form:"cueNumStep"`
			// Remote listeners (§12.8). RemoteBlock marks that the Network
			// tab posted them, so absent (unchecked) toggles mean "off" only
			// then — a partial post never switches a listener off.
			RemoteBlock          bool   `json:"remoteBlock" form:"remoteBlock"`
			RemoteHyperdeck      bool   `json:"remoteHyperdeck" form:"remoteHyperdeck"`
			RemoteHyperdeckPort  int    `json:"remoteHyperdeckPort" form:"remoteHyperdeckPort"`
			RemoteHyperdeckClips string `json:"remoteHyperdeckClips" form:"remoteHyperdeckClips"`
			RemoteOSCUDP         bool   `json:"remoteOscUdp" form:"remoteOscUdp"`
			RemoteOSCUDPPort     int    `json:"remoteOscUdpPort" form:"remoteOscUdpPort"`
			RemoteOSCTCP         bool   `json:"remoteOscTcp" form:"remoteOscTcp"`
			RemoteOSCTCPPort     int    `json:"remoteOscTcpPort" form:"remoteOscTcpPort"`
			RemoteOSCBind        string `json:"remoteOscBind" form:"remoteOscBind"`
		}
		if err := c.ShouldBind(&body); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		if raw := strings.TrimSpace(body.EscFade); raw != "" {
			ms, err := ctp.ParseTime(raw)
			if err != nil {
				respondError(c, http.StatusBadRequest, "ESC fade-out time: "+err.Error())
				return
			}
			body.EscFadeMs = &ms
		}
		// Which blocks this post carries. An all-default block means "not
		// touched by this form post" and must not wipe real settings (older
		// clients, partial pages). AP fields apply only when the Network tab
		// actually posted them (an SSID value or the checkbox); a blank pass
		// keeps the stored one — the tab never re-renders existing passwords.
		hasDisplay := body.DisplayResolution != "" || body.DisplayRefresh > 0 || body.DisplayUseEDID
		hasAudio := body.AudioDevice != "" || body.AudioRate > 0 || body.AudioChannels != ""
		hasAP := body.APSSID != "" || body.APEnabled != nil
		apPrev := config.AP()
		apEnabled := body.APEnabled != nil && *body.APEnabled
		apPass := body.APPass
		if apPass == "" {
			apPass = apPrev.Pass
		}
		remote := config.RemoteSettings{
			HyperDeck:      body.RemoteHyperdeck,
			HyperDeckPort:  body.RemoteHyperdeckPort,
			HyperDeckClips: body.RemoteHyperdeckClips,
			OSCUDP:         body.RemoteOSCUDP,
			OSCUDPPort:     body.RemoteOSCUDPPort,
			OSCTCP:         body.RemoteOSCTCP,
			OSCTCPPort:     body.RemoteOSCTCPPort,
			OSCBind:        body.RemoteOSCBind,
		}

		numStep, numStepErr := 0.0, error(nil)
		if raw := strings.TrimSpace(body.CueNumStep); raw != "" {
			if numStep, numStepErr = strconv.ParseFloat(raw, 64); numStepErr == nil {
				numStepErr = ctp.ValidateCueNumStep(numStep)
			} else {
				numStepErr = ctp.ValidateCueNumStep(-1)
			}
		}
		// Validate EVERY posted field before persisting any: a bad value
		// late in the form must not leave the earlier fields half-saved.
		var verr error
		switch {
		case body.Port > 0 && config.ValidatePort(body.Port) != nil:
			verr = config.ValidatePort(body.Port)
		case numStepErr != nil:
			verr = numStepErr
		case body.EscFadeMs != nil && ctp.ValidateEscFadeMs(*body.EscFadeMs) != nil:
			verr = ctp.ValidateEscFadeMs(*body.EscFadeMs)
		case hasDisplay && config.ValidateDisplay(body.DisplayResolution, body.DisplayRefresh) != nil:
			verr = config.ValidateDisplay(body.DisplayResolution, body.DisplayRefresh)
		case hasAudio && config.ValidateAudio(body.AudioChannels, body.AudioRate) != nil:
			verr = config.ValidateAudio(body.AudioChannels, body.AudioRate)
		case hasAP && config.ValidateAP(body.APSSID, apPass, apEnabled) != nil:
			verr = config.ValidateAP(body.APSSID, apPass, apEnabled)
		}
		if verr == nil && body.RemoteBlock {
			_, verr = config.ValidateRemote(remote)
		}
		if verr != nil {
			respondError(c, http.StatusBadRequest, verr.Error())
			return
		}

		// Apply. Validation passed, so the only failures left are persistence
		// (disk full, read-only data dir) — surfaced as 500s.
		fail := func(err error) bool {
			if err == nil {
				return false
			}
			respondError(c, http.StatusInternalServerError, "saving settings: "+err.Error())
			return true
		}
		portChanged := body.Port > 0 && body.Port != config.Port()
		displayBefore := config.Display()
		displayChanged := hasDisplay && (body.DisplayResolution != displayBefore.Resolution || body.DisplayRefresh != displayBefore.RefreshHz || body.DisplayUseEDID != displayBefore.UseEDID)
		if body.Port > 0 && fail(config.SetPort(body.Port)) {
			return
		}
		if body.Loop != nil && fail(gsp.SetLoop(*body.Loop)) {
			return
		}
		if numStep > 0 && fail(ctp.SetCueNumStep(numStep)) {
			return
		}
		if body.EscFadeMs != nil && fail(ctp.SetEscFadeMs(*body.EscFadeMs)) {
			return
		}
		// A blank password means "unchanged" (forms always send the field);
		// the explicit clear checkbox disables auth.
		if body.ClearPassword {
			if fail(config.SetAuthPassword("")) {
				return
			}
		} else if pw := strings.TrimSpace(body.Password); pw != "" {
			if fail(config.SetAuthPassword(pw)) {
				return
			}
		}
		if hasDisplay && fail(config.SetDisplay(body.DisplayResolution, body.DisplayRefresh, body.DisplayUseEDID)) {
			return
		}
		if hasAudio && fail(config.SetAudio(body.AudioDevice, body.AudioChannels, body.AudioRate)) {
			return
		}
		// The hotspot is only touched when its own settings changed:
		// re-running nmcli on every save (the old behaviour) bounced the
		// access point — dropping every Wi-Fi tablet driving the show —
		// whenever the operator changed an unrelated setting.
		if hasAP {
			if fail(config.SetAP(body.APSSID, apPass, apEnabled)) {
				return
			}
			if apNow := config.AP(); apNow != apPrev {
				if apErr := applyAP(); apErr != nil {
					networkAPWarn(apErr) // hotspot failure must never kill the save
				}
			}
		}
		if body.RemoteBlock {
			if fail(config.SetRemote(remote)) {
				return
			}
			ApplyRemote()
		}
		restartNeeded := portChanged || displayChanged
		msg := "Saved."
		if restartNeeded {
			msg = "Saved. CuTePi is restarting to apply the display or port change."
		}
		c.JSON(http.StatusOK, gin.H{
			"port":         config.Port(),
			"loop":         gsp.Loop(),
			"authEnabled":  config.HasAuth(),
			"remoteStatus": RemoteStatus(),
			"message":      msg,
			"restarting":   restartNeeded,
		})
		if restartNeeded {
			go func() {
				time.Sleep(500 * time.Millisecond) // let the JSON response flush
				if err := restartServer(300 * time.Millisecond); err != nil {
					logs.Printf(logs.RTERestart, "settings-triggered restart failed: %v", err)
				}
			}()
		}
	})

	// Restart / Shutdown the server process itself. Both respond first (so
	// the browser gets a clean 200) and act shortly after (grace), letting
	// the response flush before the process exits.
	rg.POST("/restart", func(c *gin.Context) {
		logs.Printf(logs.RTERestart, "Server restart requested")
		if err := restartServer(300 * time.Millisecond); err != nil {
			logs.Printf(logs.RTERestart, "restart failed: %v", err)
			respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		awardsSelectionSync()
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		awardsSelectionSync()
		renderCuesheet(c)
	})

	// Play a specific cue by position
	// "Fade & Stop Others Over Time" and the load itself live in FireCue
	// (routes/remote.go) so the Web UI, HyperDeck and OSC transports share
	// one code path.
	rg.POST("/cue/:cuePos/play", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTECuePlay, "Play Cue%s", cuePos)
		if err := FireCue(cuePos); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusOK)
	})

	// Play the currently selected cue (what spacebar acts on). If nothing is
	// selected, fall back to resuming whatever is loaded - also the previous
	// spacebar behaviour. The fire-and-advance core (FireSelected,
	// routes/remote.go) is shared with the HyperDeck and OSC remote
	// transports; only the render is HTTP-specific.
	rg.POST("/cue/selected/play", func(c *gin.Context) {
		if err := FireSelected(); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
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
			msg := "trim Out must be after trim In"
			switch {
			case inErr != nil:
				msg = fmt.Sprintf("invalid trim In %q (use hh:mm:ss.mmm)", in)
			case outErr != nil:
				msg = fmt.Sprintf("invalid trim Out %q (use hh:mm:ss.mmm)", out)
			}
			respondError(c, http.StatusBadRequest, msg)
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
		for _, col := range []string{"volume", "rate", "balance", "fadeIn", "preWait", "postWait", "fadeCurve", "cueDuration", "fit_mode", "rotation", "flip", "opacity", "geom_x", "geom_y", "geom_w", "geom_h", "crop_l", "crop_r", "crop_t", "crop_b"} {
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
		// fadeOut is always submitted; the enable checkbox ARMS the fade:
		// unchecked → the stored time is cleared (0) no matter what the
		// field says, while scope/curve keep their values.
		if _, present := c.GetPostForm("fadeEnabled"); present {
			if _, on := c.GetPostForm("fadeEnabled"); on {
				fo := strings.TrimSpace(c.PostForm("fadeOut"))
				if fo == "" {
					fo = "0"
				}
				fields["fadeOut"] = fo
			} else {
				fields["fadeOut"] = "0"
			}
		} else {
			fo := strings.TrimSpace(c.PostForm("fadeOut"))
			if fo == "" {
				fo = "0"
			}
			fields["fadeOut"] = fo
		}
		// Recurring schedule block (day bitmask + HH:MM[:SS] time). The
		// hidden schedule_block marker marks the block as rendered; an
		// unchecked enable box clears enabled but keeps day/time so
		// re-enabling restores them.
		if _, present := c.GetPostForm("schedule_block"); present {
			_, on := c.GetPostForm("schedule_enabled")
			if !on {
				fields["schedule_enabled"] = "0"
			} else {
				mask := 0
				for _, d := range c.PostFormArray("schedule_day") {
					if n, nerr := strconv.Atoi(strings.TrimSpace(d)); nerr == nil && n >= 1 && n <= 7 {
						mask |= 1 << (n - 1)
					}
				}
				if mask == 0 {
					respondError(c, http.StatusBadRequest, "pick at least one schedule day")
					return
				}
				hh, mm, ss, terr := splitHhMmSs(strings.TrimSpace(c.PostForm("schedule_time")))
				if terr != nil {
					respondError(c, http.StatusBadRequest, "invalid schedule time: "+terr.Error())
					return
				}
				fields["schedule_enabled"] = "1"
				fields["schedule_days"] = strconv.Itoa(mask)
				fields["schedule_time_ms"] = strconv.Itoa((hh*3600 + mm*60 + ss) * 1000)
			}
		}
		// Live page: the URL lives on the cue's own (hidden) source row.
		// Validated before anything else is saved.
		liveURLChanged := false
		if raw, present := c.GetPostForm("endpointUrl"); present {
			before, err := ctp.GetCue(cuePos)
			if err != nil {
				respondError(c, http.StatusNotFound, err.Error())
				return
			}
			if strings.TrimSpace(raw) != before.EndpointURL {
				pos, _ := strconv.Atoi(cuePos)
				if err := ctp.SetCueEndpointURL(pos, raw); err != nil {
					respondError(c, http.StatusBadRequest, err.Error())
					return
				}
				liveURLChanged = true
			}
		}
		if err := ctp.UpdateCueFields(cuePos, fields); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		cue, err := ctp.GetCue(cuePos)
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		if liveURLChanged && gsp.CurrentCuePos() == cue.CuePos {
			// On the wall now: show the new page straight away.
			if err := loadAndPlayCue(cue); err != nil {
				logs.PrintfWarn(logs.RTEEdit, "reloading live cue %d: %v", cue.CuePos, err)
			}
		}
		// Only if it is still this cue playing, checked as it applies.
		gsp.ApplyCueMix(cue.CuePos, gsp.CueMix{Mute: cue.Mute, Volume: cue.Volume, Balance: cue.Balance, Rate: cue.Rate})
		c.HTML(http.StatusOK, "cueinspector.html", gin.H{
			"Cue":           cue,
			"Selected":      true,
			"MediaDuration": cue.Duration,
			"Palette":       cuePalette,
			"MediaRows":     mediaInfoRows(cue),
			"ScheduleDay":   schedDayNum(cue.ScheduleDays),
			"Pool":          replacementPool(),
			"AudioDevice":   config.Audio().Device,
		})
	})

	// Re-link a cue to a different media item. Used to repair a cue whose
	// original source file went missing (the inspector's warning box).
	rg.PUT("/cue/:cuePos/replace", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		filename := strings.TrimSpace(c.PostForm("filename"))
		if filename == "" {
			respondError(c, http.StatusBadRequest, "a replacement media file is required")
			return
		}
		if err := ctp.ReplaceCueMedia(cuePos, filename); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
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
		pos, perr := strconv.Atoi(cuePos)
		if perr != nil {
			c.String(http.StatusBadRequest, "invalid cue position")
			return
		}
		err := ctp.MoveSheetCue(pos, -1)
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		renderCuesheet(c)
	})

	// Move cue down
	rg.POST("/cue/:cuePos/move/down", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		logs.Printf(logs.RTEDown, "Move Cue Down%s", cuePos)
		pos, perr := strconv.Atoi(cuePos)
		if perr != nil {
			c.String(http.StatusBadRequest, "invalid cue position")
			return
		}
		err := ctp.MoveSheetCue(pos, 1)
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
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
					awardsSelectionSync()
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
					awardsSelectionSync()
					renderCuesheet(c)
					return
				}
			}
		}
		err := ctp.SetCue(cuePos)
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		awardsSelectionSync()
		renderCuesheet(c)
	})

	rg.POST("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		logs.Printf(logs.RTEEdit, "Edit%s of CueNo%s", col, cuePos)
		cue, err := ctp.GetCue(cuePos)
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		val, err := ctp.CueColumnValue(cue, col)
		if err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		validate := ""
		switch col {
		case "posStart", "posEnd", "preWait", "cueDuration", "postWait", "fadeOut", "fadeIn":
			validate = "time"
		}
		c.HTML(http.StatusOK, "cueeditcol.html", gin.H{
			"cuePos":   cuePos,
			"col":      col,
			"val":      val,
			"validate": validate,
			"action":   "/api/cue/" + cuePos + "/edit/" + col,
		})
	})
	rg.PUT("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		val := c.PostForm("val")
		logs.Printf(logs.RTEUpdate, "Update %v Column %v Value %v", cuePos, col, val)
		err := ctp.UpdateCue(cuePos, col, val)
		if errors.Is(err, ctp.ErrDuplicateCueNum) {
			rejectCueNum(c, err, `#cuesheet tr.cue[data-cue-pos="`+cuePos+`"] .cue-num`, "")
			renderCuesheet(c)
			return
		}
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ctp.ErrInvalidTimeFormat) {
				status = http.StatusBadRequest
			}
			respondError(c, status, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
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
			Enabled  bool   `json:"enabled"`
			Day      int    `json:"day"`      // 1=Mon .. 7=Sun
			TimeHhMm string `json:"timeHhMm"` // "HH:MM" or "HH:MM:SS"
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid schedule payload: "+err.Error())
			return
		}
		// Disabling needs no day/time: clear the trigger and return before
		// validation, which only applies to enabling.
		if !body.Enabled {
			if err := ctp.SetCueSchedule(cuePos, false, 1, 0); err != nil {
				respondError(c, http.StatusInternalServerError, err.Error())
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
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.HTML(http.StatusOK, "cueinspector.html", inspectorData())
	})

}

// displayModeLabel is the HDMI output's current mode for the Settings
// Display tab, e.g. "1920 × 1080 @ 60 Hz" ("" when unknown).
func displayModeLabel() string {
	w, h, hz := gsp.DisplayMode()
	if w <= 0 || h <= 0 {
		return ""
	}
	if hz > 0 {
		return fmt.Sprintf("%d × %d @ %d Hz", w, h, hz)
	}
	return fmt.Sprintf("%d × %d", w, h)
}
