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
	"CuTePi/logs"
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
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "no URL provided"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	writeLine := func(msg map[string]any) bool {
		body, _ := json.Marshal(msg)
		_, err := c.Writer.Write(append(body, '\n'))
		c.Writer.Flush()
		return err == nil
	}
	stage := func(s string) { writeLine(map[string]any{"stage": s}) }
	fail := func(code string, msg string) {
		logs.PrintfWarn(code, "url=%q error=%s", url, msg)
		writeLine(map[string]any{"error": msg})
	}

	stage("resolving")
	th := &pctThrottle{every: 250 * time.Millisecond}
	tmpDir, filename, err := downloadWithYtDlp(url, func(line string) {
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
func downloadWithYtDlp(url string, onLine func(string)) (string, string, error) {
	outputTemplate := "%(title)s.%(ext)s"
	// The default web client currently returns YouTube's "page needs to be
	// reloaded" response in the target environment. Android remains the
	// simplest yt-dlp client that resolves these public videos.
	extractorArgs := "youtube:player_client=android"

	tmpDir, err := os.MkdirTemp(config.MediaLocation(), "ytdlp-")
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), ytDlpResolveTimeout)
	defer cancel()
	printCmd := exec.CommandContext(ctx, "yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "-o", outputTemplate, "--print", "filename", "--", url)
	printCmd.Dir = tmpDir
	nameOut, err := printCmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("could not resolve output filename: %w: %s", err, strings.TrimSpace(string(nameOut)))
	}
	filename := strings.TrimSpace(string(nameOut))
	if filename == "" {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("yt-dlp returned an empty filename")
	}

	// Keep the two-step flow so the final path is known before probing, but
	// force the same single-video selection in both invocations. --newline
	// makes yt-dlp print progress once per line (one line per tick instead of
	// \r-updated), so a line-based parser sees live percentages.
	ctx, cancel = context.WithTimeout(context.Background(), ytDlpDownloadTimeout)
	defer cancel()
	dlCmd := exec.CommandContext(ctx, "yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "--newline", "--progress", "-o", outputTemplate, "--", url)
	dlCmd.Dir = tmpDir

	var tail strings.Builder
	if onLine != nil {
		// One line-writer per stream so a partial line in stdout can never
		// be spliced with a mid-line stderr write; progress (stdout) and
		// stage lines (stderr) share the tail for errors.
		wout := &lineWriter{onLine: onLine, tail: &tail, max: 8 << 10}
		werr := &lineWriter{onLine: onLine, tail: &tail, max: 8 << 10}
		dlCmd.Stdout = wout
		dlCmd.Stderr = werr
	}
	if err := dlCmd.Run(); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("%w: %s", err, strings.TrimSpace(tail.String()))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, filename)); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("yt-dlp reported success but output %q is missing: %w", filename, err)
	}

	return tmpDir, filename, nil
}

// lineWriter is exec-friendly stdout/stderr plumbing: complete lines are
// handed to onLine (buffering partial writes); a bounded tail keeps the last
// output for error text.
type lineWriter struct {
	mu     sync.Mutex
	buf    string
	onLine func(string)
	tail   *strings.Builder
	max    int
}

func (w *lineWriter) Write(p []byte) (int, error) {
	s := string(p)
	if w.tail != nil {
		w.tail.WriteString(s)
		if w.tail.Len() > w.max {
			t := w.tail.String()
			w.tail.Reset()
			w.tail.WriteString("…" + t[len(t)-w.max:])
		}
	}
	w.mu.Lock()
	w.buf += s
	var lines []string
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, strings.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	w.mu.Unlock()
	if w.onLine != nil {
		for _, l := range lines {
			if l != "" {
				w.onLine(l)
			}
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
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "original and new names are required"})
		return
	}
	base := filepath.Base(name)
	if base == "." || base == "" {
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "invalid filename"})
		return
	}
	if base == old {
		oldPath := filepath.Join(config.MediaLocation(), old)
		if _, err := os.Stat(oldPath); err != nil {
			c.HTML(http.StatusNotFound, "error.html", gin.H{"error": "source file not found on disk"})
			return
		}
		// No-op: no rename to make, just refresh the pool.
		mediapool, err := mediapoolView()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
		return
	}
	if filepath.Ext(old) != "" && !strings.Contains(base, ".") {
		base += filepath.Ext(old)
	}
	mediaDir := config.MediaLocation()
	oldPath := filepath.Join(mediaDir, base)
	oldFile := filepath.Join(mediaDir, filepath.Base(old))
	if _, err := os.Stat(oldFile); err != nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{"error": "source file not found on disk"})
		return
	}
	if _, err := os.Stat(oldPath); err == nil {
		c.HTML(http.StatusConflict, "error.html", gin.H{"error": fmt.Sprintf("a file named %q already exists", base)})
		return
	}
	if err := os.Rename(oldFile, oldPath); err != nil {
		logs.PrintfWarn(logs.YDLRename, "old=%q new=%q error=%v", old, base, err)
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": "could not rename file on disk: " + err.Error()})
		return
	}
	if err := ctp.RenameMedia(filepath.Base(old), base); err != nil {
		// Roll the file back so DB and disk stay consistent.
		_ = os.Rename(oldPath, oldFile)
		logs.PrintfWarn(logs.YDLRename, "old=%q new=%q revert=%t error=%v", old, base, true, err)
		c.HTML(http.StatusConflict, "error.html", gin.H{"error": err.Error()})
		return
	}
	logs.Printf(logs.YDLRename, "old=%q new=%q done", old, base)
	mediapool, err := mediapoolView()
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}
	c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
}
