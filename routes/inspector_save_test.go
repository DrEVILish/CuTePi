package routes

import (
	"testing"

	"CuTePi/ctp"
	"CuTePi/media"
)

// Regression: the inspector PUT must persist fields to the DB (a silent
// no-write here would make every inspector edit cosmetic only).
func TestInspectorSavePersists(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("qa.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, "qa.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("qa.mp4", ""); err != nil {
		t.Fatal(err)
	}
	w := putForm(t, r, "/api/cue/inspector/1", "volume", "-38", "posStart", "0", "posEnd", "0")
	if w.Code != 200 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatal(err)
	}
	if cue.Volume != -38 {
		t.Fatalf("volume = %v, want -38", cue.Volume)
	}
}
