package routes

import (
	"strings"
	"testing"

	"CuTePi/ctp"
	"CuTePi/media"
)

// The inspector links a clip's stored waveform instead of embedding it (a
// long clip's peaks are hundreds of KB, re-sent on every inspector render);
// the link carries the store time, so the browser can keep it.
func TestInspectorLinksWaveform(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("wave clip.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, "wave clip.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("wave clip.mp4", ""); err != nil {
		t.Fatal(err)
	}
	post(t, r, "/api/cue/1")
	if body := get(t, r, "/api/cue/inspector").Body.String(); strings.Contains(body, "data-wave-url") || !strings.Contains(body, `data-wave-pending="1"`) {
		t.Fatal("inspector links a waveform the clip does not have, or does not say it is being analysed")
	}
	if w := get(t, r, "/api/media/wave%20clip.mp4/peaks"); w.Code != 404 {
		t.Fatalf("peaks without a waveform = %d, want 404", w.Code)
	}
	pool, _ := ctp.GetMediapool()
	if err := ctp.StoreWaveform(pool.Medias[0].Media_id, "[0.25,0.5]"); err != nil {
		t.Fatal(err)
	}
	cue, _ := ctp.GetCue("1")
	body := get(t, r, "/api/cue/inspector").Body.String()
	want := `data-wave-url="/api/media/wave%20clip.mp4/peaks?v=`
	if cue.WaveformVersion == 0 || !strings.Contains(body, want) || strings.Contains(body, "[0.25,0.5]") {
		t.Fatalf("inspector: version %d, want a link %s and no peaks inline", cue.WaveformVersion, want)
	}
	w := get(t, r, "/api/media/wave%20clip.mp4/peaks?v=1")
	if w.Code != 200 || w.Body.String() != "[0.25,0.5]" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("peaks = %d %q (%s)", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
}

// The trim timeline is the ftl-themes waveform (DESIGN §5.5): the seek
// layer is an inert playhead, the region's edges are Trim In/Out without a
// name (the form sends posStart/posEnd from the fields once), and the page
// loads the scripts that drive it.
func TestTrimTimelineMarkup(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("tl.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 30}, "tl.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("tl.mp4", ""); err != nil {
		t.Fatal(err)
	}
	post(t, r, "/api/cue/1")
	body := get(t, r, "/api/cue/inspector").Body.String()
	for _, want := range []string{`class="waveform"`, `class="trim-playhead"`, ` inert `, `class="waveform-region is-static"`,
		`id="trim-edge-in"`, `id="trim-edge-out"`, `data-cue-pos="1"`, `id="trim-mk-fadein"`, `id="trim-length"`} {
		if !strings.Contains(body, want) {
			t.Errorf("inspector timeline missing %s", want)
		}
	}
	if strings.Count(body, `name="posStart"`) != 1 || strings.Count(body, `name="posEnd"`) != 1 {
		t.Errorf("posStart/posEnd must be sent once (the fields), not by the region edges")
	}
	page := get(t, r, "/").Body.String()
	for _, want := range []string{`/ftl/assets/js/controls.js?v=`, `/src/trimline.js?v=`} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not load %s", want)
		}
	}
}
