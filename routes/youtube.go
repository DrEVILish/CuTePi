package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"
)

func Youtube(rg *gin.RouterGroup) {
	rg.POST("", handleYoutubeDownload)
	rg.POST("/", handleYoutubeDownload)
	rg.POST("/rename", handleYoutubeRename)
}

// handleYoutubeDownload streams the download as a newline-delimited JSON
// event stream: {"stage":"resolving"}, {"stage":"download","pct":45.3,
// "speed":"2.1MiB/s","eta":"00:04"}, ..., {"done":true,"filename":...} or
// {"error":"..."}. The client renders these live; on done it refreshes the
// media pool itself (its own re-render follows the yt-dlp title anyway).
func handleYoutubeDownload(c *gin.Context) {
	url := strings.TrimSpace(c.PostForm("url"))
	logs.Printf(logs.YDLRequest, "stage=request method=%s path=%s url=%q", c.Request.Method, c.Request.URL.Path, url)
	if url == "" {
		logs.Printf(logs.YDLFailed, "stage=validate reason=empty_url")
		respondError(c, http.StatusBadRequest, "no URL provided")
		return
	}

	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Cache-Control", "no-cache")
	// yt-dlp's stdout and stderr are pumped on separate goroutines; gin's
	// ResponseWriter is not safe for concurrent use, so every write to the
	// stream goes through this one mutex.
	var wmu sync.Mutex
	writeLine := func(msg map[string]any) bool {
		body, _ := json.Marshal(msg)
		wmu.Lock()
		defer wmu.Unlock()
		_, err := c.Writer.Write(append(body, '\n'))
		c.Writer.Flush()
		return err == nil
	}
	stage := func(s string) { writeLine(map[string]any{"stage": s}) }
	fail := func(code string, msg string) {
		logs.PrintfWarn(code, "url=%q error=%s", url, msg)
		writeLine(map[string]any{"error": redactPaths(msg)})
	}

	stage("resolving")
	th := &pctThrottle{every: 250 * time.Millisecond}
	// The request context: a client that disconnects (closed modal, lost
	// Wi-Fi) kills yt-dlp instead of leaving it downloading for 30 minutes.
	tmpDir, filename, err := downloadWithYtDlp(c.Request.Context(), url, func(line string) {
		if msg := parseYtDlpLine(line, th); msg != nil {
			writeLine(msg)
		}
	})
	if err != nil {
		fail(logs.YDLFailed, fmt.Sprintf("download failed: %v", err))
		return
	}
	defer os.RemoveAll(tmpDir)
	dlPath := filepath.Join(tmpDir, filename)
	logs.Printf(logs.YDLDownload, "stage=complete filename=%q", filename)

	// A download never silently replaces a pool file of the same name (the
	// upload path asks first; here there is nobody to ask): keep both, and
	// the operator can rename it from the done dialog.
	dlName := filename
	filename, release, err := reserveMediaName(filename, reserveRename)
	if err != nil {
		fail(logs.YDLFailed, err.Error())
		return
	}
	defer release()
	if filename != dlName {
		logs.Printf(logs.YDLDownload, "stage=rename reason=name_taken from=%q to=%q", dlName, filename)
	}

	stage("importing")
	logs.Printf(logs.YDLProbe, "stage=start filename=%q path=%q", filename, dlPath)
	if err := importMedia(filename, dlPath); err != nil {
		fail(logs.YDLFailed, err.Error())
		return
	}
	logs.Printf(logs.YDLRender, "stage=complete filename=%q", filename)
	writeLine(map[string]any{"done": true, "filename": filename})
}

// parseYtDlpLine maps one yt-dlp console line to a progress event. Lines
// without meaningful state yield nil (dropped). pct events are throttled so
// the DOM is not redrawn at yt-dlp's full line rate (~10/s).
var (
	rePct     = regexp.MustCompile(`\[download\]\s+(\d{1,3}(?:\.\d+)?)%`)
	reSpeed   = regexp.MustCompile(` at ([\d.]+\s?(?:KiB|MiB|GiB|kB|MB|GB))/s`)
	reEta     = regexp.MustCompile(` ETA (\d+:\d+)`)
	reMerge   = regexp.MustCompile(`\[(Merger|VideoRemuxer|ExtractAudio|Metadata|VideoConvertor)\]`)
	reYtTitle = regexp.MustCompile(`\[(?:youtube|youtu)\]\s+[A-Za-z0-9_-]+:\s+(.+)`)
)

