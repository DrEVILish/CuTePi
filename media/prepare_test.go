package media

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestPrepareRegressionPrepareWhileBusy(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "started")
	fake := "#!/bin/sh\necho started > '" + mark + "'\nexec /bin/sleep 3\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	old := PlaybackBusy
	PlaybackBusy = func() bool { return true }
	defer func() { PlaybackBusy = old }()
	ctx, cancel := context.WithCancel(context.Background())
	if err := os.WriteFile(filepath.Join(dir, "src"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- PrepareVideo(ctx, filepath.Join(dir, "src"), filepath.Join(dir, "out"), &MediaVideoInfo{Width: 64, Height: 64, FPS: 30}, 64, 64, 60)
	}()
	time.Sleep(100 * time.Millisecond)
	_, err := os.Stat(mark)
	cancel()
	<-done
	if err == nil {
		t.Fatal("encoder launched while playback was already busy")
	}
}

func TestPrepareRejectsSourceReplacedDuringEncode(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "prepared")
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	if err := os.WriteFile(src, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nfor last; do :; done\ntouch '" + started + "'\nwhile [ ! -e '" + release + "' ]; do sleep 0.01; done\nprintf prepared > \"$last\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- PrepareVideo(ctx, src, dst, &MediaVideoInfo{Width: 64, Height: 64, FPS: 30}, 64, 64, 60)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("encoder never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(src+".new", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src+".new", src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("obsolete encode was reported successful")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("obsolete prepared copy was published: %v", err)
	}
}

func TestPreparerCancelDrainsBeforeRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"success", nil},
		{"cancelled", context.Canceled},
		{"skipped", ErrNotPreparable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type start struct {
				filename string
				err      error
				active   int32
			}
			started := make(chan start, 3)
			cancelled, release := make(chan struct{}), make(chan struct{})
			var calls, active atomic.Int32
			p := &Preparer{Run: func(ctx context.Context, filename string) error {
				n := active.Add(1)
				defer active.Add(-1)
				first := calls.Add(1) == 1
				started <- start{filename, ctx.Err(), n}
				if first {
					<-ctx.Done()
					close(cancelled)
					<-release
					return tc.err
				}
				return nil
			}}
			t.Cleanup(func() {
				p.Cancel()
				select {
				case <-release:
				default:
					close(release)
				}
			})
			p.Enqueue([]string{"clip.mp4", "old-pending.mp4"})
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("first job did not start")
			}
			p.Cancel()
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("running job did not receive cancellation")
			}
			p.Enqueue([]string{"clip.mp4", "clip.mp4"})
			if jobs := p.Jobs(); len(jobs) != 3 || jobs[2].State != "queued" {
				t.Fatalf("retry must queue once while cancelled job drains: %+v", jobs)
			}
			select {
			case got := <-started:
				t.Fatalf("job started before cancelled runner drained: %+v", got)
			default:
			}
			close(release)
			select {
			case got := <-started:
				if got.filename != "clip.mp4" || got.err != nil || got.active != 1 {
					t.Errorf("retry must run alone with fresh context: %+v", got)
				}
				deadline := time.Now().Add(2 * time.Second)
				for p.Jobs()[2].State == "queued" || p.Jobs()[2].State == "preparing" {
					if time.Now().After(deadline) {
						t.Fatalf("retry did not finish: %+v", p.Jobs())
					}
					time.Sleep(time.Millisecond)
				}
				if jobs := p.Jobs(); jobs[2].State != "done" {
					t.Errorf("retry failed: %+v", jobs)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("retry stayed queued after cancelled runner drained: %+v", p.Jobs())
			}
			for _, job := range p.Jobs()[:2] {
				if job.State != "failed" || job.Error != "cancelled" {
					t.Errorf("cancelled job result overwritten: %+v", job)
				}
			}
		})
	}
}
