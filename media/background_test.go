package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Import and conversion tools run at nice 19 and best-effort I/O level 7,
// with the tool's own PID (nice and ionice exec it in place).
func TestBackgroundCommand(t *testing.T) {
	if bgNice == "" || bgIonice == "" {
		t.Skip("nice or ionice not installed")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	out, err := BackgroundCommand(context.Background(), "sh", "-c", "cat /proc/$$/stat; ionice -p $$").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(out), "\n", 2)
	f := strings.Fields(lines[0][strings.LastIndex(lines[0], ")")+1:])
	if comm := lines[0][strings.Index(lines[0], "(")+1 : strings.LastIndex(lines[0], ")")]; comm != "sh" {
		t.Errorf("process is %q, want the tool itself (sh)", comm)
	}
	if nice := f[16]; nice != "19" {
		t.Errorf("nice %s, want 19", nice)
	}
	if io := strings.TrimSpace(lines[1]); io != "best-effort: prio 7" {
		t.Errorf("I/O priority %q, want best-effort: prio 7", io)
	}
}

// While something plays, the import's decodes run on one thread.
func TestBusyThreads(t *testing.T) {
	saved := PlaybackBusy
	defer func() { PlaybackBusy = saved }()
	PlaybackBusy = nil
	if a := busyThreads(); a != nil {
		t.Errorf("no hook: %v, want none", a)
	}
	PlaybackBusy = func() bool { return false }
	if a := busyThreads(); a != nil {
		t.Errorf("idle: %v, want none", a)
	}
	PlaybackBusy = func() bool { return true }
	if a := strings.Join(busyThreads(), " "); a != "-threads 1 -filter_threads 1" {
		t.Errorf("playing: %q", a)
	}
}

// A stored file's kind follows its probed mimetype, else its extension.
func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		name, mime string
		want       Kind
	}{
		{"a.mkv", "audio/matroska,webm", KindAudio}, {"a.mov", "audio/mov,mp4,m4a,3gp,3g2,mj2", KindAudio},
		{"v.mkv", "video/matroska,webm", KindVideo}, {"v.mkv", "", KindVideo},
		{"x.wav", "", KindAudio}, {"p.png", "image/png", KindImage},
	} {
		if got := KindOf(c.name, c.mime); got != c.want {
			t.Errorf("KindOf(%q, %q) = %v, want %v", c.name, c.mime, got, c.want)
		}
	}
}

// A video thumbnail is the keyframe at or before the seek point; it works for
// codecs that refuse keyframe-only decoding with frame threads (Theora).
func TestVideoThumbnailKeyframe(t *testing.T) {
	if bgNice == "" {
		t.Skip("no nice")
	}
	dir := t.TempDir()
	for _, c := range []struct{ name, codec string }{{"t.mkv", "libtheora"}, {"v.mkv", "libvpx-vp9"}, {"m.mkv", "mpeg2video"}} {
		src := filepath.Join(dir, c.name)
		if b, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25", "-t", "4",
			"-g", "25", "-c:v", c.codec, src).CombinedOutput(); err != nil {
			t.Skipf("%s: %v %s", c.codec, err, b)
		}
		out := src + ".jpg"
		if err := GenerateThumbnail(src, KindVideo, 4, out); err != nil {
			t.Errorf("%s: %v", c.codec, err)
		} else if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			t.Errorf("%s: no thumbnail", c.codec)
		}
	}
}