// pctThrottle admits at most one percent event per interval (final 100%
// always passes). Safe for concurrent use; yt-dlp writes from a pipe goroutine.
type pctThrottle struct {
	mu    sync.Mutex
	last  time.Time
	every time.Duration
}

func (t *pctThrottle) allow(done bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	every := t.every
	if every == 0 {
		every = 250 * time.Millisecond
	}
	if done || time.Since(t.last) >= every {
		t.last = time.Now()
		return true
	}
	return false
}

func parsePct(line string, th *pctThrottle) map[string]any {
	m := rePct.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	pct, _ := strconv.ParseFloat(m[1], 64)
	if !th.allow(pct >= 100) {
		return nil
	}
	msg := map[string]any{"stage": "download", "pct": pct}
	if sp := reSpeed.FindStringSubmatch(line); sp != nil {
		msg["speed"] = sp[1] + "/s"
	}
	if et := reEta.FindStringSubmatch(line); et != nil {
		msg["eta"] = et[1]
	}
	return msg
}

func parseYtDlpLine(line string, th *pctThrottle) map[string]any {
	// Stage lines that carry text.
	if reMerge.MatchString(line) {
		return map[string]any{"stage": "processing"}
	}
	if m := reYtTitle.FindStringSubmatch(line); m != nil && !strings.Contains(line, "Extracting URL") {
		return map[string]any{"stage": "resolved", "title": strings.TrimSpace(m[1])}
	}
	return parsePct(line, th)
}

// ytDlpResolveTimeout bounds the filename-resolution invocation; the
// download itself gets ytDlpDownloadTimeout (long videos over slow links).
var (
	ytDlpResolveTimeout  = 60 * time.Second
	ytDlpDownloadTimeout = 30 * time.Minute
)

// downloadWithYtDlp shells out to yt-dlp to fetch url, using yt-dlp's own
// filename templating, and returns the temp dir holding the download plus
// the resulting filename (relative to that dir), resolved via --print
// filename. Both invocations run under exec.CommandContext so a hung
// yt-dlp (network black hole, spinner) fails the request after the timeout
// instead of pinning the HTTP handler forever. The download lands in a
// temp subdir - never directly over an existing media file. Every progress
// line from the second (download) invocation is passed to onLine, so a
// streaming handler can mirror yt-dlp's own percentage/speed/ETA.
func downloadWithYtDlp(parent context.Context, url string, onLine func(string)) (string, string, error) {
	outputTemplate := "%(title)s.%(ext)s"
	// The default web client currently returns YouTube's "page needs to be
	// reloaded" response in the target environment. Android remains the
	// simplest yt-dlp client that resolves these public videos.
	extractorArgs := "youtube:player_client=android"

	// Scratch space under the data dir's tmp/ — not inside the media dir,
	// which is served statically at /media and scanned as the pool.
	if err := os.MkdirAll(config.TmpDir(), 0o755); err != nil {
		return "", "", err
	}
	tmpDir, err := os.MkdirTemp(config.TmpDir(), "ytdlp-")
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(parent, ytDlpResolveTimeout)
	defer cancel()
	printCmd := exec.CommandContext(ctx, "yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "-o", outputTemplate, "--print", "filename", "--", url)
	printCmd.Dir = tmpDir
	// stdout only: anything yt-dlp says on stderr (notices, deprecation
	// warnings) must not end up in the filename.
	var nameErr strings.Builder
	printCmd.Stderr = &nameErr
	nameOut, err := printCmd.Output()
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("could not resolve output filename: %w: %s", err, strings.TrimSpace(nameErr.String()))
	}
	// --print emits the filename as its own line; take the last one.
	filename := ""
	for _, line := range strings.Split(string(nameOut), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			filename = line
		}
	}
	if filename == "" {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("yt-dlp returned an empty filename")
	}

	// Keep the two-step flow so the final path is known before probing, but
	// force the same single-video selection in both invocations. --newline
	// makes yt-dlp print progress once per line (one line per tick instead of
	// \r-updated), so a line-based parser sees live percentages.
	ctx, cancel = context.WithTimeout(parent, ytDlpDownloadTimeout)
	defer cancel()
	dlCmd := exec.CommandContext(ctx, "yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "--newline", "--progress", "-o", outputTemplate, "--", url)
	dlCmd.Dir = tmpDir

	// One line-writer per stream so a partial line in stdout can never be
	// spliced with a mid-line stderr write; both share one sink (tail +
	// callback) whose mutex serialises the two pump goroutines.
	sink := &lineSink{onLine: onLine, max: 8 << 10}
	dlCmd.Stdout = &lineWriter{sink: sink}
	dlCmd.Stderr = &lineWriter{sink: sink}
	if err := dlCmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("%w: %s", err, strings.TrimSpace(sink.Tail()))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, filename)); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("yt-dlp reported success but output %q is missing: %w", filename, err)
	}

	return tmpDir, filename, nil
}

