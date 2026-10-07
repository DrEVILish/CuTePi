package routes

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"
)

// End-to-end playback through the exact endpoints the WebUI drives: a row
// click selects (POST /api/cue/:pos) and both Space and the GO button fire
// POST /api/cue/selected/play. Asserts frames actually flow (position
// advances — a stalled wall sink parks at 0 with no error), the end arrives
// (no bus dispatch means it parks at duration forever) and the pipeline
// tears down. Needs real GStreamer + ffmpeg; the wall sink is fakesink so
// it runs headless anywhere. The 10 s H264 fixture exercises real decoder
// negotiation through the flips/converters.
func TestSelectedPlayRunsToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build a playback fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping live playback test")
	}
	// The fixture is H264: without a real H264 decoder decodebin links no
	// pads and the pipeline vacuously "plays" nothing (position 0 forever).
	requireH264Decoder(t)
	t.Setenv("CUTEPI_WALL_SINK", "fakesink") // headless: no display sink
	r := setupTestServer(t)
	stageVideoFixture(t)

	if err := ctp.AddCue("play-e2e.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	// Suite DB is shared across tests: remove the fixture afterwards so
	// later tests see their own sheet. Stop first — a playing clip cannot
	// be deleted (defers run LIFO, so register the delete first).
	defer del(t, r, "/api/media/play-e2e.mp4")
	defer gsp.Stop()

	// Row click selects...
	if w := post(t, r, "/api/cue/1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1 = %d, want 200: %s", w.Code, w.Body.String())
	}
	// ...Space / GO fires the selection.
	if w := post(t, r, "/api/cue/selected/play"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/selected/play = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Frames must flow: the position clock advances. A stalled wall sink
	// (or an unlinkable chain) parks at 0 with no error and a 200 status,
	// so this is the assertion that actually catches a silent no-play.
	posDeadline := time.Now().Add(20 * time.Second)
	for gsp.CurrentPosition() <= 0.05 {
		if time.Now().After(posDeadline) {
			t.Fatalf("position never advanced (stuck at %.3f) — frames are not flowing",
				gsp.CurrentPosition())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The end must arrive and tear the pipeline down. Without bus dispatch
	// the clip parks on its last frame and CurrentPlaying never clears.
	endDeadline := time.Now().Add(30 * time.Second)
	for gsp.CurrentPlaying() != "" {
		if time.Now().After(endDeadline) {
			t.Fatalf("pipeline still current after EOS window — end hook never fired")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The Tests toggle: on when dark, off when showing, unknown names rejected.
// The on half needs a real videotestsrc; the 400 needs nothing.
func TestTestToggleAndPatterns(t *testing.T) {
	r := setupTestServer(t)
	if w := post(t, r, "/api/test/bogus-pattern"); w.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/test/bogus-pattern = %d, want 400", w.Code)
	}
	w := get(t, r, "/api/testpatterns")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/testpatterns = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`smpte`, `smpte100`, `snow`, `circular`, `solid-color`, `checkers-8`, `blink`, `bar`} {
		if !strings.Contains(body, `"`+want+`"`) {
			t.Errorf("testpatterns missing %q", want)
		}
	}
	// Curated set (§12.10): the rest of the videotestsrc enum stays out.
	for _, bad := range []string{`smpte-rp-219`, `"circle"`, `"solid"`, `"pinwheel"`, `"zone-plate"`, `"smpte75"`} {
		if strings.Contains(body, bad) {
			t.Errorf("testpatterns still carries invalid nick %s", bad)
		}
	}
}

// The toggle answers its state both ways (needs a real videotestsrc).
func TestTestToggleCycle(t *testing.T) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping toggle test")
	}
	if out, err := exec.Command("gst-inspect-1.0", "videotestsrc").Output(); err != nil ||
		!strings.Contains(string(out), "GstVideoTestSrcPattern") {
		t.Skip("videotestsrc unavailable; skipping toggle test")
	}
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	r := setupTestServer(t)
	defer gsp.Stop()
	w := post(t, r, "/api/test/toggle")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"showing":true`) {
		t.Fatalf("toggle on = %d %s, want showing:true", w.Code, w.Body.String())
	}
	w = post(t, r, "/api/test/toggle")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"showing":false`) {
		t.Fatalf("toggle off = %d %s, want showing:false", w.Code, w.Body.String())
	}
}

