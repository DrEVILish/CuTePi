package routes

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/logs"
	"CuTePi/media"
)

func Youtube(rg *gin.RouterGroup) {
	rg.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "youtube pong")
	})

	rg.POST("", handleYoutubeDownload)
	rg.POST("/", handleYoutubeDownload)
}

func handleYoutubeDownload(c *gin.Context) {
	url := strings.TrimSpace(c.PostForm("url"))
	logs.Printf(logs.YDLRequest, "stage=request method=%s path=%s url=%q", c.Request.Method, c.Request.URL.Path, url)
	if url == "" {
		logs.Printf(logs.YDLFailed, "stage=validate reason=empty_url")
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "no URL provided"})
		return
	}

	logs.Printf(logs.YDLResolve, "stage=start url=%q media_dir=%q", url, config.MediaLocation())
	filename, err := downloadWithYtDlp(url)
	if err != nil {
		logs.Printf(logs.YDLFailed, "stage=resolve_or_download url=%q error=%v", url, err)
		c.HTML(http.StatusUnprocessableEntity, "error.html", gin.H{
			"error": fmt.Sprintf("download failed: %v", err),
		})
		return
	}
	logs.Printf(logs.YDLDownload, "stage=complete filename=%q", filename)

	destPath := filepath.Join(config.MediaLocation(), filename)
	logs.Printf(logs.YDLProbe, "stage=start filename=%q path=%q", filename, destPath)
	meta, err := media.Probe(destPath)
	if err != nil {
		logs.Printf(logs.YDLFailed, "stage=probe filename=%q error=%v", filename, err)
		c.HTML(http.StatusUnprocessableEntity, "error.html", gin.H{"error": err.Error()})
		return
	}

	size, err := fileSize(destPath)
	if err != nil {
		logs.Printf(logs.YDLFailed, "stage=stat filename=%q error=%v", filename, err)
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}

	title := strings.TrimSuffix(filename, filepath.Ext(filename))
	logs.Printf(logs.YDLRegister, "stage=start filename=%q title=%q size=%d mimetype=%q duration=%.3f", filename, title, size, meta.Mimetype, meta.Duration)
	if err := ctp.RegisterMedia(filename, size, meta, title); err != nil {
		logs.Printf(logs.YDLFailed, "stage=register filename=%q error=%v", filename, err)
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}

	mediapool, err := mediapoolView()
	if err != nil {
		logs.Printf(logs.YDLFailed, "stage=render filename=%q error=%v", filename, err)
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
		return
	}
	logs.Printf(logs.YDLRender, "stage=complete filename=%q media_count=%d", filename, len(mediapool))
	c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
}

// downloadWithYtDlp shells out to yt-dlp to fetch url into the media
// directory, using yt-dlp's own filename templating, then returns the
// resulting filename (relative to the media directory) by asking yt-dlp
// for it directly via --print filename.
func downloadWithYtDlp(url string) (string, error) {
	outputTemplate := "%(title)s.%(ext)s"
	// The default web client currently returns YouTube's "page needs to be
	// reloaded" response in the target environment. Android remains the
	// simplest yt-dlp client that resolves these public videos.
	extractorArgs := "youtube:player_client=android"

	printCmd := exec.Command("yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "-o", outputTemplate, "--print", "filename", url)
	printCmd.Dir = config.MediaLocation()
	nameOut, err := printCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("could not resolve output filename: %w: %s", err, strings.TrimSpace(string(nameOut)))
	}
	filename := strings.TrimSpace(string(nameOut))
	if filename == "" {
		return "", fmt.Errorf("yt-dlp returned an empty filename")
	}

	// Keep the two-step flow so the final path is known before probing, but
	// force the same single-video selection in both invocations.
	dlCmd := exec.Command("yt-dlp", "--extractor-args", extractorArgs, "--restrict-filenames", "--no-playlist", "--no-warnings", "--no-progress", "-o", outputTemplate, url)
	dlCmd.Dir = config.MediaLocation()
	if out, err := dlCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(config.MediaLocation(), filename)); err != nil {
		return "", fmt.Errorf("yt-dlp reported success but output %q is missing: %w", filename, err)
	}

	return filename, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
