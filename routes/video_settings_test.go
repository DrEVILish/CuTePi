package routes

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"CuTePi/ctp"
	"CuTePi/media"
)

// Geometry (fit/rotation/flip) round-trips through the inspector PUT and is
// rejected when out of range; the image Time tab exposes display timing.
func TestInspectorGeometryRoundTrip(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("geo.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, "geo.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.RegisterMedia("geo.png", 100, media.Metadata{Mimetype: "image/png"}, "geo.png"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("geo.mp4", ""); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("geo.png", ""); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"fit_mode": {"stretch"}, "rotation": {"90"}, "flip": {"h"}}
	save := func(pos string) int {
		req := httptest.NewRequest("PUT", "/api/cue/inspector/"+pos, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := save("1"); code != 200 {
		t.Fatalf("save: %d", code)
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatal(err)
	}
	if cue.FitMode != "stretch" || cue.Rotation != 90 || cue.Flip != "h" {
		t.Fatalf("geometry: %+v", cue)
	}
	for _, tc := range []struct{ col, val string }{{"fit_mode", "cover"}, {"rotation", "45"}, {"flip", "both"}} {
		values.Set(tc.col, tc.val)
		if code := save("1"); code != 400 {
			t.Fatalf("%s=%s: %d, want 400", tc.col, tc.val, code)
		}
		values.Set(tc.col, map[string]string{"fit_mode": "stretch", "rotation": "90", "flip": "h"}[tc.col])
	}
	// Image cue: Time tab offers display duration, auto-continue and fades.
	post(t, r, "/api/cue/2")
	body := get(t, r, "/api/cue/inspector").Body.String()
	for _, want := range []string{`name="cueDuration"`, `name="autoContinue"`, `name="fadeAction"`, `name="preWait"`, `name="postWait"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("image inspector missing %q", want)
		}
	}
	if strings.Contains(body, "no playback timing or trim") {
		t.Fatal("image inspector still shows the static-image dead end")
	}
}