// ESC fade time round-trips through settings; garbage is rejected.
func TestEscFadeMsSetting(t *testing.T) {
	r := setupTestServer(t)
	if w := postForm(t, r, "/api/settings", "escFadeMs", "750"); w.Code != http.StatusOK {
		t.Fatalf("POST settings escFadeMs = %d: %s", w.Code, w.Body.String())
	}
	if got := get(t, r, "/api/settings"); !strings.Contains(got.Body.String(), `"escFadeMs":750`) {
		t.Fatalf("settings lacks escFadeMs 750: %s", got.Body.String())
	}
	if w := postForm(t, r, "/api/settings", "escFadeMs", "99999"); w.Code != http.StatusBadRequest {
		t.Fatalf("POST settings escFadeMs=99999 = %d, want 400", w.Code)
	}
}

// Appearance is the only theme control: exactly one theme select, inside
// the Appearance pane.
func TestThemeControlLivesInAppearanceTab(t *testing.T) {
	raw, err := os.ReadFile("../templates/settingsModal.html")
	if err != nil {
		t.Fatalf("read settingsModal: %v", err)
	}
	html := string(raw)
	if n := strings.Count(html, `id="settingsTheme"`); n != 1 {
		t.Fatalf("found %d settingsTheme controls, want exactly 1", n)
	}
	pane := strings.Index(html, `id="settingsPaneAppearance"`)
	sel := strings.Index(html, `id="settingsTheme"`)
	if pane < 0 || sel < pane {
		t.Fatalf("settingsTheme is not inside the Appearance pane")
	}
}

func requireH264Decoder(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("gst-inspect-1.0").Output(); err != nil ||
		!(strings.Contains(string(out), "avdec_h264") || strings.Contains(string(out), "openh264dec") ||
			strings.Contains(string(out), "v4l2h264dec")) {
		t.Skip("no H.264 decoder plugin; cannot verify decoded playback")
	}
}

// stageVideoFixture builds a 10 s H264 file into the test media pool and
// registers it. Shared by the playback tests so each exercises the same
// real-decoder path.
func stageVideoFixture(t *testing.T) {
	t.Helper()
	fix := filepath.Join(t.TempDir(), "play-e2e.mp4")
	if err := exec.Command("ffmpeg", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=10:duration=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", fix).Run(); err != nil {
		t.Skipf("ffmpeg fixture failed: %v", err)
	}
	raw, err := os.ReadFile(fix)
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "play-e2e.mp4"), raw, 0o644); err != nil {
		t.Fatalf("WriteFile media: %v", err)
	}
	if err := ctp.RegisterMedia("play-e2e.mp4", int64(len(raw)), media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "640x360", Codec: "h264",
	}, "play-e2e.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
}

