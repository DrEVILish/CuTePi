package routes

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HEVC on import (yt-dlp downloads). Sites rarely serve HEVC (YouTube sends
// AV1, VP9 or H.264), and on a Pi 4 HEVC is the one video codec that plays at
// 1080p60 through fades (README codec table: H.264 58.4 fps, VP9 41, AV1 21).
// So a download that isn't HEVC is re-encoded once, at import, with x265:
// CRF 18 "superfast" measured 44.6 dB PSNR at 1080p60 (h264-pi4 OPEN-CODECS.md)
// and encodes at about 10 fps on a Pi 4 at 2 GHz, so a 1080p60 video takes
// about six times its length. Audio and subtitles are copied unchanged into
// MKV, which carries any of them. CUTEPI_YTDLP_HEVC=0 keeps downloads as
// they come.

// ytEncodeTimeout bounds one conversion (a 1 h 1080p60 video takes ~6 h).
var ytEncodeTimeout = 8 * time.Hour

func hevcOnImport() bool { return os.Getenv("CUTEPI_YTDLP_HEVC") != "0" }

// probeVideoCodec returns the codec name of the file's first video stream
// ("" for none).
func probeVideoCodec(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", "--", path).Output()
	return strings.TrimSpace(string(out)), err
}

// probeVideo returns the duration in seconds and whether the first video
// stream has more than 8 bits per sample (kept as 10-bit HEVC, Main 10).
func probeVideo(ctx context.Context, path string) (float64, bool) {
	out, _ := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt:format=duration", "-of", "default=nw=1", "--", path).Output()
	var dur float64
	deep := false
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "duration":
			dur, _ = strconv.ParseFloat(v, 64)
		case "pix_fmt":
			deep = strings.Contains(v, "10") || strings.Contains(v, "12")
		}
	}
	return dur, deep
}

// hevcTranscode re-encodes src to HEVC next to it (same name, .mkv) and
// returns the new path; src is removed on success. progress gets the percent
// done and an ETA (mm:ss) from ffmpeg's -progress output.
func hevcTranscode(parent context.Context, src string, progress func(pct float64, eta string)) (string, error) {
	ctx, cancel := context.WithTimeout(parent, ytEncodeTimeout)
	defer cancel()
	dur, deep := probeVideo(ctx, src)
	dst := strings.TrimSuffix(src, filepath.Ext(src)) + ".hevc.mkv"
	pixFmt := "yuv420p"
	if deep {
		pixFmt = "yuv420p10le"
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-map", "0:v:0", "-map", "0:a?", "-map", "0:s?",
		"-c:v", "libx265", "-preset", "superfast", "-crf", "18", "-pix_fmt", pixFmt,
		"-x265-params", "log-level=error", "-c:a", "copy", "-c:s", "copy",
		"-progress", "pipe:1", "-nostats", dst)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		if k != "out_time_us" || dur <= 0 {
			continue
		}
		us, err := strconv.ParseFloat(v, 64)
		if err != nil || us <= 0 {
			continue
		}
		pct := min(100, us/1e6/dur*100)
		eta := ""
		if pct > 0.5 {
			left := time.Since(start).Seconds() * (100 - pct) / pct
			eta = fmt.Sprintf("%d:%02d", int(left)/60, int(left)%60)
		}
		progress(pct, eta)
	}
	if err := cmd.Wait(); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	progress(100, "0:00")
	// The name loses yt-dlp's original extension: "Title.webm" -> "Title.mkv".
	final := strings.TrimSuffix(src, filepath.Ext(src)) + ".mkv"
	os.Remove(src)
	if err := os.Rename(dst, final); err != nil {
		return "", err
	}
	return final, nil
}
