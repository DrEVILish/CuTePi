package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Show compiler (PERFORMANCE_PROPOSALS Proposal 2, DESIGN §12.17): a video
// the playback warning flags gets a prepared copy in the form this Pi decodes
// in hardware, HEVC Main 8-bit, no larger than the display, keyframes every
// half second, progressive. The original is kept; a cue plays the copy while
// it is newer than the source. Alpha video is not prepared (an HEVC copy would
// need the colour + matte recombined on the GPU, which the wall has not got).

// PreparedDir is where prepared copies live (set by main: a sibling of the
// thumbnails directory). "" disables the feature.
var PreparedDir string

// PreparedPath is filename's prepared copy, whether or not it exists.
func PreparedPath(filename string) string {
	if PreparedDir == "" || filename == "" || filepath.IsAbs(filename) {
		return ""
	}
	return filepath.Join(PreparedDir, filename+".hevc.mkv")
}

// Prepared is the prepared copy of filename (in mediaDir) when it exists and
// is newer than the source, else "".
func Prepared(mediaDir, filename string) string {
	p := PreparedPath(filename)
	if p == "" {
		return ""
	}
	ps, err := os.Stat(p)
	if err != nil {
		return ""
	}
	ss, err := os.Stat(filepath.Join(mediaDir, filename))
	if err != nil || !ps.ModTime().After(ss.ModTime()) {
		return ""
	}
	return p
}

// ErrNotPreparable: the file is not a video the compiler handles.
var ErrNotPreparable = errors.New("media: not preparable (no video, or video with alpha)")

// PrepareArgs are ffmpeg's arguments for v (src) at a w x h, hz display.
func PrepareArgs(src, dst string, v *MediaVideoInfo, w, h, hz int) ([]string, error) {
	if v == nil || v.Alpha || v.Width <= 0 || v.Height <= 0 {
		return nil, ErrNotPreparable
	}
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if hz <= 0 {
		hz = 60
	}
	var vf []string
	fps := v.FPS
	if v.Interlaced {
		// One picture per field, as the wall shows interlaced video (§6.1.4).
		vf = append(vf, "bwdif=mode=send_field")
		fps *= 2
	}
	if fps <= 0 || fps > float64(hz) {
		vf = append(vf, fmt.Sprintf("fps=%d", hz))
		fps = float64(hz)
	}
	if v.Width > w || v.Height > h {
		// Fit inside the display, aspect kept, even sizes (no padding: the
		// cue's own fit/geometry still applies to the picture).
		vf = append(vf, fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:force_divisible_by=2", w, h))
	}
	vf = append(vf, "format=yuv420p")
	gop := int(fps/2 + 0.5)
	if gop < 1 {
		gop = 1
	}
	return []string{"-y", "-nostdin", "-v", "error", "-i", src,
		"-map", "0:v:0", "-map", "0:a?",
		"-vf", strings.Join(vf, ","),
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "16",
		"-x265-params", fmt.Sprintf("keyint=%d:min-keyint=%d:no-open-gop=1:scenecut=0:log-level=error", gop, gop),
		"-c:a", "flac", "-f", "matroska", dst}, nil
}

// PrepareVideo encodes src's prepared copy to dst (via dst.part). The encoder
// is held (SIGSTOP) whenever PlaybackBusy reports playback and resumed when
// the wall is idle again, so preparing never takes CPU from a show.
func PrepareVideo(ctx context.Context, src, dst string, v *MediaVideoInfo, w, h, hz int) error {
	tmp := dst + ".part"
	args, err := PrepareArgs(src, tmp, v, w, h, hz)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, lookPath("nice"), append([]string{"-n", "19", lookPath("ffmpeg")}, args...)...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	held := false
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				os.Remove(tmp)
				return fmt.Errorf("media: preparing %q: %w: %s", filepath.Base(src), err, strings.TrimSpace(out.String()))
			}
			return os.Rename(tmp, dst)
		case <-tick.C:
			busy := PlaybackBusy != nil && PlaybackBusy()
			if busy != held && cmd.Process != nil {
				sig := syscall.SIGCONT
				if busy {
					sig = syscall.SIGSTOP
				}
				_ = cmd.Process.Signal(sig)
				held = busy
			}
		case <-ctx.Done():
			if held && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGCONT) // let the kill land
			}
			<-done
			os.Remove(tmp)
			return ctx.Err()
		}
	}
}

// PrepareJob is one file in the compiler's queue, as the UI shows it.
type PrepareJob struct {
	Filename string `json:"filename"`
	State    string `json:"state"` // queued, preparing, done, failed, skipped
	Error    string `json:"error,omitempty"`
}

// Preparer runs prepare jobs one at a time, in order.
type Preparer struct {
	mu      sync.Mutex
	jobs    []*PrepareJob
	running bool
	cancel  context.CancelFunc
	// Run prepares one file (set by the caller: it knows the media dir, the
	// probe and the display).
	Run func(ctx context.Context, filename string) error
}

// Enqueue adds files not already queued or running, and starts the queue.
func (p *Preparer) Enqueue(files []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range files {
		dup := false
		for _, j := range p.jobs {
			if j.Filename == f && (j.State == "queued" || j.State == "preparing") {
				dup = true
			}
		}
		if !dup {
			p.jobs = append(p.jobs, &PrepareJob{Filename: f, State: "queued"})
		}
	}
	if !p.running {
		p.running = true
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		go p.loop(ctx)
	}
}

func (p *Preparer) loop(ctx context.Context) {
	for {
		p.mu.Lock()
		var j *PrepareJob
		for _, c := range p.jobs {
			if c.State == "queued" {
				j = c
				break
			}
		}
		if j == nil || ctx.Err() != nil {
			p.running = false
			p.mu.Unlock()
			return
		}
		j.State = "preparing"
		p.mu.Unlock()
		err := p.Run(ctx, j.Filename)
		p.mu.Lock()
		switch {
		case errors.Is(err, ErrNotPreparable):
			j.State, j.Error = "skipped", err.Error()
		case err != nil:
			j.State, j.Error = "failed", err.Error()
		default:
			j.State = "done"
		}
		p.mu.Unlock()
	}
}

// Cancel stops the queue: the running encode is killed, queued jobs dropped.
func (p *Preparer) Cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	for _, j := range p.jobs {
		if j.State == "queued" || j.State == "preparing" {
			j.State, j.Error = "failed", "cancelled"
		}
	}
}

// Jobs is a snapshot of the queue.
func (p *Preparer) Jobs() []PrepareJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PrepareJob, len(p.jobs))
	for i, j := range p.jobs {
		out[i] = *j
	}
	return out
}