// A blank display duration holds the still indefinitely (the inspector
// promises "blank = indefinitely"). Before the hold, the still
// EOS-tore-down on its first frame — flash, then nothing.
func TestImageBlankDurationHolds(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build an image fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping live image test")
	}
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	r := setupTestServer(t)

	fix := filepath.Join(t.TempDir(), "hold-e2e.png")
	if err := exec.Command("ffmpeg", "-v", "error",
		"-f", "lavfi", "-i", "color=c=0x1a3a5c:size=640x360:rate=10",
		"-frames:v", "1", fix).Run(); err != nil {
		t.Skipf("ffmpeg fixture failed: %v", err)
	}
	raw, err := os.ReadFile(fix)
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "hold-e2e.png"), raw, 0o644); err != nil {
		t.Fatalf("WriteFile media: %v", err)
	}
	if err := ctp.RegisterMedia("hold-e2e.png", int64(len(raw)), media.Metadata{
		Mimetype: "image/png", Resolution: "640x360", Codec: "png",
	}, "hold-e2e.png"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("hold-e2e.png", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	defer del(t, r, "/api/media/hold-e2e.png")
	defer gsp.Stop()

	if w := post(t, r, "/api/cue/1/play"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1/play = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Past any instant-EOS window: the frame must still be up.
	time.Sleep(2500 * time.Millisecond)
	if got := gsp.CurrentPlaying(); got != "hold-e2e.png" {
		t.Fatalf("CurrentPlaying = %q after 2.5 s, want the held still", got)
	}
}

// Direct play (custom test patterns, tile "Play") of a still holds the frame
// until Stop, like a blank-duration cue.
func TestDirectPlayImageHolds(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build an image fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping live image test")
	}
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	r := setupTestServer(t)

	fix := filepath.Join(t.TempDir(), "direct-hold-e2e.png")
	if err := exec.Command("ffmpeg", "-v", "error",
		"-f", "lavfi", "-i", "color=c=0x1a3a5c:size=640x360:rate=10",
		"-frames:v", "1", fix).Run(); err != nil {
		t.Skipf("ffmpeg fixture failed: %v", err)
	}
	raw, err := os.ReadFile(fix)
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "direct-hold-e2e.png"), raw, 0o644); err != nil {
		t.Fatalf("WriteFile media: %v", err)
	}
	if err := ctp.RegisterMedia("direct-hold-e2e.png", int64(len(raw)), media.Metadata{
		Mimetype: "image/png", Resolution: "640x360", Codec: "png",
	}, "direct-hold-e2e.png"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	defer del(t, r, "/api/media/direct-hold-e2e.png")
	defer gsp.Stop()

	if w := post(t, r, "/api/play/direct-hold-e2e.png"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/play = %d: %s", w.Code, w.Body.String())
	}
	time.Sleep(2 * time.Second)
	if got := gsp.CurrentPlaying(); got != "direct-hold-e2e.png" {
		t.Fatalf("CurrentPlaying = %q after 2 s, want the held still", got)
	}
}

// The widget clock's dead-reckoning flag follows transport state.
func TestNowPlayingFlagFollowsPause(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build a playback fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping live playback test")
	}
	requireH264Decoder(t)
	t.Setenv("CUTEPI_WALL_SINK", "fakesink") // headless: no display sink
	r := setupTestServer(t)
	stageVideoFixture(t)
	if err := ctp.AddCue("play-e2e.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	defer del(t, r, "/api/media/play-e2e.mp4")
	defer gsp.Stop()

	if w := post(t, r, "/api/cue/1/play"); w.Code != http.StatusOK {
		t.Fatalf("play = %d: %s", w.Code, w.Body.String())
	}
	if body := get(t, r, "/api/nowplaying").Body.String(); !strings.Contains(body, `data-playing="1"`) {
		t.Fatalf("nowplaying while playing lacks data-playing=\"1\"")
	}
	if w := post(t, r, "/api/togglePause"); w.Code != http.StatusOK {
		t.Fatalf("togglePause = %d: %s", w.Code, w.Body.String())
	}
	if body := get(t, r, "/api/nowplaying").Body.String(); !strings.Contains(body, `data-playing="0"`) {
		t.Fatalf("nowplaying while paused lacks data-playing=\"0\"")
	}
}

// ESC fades out and stops: 200 at once, silence shortly after.
func TestEscFadeStopsPlayback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build a playback fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping live esc test")
	}
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	r := setupTestServer(t)

	if w := post(t, r, "/api/esc"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/esc idle = %d, want 200", w.Code)
	}
	stageVideoFixture(t)
	if err := ctp.AddCue("play-e2e.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	defer del(t, r, "/api/media/play-e2e.mp4")
	defer gsp.Stop()
	if w := post(t, r, "/api/cue/1/play"); w.Code != http.StatusOK {
		t.Fatalf("play = %d: %s", w.Code, w.Body.String())
	}
	if w := post(t, r, "/api/esc"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/esc playing = %d, want 200", w.Code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for gsp.CurrentPlaying() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("pipeline still current 10 s after ESC")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
