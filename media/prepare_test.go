package media

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareArgs(t *testing.T) {
	if _, err := PrepareArgs("a", "b", &MediaVideoInfo{Codec: "prores", Width: 1920, Height: 1080, Alpha: true}, 1920, 1080, 60); err != ErrNotPreparable {
		t.Fatalf("alpha video: err %v, want ErrNotPreparable", err)
	}
	args, err := PrepareArgs("a", "b", &MediaVideoInfo{Codec: "h264", Width: 3840, Height: 2160, FPS: 59.94}, 1920, 1080, 60)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"scale=w=1920:h=1080", "keyint=30", "libx265", "format=yuv420p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("4K h264: args lack %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "fps=") {
		t.Errorf("59.94 fps on a 60 Hz display must not be rate-converted: %s", joined)
	}
	args, _ = PrepareArgs("a", "b", &MediaVideoInfo{Codec: "mpeg2video", Width: 1920, Height: 1080, FPS: 29.97, Interlaced: true}, 1920, 1080, 60)
	if joined = strings.Join(args, " "); !strings.Contains(joined, "bwdif=mode=send_field") || strings.Contains(joined, "scale=") {
		t.Errorf("1080i: want bwdif and no scale: %s", joined)
	}
}

// End to end: a 4K H.264 clip becomes a 1080p HEVC copy that Prepared finds,
// and that goes stale when the source changes.
func TestPrepareVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg has no libx265")
	}
	dir := t.TempDir()
	PreparedDir = filepath.Join(dir, "prepared")
	defer func() { PreparedDir = "" }()
	src := filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=2",
		"-f", "lavfi", "-i", "sine=d=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src).CombinedOutput(); err != nil {
		t.Skipf("cannot make clip: %v: %s", err, out)
	}
	v := &MediaVideoInfo{Codec: "h264", Width: 640, Height: 360, FPS: 30}
	dst := PreparedPath("clip.mp4")
	if err := PrepareVideo(context.Background(), src, dst, v, 320, 180, 60); err != nil {
		t.Fatal(err)
	}
	if got := Prepared(dir, "clip.mp4"); got != dst {
		t.Fatalf("Prepared = %q, want %q", got, dst)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height", "-of", "json", dst).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Streams) != 2 || probe.Streams[0].Codec != "hevc" || probe.Streams[0].Width != 320 || probe.Streams[0].Height != 180 || probe.Streams[1].Codec != "flac" {
		t.Fatalf("prepared streams: %+v", probe.Streams)
	}
	// A newer source makes the copy stale.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if got := Prepared(dir, "clip.mp4"); got != "" {
		t.Fatalf("stale copy still used: %q", got)
	}
}

func TestPreparerQueue(t *testing.T) {
	ran := make(chan string, 4)
	p := &Preparer{Run: func(ctx context.Context, f string) error {
		ran <- f
		if f == "alpha.mov" {
			return ErrNotPreparable
		}
		return nil
	}}
	p.Enqueue([]string{"a.mp4", "alpha.mov", "a.mp4"})
	for i := 0; i < 2; i++ {
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatal("queue did not run")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs := p.Jobs()
		if len(jobs) == 2 && jobs[0].State == "done" && jobs[1].State == "skipped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs: %+v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
