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
	if body := get(t, r, "/api/cue/inspector").Body.String(); strings.Contains(body, "data-wave") {
		t.Fatal("inspector links a waveform the clip does not have")
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
