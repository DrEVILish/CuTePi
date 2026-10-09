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
	"sync"
	"syscall"
	"time"

	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/ws"
)

// URL imports keep the site's own format when it plays at 1080p60 here, and
// are converted once, at import, when it does not (DESIGN §12.15). Which
// codecs play at 1080p60 comes from the codec support tests on this hardware
// (README, "Codec support"; YouTube downloads in h264-pi4 results/43), per
// display path:
//
//   - GPU wall: H.264 8-bit (hardware: YouTube 1080p60 60.2 fps steady and
//     through fades), HEVC 8/10-bit (hardware, 60), VP8, MPEG-2, MPEG-1,
//     MPEG-4 Part 2, MJPEG (software, 60). Not: VP9 (YouTube 59.8 steady,
//     fade-out 21), AV1 (YouTube 49.4 with dav1d), ProRes, CineForm, Theora.
//   - KMS wall: H.264 8-bit (hardware, 59.8 steady), VP8, MPEG-2, MPEG-4
//     Part 2, MJPEG, Theora (software, 59.2-60); not HEVC (its 128-column
//     frames cannot go on a plane as they are: 0.8 fps). Fades on this wall
//     are ~30 steps/s for every codec (one kmssink per layer).
//
// The conversion target is the best measured option on the display path in
// use: HEVC on the GPU wall (hardware decode, 44.6 dB at 7.9 Mbit/s for
// 1080p60 with x265 superfast CRF 18), H.264 on the KMS wall (hardware
// decode). A conversion never runs while anything plays: it waits for the
// wall to be idle, and is paused (SIGSTOP) the moment playback starts and
// resumed when it ends. CUTEPI_YTDLP_TRANSCODE=0 turns conversions off.

// plays60 reports whether a video codec (ffprobe codec_name) plays a 1080p60
// file at 60 fps on the display path in use.
func plays60(codec string, gpuWall bool) bool {
	switch codec {
	case "h264", "vp8", "mpeg2video", "mpeg1video", "mpeg4", "mjpeg":
		return true
	case "hevc":
		return gpuWall
	case "theora":
		return !gpuWall
	}
	return false
}

func conversionOn() bool { return os.Getenv("CUTEPI_YTDLP_TRANSCODE") != "0" }

// convertArgs are ffmpeg's video arguments for the conversion target, and
// the target's codec name.
func convertArgs(gpuWall, deep bool) ([]string, string) {
	if gpuWall {
		pix := "yuv420p"
		if deep {
			pix = "yuv420p10le" // Main 10 decodes in hardware too
		}
		return []string{"-c:v", "libx265", "-preset", "superfast", "-crf", "18", "-pix_fmt", pix, "-x265-params", "log-level=error"}, "hevc"
	}
	// The KMS wall's hardware path is 8-bit H.264.
	return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p"}, "h264"
}

// ytEncodeTimeout bounds one conversion's running time (paused time excluded
// is not tracked: a long show may hold a conversion paused for hours).
var ytEncodeTimeout = 24 * time.Hour

// probeVideoCodec returns the codec name of the file's first video stream
// ("" for none).
func probeVideoCodec(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", "--", path).Output()
	return strings.TrimSpace(string(out)), err
}

// probeVideo returns the duration in seconds and whether the first video
// stream has more than 8 bits per sample.
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

// playbackRunning: anything on the wall or any cue running (a variable for
// tests).
var playbackRunning = func() bool { return !gsp.WallIdle() || len(gsp.RunningCues()) > 0 }

// convJob is one queued conversion. The import request that queued it
// listens while it is connected; the job runs on without it.
type convJob struct {
	tmpDir, src, from string
	mu                sync.Mutex
	listen            func(map[string]any)
	done              chan struct{}
}

func (j *convJob) event(m map[string]any) {
	j.mu.Lock()
	f := j.listen
	j.mu.Unlock()
	if f != nil {
		f(m)
	}
}

func (j *convJob) setListener(f func(map[string]any)) {
	j.mu.Lock()
	j.listen = f
	j.mu.Unlock()
}

var (
	convOnce  sync.Once
	convQueue = make(chan *convJob, 64)
)