// lineSink is the state shared by a command's stdout and stderr writers: a
// bounded tail of recent output (for error text) and the per-line callback.
// exec pumps the two streams on separate goroutines, so everything here is
// under mu — including onLine, which writes to the HTTP response.
type lineSink struct {
	mu     sync.Mutex
	tail   strings.Builder
	max    int
	onLine func(string)
}

// Tail returns the retained recent output.
func (s *lineSink) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tail.String()
}

// lineWriter is exec-friendly stdout/stderr plumbing: complete lines are
// handed to the sink's onLine (buffering partial writes per stream).
type lineWriter struct {
	sink *lineSink
	buf  string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	s := w.sink
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tail.Write(p)
	if s.tail.Len() > s.max {
		t := []rune(s.tail.String())
		if len(t) > s.max {
			t = t[len(t)-s.max:] // rune-safe cut: never split a UTF-8 sequence
		}
		s.tail.Reset()
		s.tail.WriteString("…" + string(t))
	}
	w.buf += string(p)
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(w.buf[:i])
		w.buf = w.buf[i+1:]
		if line != "" && s.onLine != nil {
			s.onLine(line)
		}
	}
	return len(p), nil
}

// handleYoutubeRename renames a just-downloaded media file: on-disk rename
// plus pool/cuesheet reconciliation, returning the refreshed media pool
// partial. The extension is preserved when the operator types a bare name.
func handleYoutubeRename(c *gin.Context) {
	old := strings.TrimSpace(c.PostForm("old"))
	name := strings.TrimSpace(c.PostForm("name"))
	if old == "" || name == "" {
		respondError(c, http.StatusBadRequest, "original and new names are required")
		return
	}
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "" || strings.HasPrefix(base, ".") {
		respondError(c, http.StatusBadRequest, "invalid filename")
		return
	}
	if base == old {
		oldPath := filepath.Join(config.MediaLocation(), old)
		if _, err := os.Stat(oldPath); err != nil {
			respondError(c, http.StatusNotFound, "source file not found on disk")
			return
		}
		// No-op: no rename to make, just refresh the pool.
		mediapool, err := mediapoolView()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
		return
	}
	if filepath.Ext(old) != "" && !strings.Contains(base, ".") {
		base += filepath.Ext(old)
	}
	// The new name must still be a media type the pool plays: renaming
	// clip.mp4 to clip.txt would register an unplayable item.
	if media.KindFromExtension(base) != media.KindFromExtension(filepath.Base(old)) {
		respondError(c, http.StatusBadRequest, fmt.Sprintf("%q must keep the %s file type", base, filepath.Ext(old)))
		return
	}
	if gsp.CurrentPlaying() == filepath.Base(old) {
		respondError(c, http.StatusConflict, "can't rename the clip that is playing")
		return
	}
	mediaDir := config.MediaLocation()
	newPath := filepath.Join(mediaDir, base)
	oldFile := filepath.Join(mediaDir, filepath.Base(old))
	if _, err := os.Stat(oldFile); err != nil {
		respondError(c, http.StatusNotFound, "source file not found on disk")
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		respondError(c, http.StatusConflict, fmt.Sprintf("a file named %q already exists", base))
		return
	}
	if err := os.Rename(oldFile, newPath); err != nil {
		logs.PrintfWarn(logs.YDLRename, "old=%q new=%q error=%v", old, base, err)
		respondError(c, http.StatusInternalServerError, "could not rename file on disk: "+err.Error())
		return
	}
	if err := ctp.RenameMedia(filepath.Base(old), base); err != nil {
		// Roll the file back so DB and disk stay consistent.
		_ = os.Rename(newPath, oldFile)
		logs.PrintfWarn(logs.YDLRename, "old=%q new=%q revert=%t error=%v", old, base, true, err)
		respondError(c, http.StatusConflict, err.Error())
		return
	}
	logs.Printf(logs.YDLRename, "old=%q new=%q done", old, base)
	mediapool, err := mediapoolView()
	if err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
}
