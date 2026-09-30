package routes

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// addSelectedCue registers one media item, adds it as cue 1 and selects it.
func addSelectedCue(t *testing.T, name, mime string, dur float64) {
	t.Helper()
	if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: mime, Duration: dur}, name); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue(name, ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := ctp.SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
}

// paneBalanced reports whether the markup from one tab pane's opening tag up
// to the next pane's opening tag closes every <div> it opens — i.e. the
// panes are siblings, not nested inside each other.
func paneBalanced(t *testing.T, body, from, to string) bool {
	t.Helper()
	i := strings.Index(body, `data-tabpane="`+from+`"`)
	j := strings.Index(body, `data-tabpane="`+to+`"`)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("panes %q..%q not found in order", from, to)
	}
	// Step back to the pane's own "<div" and stop before the next pane's.
	seg := body[strings.LastIndex(body[:i], "<div"):strings.LastIndex(body[:j], "<div")]
	return strings.Count(seg, "<div") == strings.Count(seg, "</div>")
}

// §5.5: the Time tab renders its fields even where the duration is unknown;
// only the waveform timeline is duration-gated.
func TestInspectorTimeTabWithoutDuration(t *testing.T) {
	r := setupTestServer(t)
	addSelectedCue(t, "nodur.mp4", "video/mp4", 0)
	body := get(t, r, "/api/cue/inspector").Body.String()
	for _, want := range []string{`id="insp-in"`, `id="insp-out"`, `id="insp-prewait"`, `id="insp-postwait"`, `id="insp-loop"`, `id="insp-hold"`, `id="insp-autocontinue"`, `id="insp-rate"`, `id="insp-fadecurve"`} {
		if !strings.Contains(body, want) {
			t.Errorf("time tab without duration missing %s", want)
		}
	}
	if strings.Contains(body, `id="trim-timeline"`) {
		t.Error("timeline rendered without a duration")
	}
	if !paneBalanced(t, body, "time", "audio") {
		t.Error("time pane not closed before the audio pane")
	}
}

// Every inspector tab pane is a sibling: an image cue's Time pane used to
// stay open and swallow the Video/Media/Colour panes.
func TestInspectorPanesAreSiblings(t *testing.T) {
	r := setupTestServer(t)
	addSelectedCue(t, "still.png", "image/png", 0)
	body := get(t, r, "/api/cue/inspector").Body.String()
	if strings.Contains(body, `data-tabpane="audio"`) {
		t.Error("image cue renders an audio pane")
	}
	if !paneBalanced(t, body, "time", "video") {
		t.Error("image cue: time pane not closed before the video pane")
	}
}

func TestInspectorVideoPanesAreSiblings(t *testing.T) {
	r := setupTestServer(t)
	addSelectedCue(t, "clip.mp4", "video/mp4", 12)
	body := get(t, r, "/api/cue/inspector").Body.String()
	if !strings.Contains(body, `id="trim-timeline"`) {
		t.Fatal("timeline missing for a cue with a duration")
	}
	for _, p := range [][2]string{{"time", "audio"}, {"audio", "video"}, {"video", "media"}, {"media", "advanced"}} {
		if !paneBalanced(t, body, p[0], p[1]) {
			t.Errorf("%s pane not closed before %s", p[0], p[1])
		}
	}
	// §5.5 extras: rate 1× reset, curve preview, output device picker.
	for _, want := range []string{`data-reset-slider="insp-rate"`, `data-curve-for="insp-fadecurve"`, `id="insp-audio-device"`} {
		if !strings.Contains(body, want) {
			t.Errorf("inspector missing %s", want)
		}
	}
}

// The inspector's output-device picker saves the system-wide device.
func TestInspectorAudioDeviceSaves(t *testing.T) {
	r := setupTestServer(t)
	prev := config.Audio()
	t.Cleanup(func() { _ = config.SetAudio(prev.Device, prev.Channels, prev.Rate) })
	if w := postForm(t, r, "/api/setting/audiodevice", "device", "hw:CARD=vc4hdmi1,DEV=0"); w.Code != http.StatusNoContent {
		t.Fatalf("POST = %d: %s", w.Code, w.Body.String())
	}
	if got := config.Audio(); got.Device != "hw:CARD=vc4hdmi1,DEV=0" || got.Channels != prev.Channels || got.Rate != prev.Rate {
		t.Errorf("audio = %+v, want the device changed and channels/rate kept", got)
	}
}

// §5.4: a scheduled cue's clock icon sits right after the media icon.
func TestScheduleClockBesideMediaIcon(t *testing.T) {
	r := setupTestServer(t)
	addSelectedCue(t, "timed.mp4", "video/mp4", 5)
	if err := ctp.SetCueSchedule(1, true, 1, 0); err != nil {
		t.Fatalf("SetCueSchedule: %v", err)
	}
	body := get(t, r, "/api/cuesheet").Body.String()
	td := body[strings.Index(body, `<td class="cue-type"`):]
	td = td[:strings.Index(td, "</td>")]
	if !strings.Contains(td, "icon-clock") {
		t.Errorf("clock icon not in the media-icon cell: %s", td)
	}
}

func TestWaitPct(t *testing.T) {
	w := waitState{PhaseStart: 1000, PhaseEnd: 3000}
	for _, c := range []struct {
		now  int64
		want int
	}{{500, 0}, {1000, 0}, {2000, 50}, {3000, 100}, {9000, 100}} {
		if got := waitPct(w, c.now); got != c.want {
			t.Errorf("waitPct(now=%d) = %d, want %d", c.now, got, c.want)
		}
	}
	if got := waitPct(waitState{PhaseStart: 5, PhaseEnd: 5}, 5); got != 100 {
		t.Errorf("zero-length phase = %d, want 100", got)
	}
}

// §5.7: uploads warn against the media disk's free space.
func TestDiskFreeEndpoint(t *testing.T) {
	r := setupTestServer(t)
	w := get(t, r, "/api/disk")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/disk = %d", w.Code)
	}
	var body struct {
		FreeBytes uint64 `json:"freeBytes"`
		Known     bool   `json:"known"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !body.Known || body.FreeBytes == 0 {
		t.Errorf("disk = %+v, want the temp media dir's free space", body)
	}
}

// §6.9: the ESC fade defaults to 1000 ms.
func TestEscFadeDefault(t *testing.T) {
	setupTestDB(t)
	if got := ctp.GetEscFadeMs(); got != 1000 {
		t.Errorf("default ESC fade = %d ms, want 1000", got)
	}
}