// queueConversion hands a downloaded file (src in tmpDir, which the job then
// owns) to the conversion worker.
func queueConversion(tmpDir, src, from string) *convJob {
	convOnce.Do(func() { go convWorker() })
	j := &convJob{tmpDir: tmpDir, src: src, from: from, done: make(chan struct{})}
	convQueue <- j
	return j
}

func convWorker() {
	for j := range convQueue {
		runConversion(j)
		close(j.done)
	}
}

// runConversion waits for an idle wall, converts (paused whenever playback
// runs), then imports the result into the pool and tells every client.
func runConversion(j *convJob) {
	defer os.RemoveAll(j.tmpDir)
	name := filepath.Base(j.src)
	for playbackRunning() {
		j.event(map[string]any{"stage": "queued", "from": j.from, "paused": true})
		time.Sleep(time.Second)
	}
	gpuWall := gsp.GPUWall()
	ctx, cancel := context.WithTimeout(context.Background(), ytEncodeTimeout)
	defer cancel()
	dur, deep := probeVideo(ctx, j.src)
	vargs, to := convertArgs(gpuWall, deep)
	dst := strings.TrimSuffix(j.src, filepath.Ext(j.src)) + ".converted.mkv"
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", j.src,
		"-map", "0:v:0", "-map", "0:a?", "-map", "0:s?"}, vargs...)
	args = append(args, "-c:a", "copy", "-c:s", "copy", "-progress", "pipe:1", "-nostats", dst)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		j.event(map[string]any{"error": fmt.Sprintf("conversion could not start: %v", err)})
		return
	}
	logs.Printf(logs.YDLDownload, "stage=convert from=%s to=%s file=%q", j.from, to, name)

	// Pause while anything plays (SIGSTOP), resume when the wall is idle.
	stopWatch := make(chan struct{})
	var paused sync.Mutex
	isPaused := false
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-t.C:
				busy := playbackRunning()
				paused.Lock()
				if busy && !isPaused {
					cmd.Process.Signal(syscall.SIGSTOP)
					isPaused = true
					logs.Printf(logs.YDLDownload, "stage=convert paused (playback) file=%q", name)
					j.event(map[string]any{"stage": "encoding", "from": j.from, "to": to, "paused": true})
				} else if !busy && isPaused {
					cmd.Process.Signal(syscall.SIGCONT)
					isPaused = false
					logs.Printf(logs.YDLDownload, "stage=convert resumed file=%q", name)
				}
				paused.Unlock()
			}
		}
	}()
	start := time.Now()
	var lastEv time.Time
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		if k != "out_time_us" || dur <= 0 {
			continue
		}
		us, err := strconv.ParseFloat(v, 64)
		if err != nil || us <= 0 || time.Since(lastEv) < time.Second {
			continue
		}
		lastEv = time.Now()
		pct := min(100, us/1e6/dur*100)
		eta := ""
		if pct > 0.5 {
			left := time.Since(start).Seconds() * (100 - pct) / pct
			eta = fmt.Sprintf("%d:%02d", int(left)/60, int(left)%60)
		}
		j.event(map[string]any{"stage": "encoding", "from": j.from, "to": to, "pct": pct, "eta": eta})
	}
	err = cmd.Wait()
	close(stopWatch)
	if err != nil {
		os.Remove(dst)
		msg := fmt.Sprintf("conversion to %s failed: %v: %s", to, err, strings.TrimSpace(stderr.String()))
		logs.PrintfWarn(logs.YDLFailed, "file=%q %s", name, msg)
		j.event(map[string]any{"error": msg})
		return
	}
	// "Title.webm" -> "Title.mkv" in the pool.
	final := strings.TrimSuffix(name, filepath.Ext(name)) + ".mkv"
	final, release, err := reserveMediaName(final, reserveRename)
	if err != nil {
		os.Remove(dst)
		j.event(map[string]any{"error": err.Error()})
		return
	}
	defer release()
	j.event(map[string]any{"stage": "importing"})
	if err := importMedia(final, dst); err != nil {
		j.event(map[string]any{"error": err.Error()})
		return
	}
	logs.Printf(logs.YDLRender, "stage=complete filename=%q converted from=%s to=%s in %v", final, j.from, to, time.Since(start).Round(time.Second))
	ws.BroadcastMedia()
	j.event(map[string]any{"done": true, "filename": final, "converted": to})
}
