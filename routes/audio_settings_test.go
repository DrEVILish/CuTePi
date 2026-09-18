package routes

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"CuTePi/ctp"
	"CuTePi/media"
)

func TestInspectorAudioSettingsRoundTrip(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("settings.wav", 100, media.Metadata{Mimetype: "audio/wav", Duration: 10}, "settings.wav"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("settings.wav", ""); err != nil {
		t.Fatal(err)
	}
	post(t, r, "/api/cue/1")
	values := url.Values{"volume": {"3"}, "rate": {"1.5"}, "balance": {"-0.5"}, "mute": {"true"}, "fadeIn": {"1.25"}}
	save := func() int {
		req := httptest.NewRequest("PUT", "/api/cue/inspector/1", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := save(); code != 200 {
		t.Fatalf("save: %d", code)
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatal(err)
	}
	if cue.Volume != 3 || cue.Rate != 1.5 || cue.Balance != -0.5 || !cue.Mute || cue.FadeIn != 1250 {
		t.Fatalf("settings: %+v", cue)
	}
	body := get(t, r, "/api/cue/inspector").Body.String()
	if strings.Count(body, `name="fadeIn"`) != 1 {
		t.Fatal("fadeIn must submit only once")
	}
	for _, tc := range []struct{ col, val string }{{"rate", "NaN"}, {"rate", "+Inf"}, {"balance", "NaN"}, {"mute", "perhaps"}, {"fadeIn", "-1"}, {"fadeIn", "NaN"}, {"fadeIn", "1:Inf"}, {"volume", "NaN"}} {
		old := values.Get(tc.col)
		values.Set(tc.col, tc.val)
		if code := save(); code != 400 {
			t.Fatalf("%s=%s: %d", tc.col, tc.val, code)
		}
		values.Set(tc.col, old)
	}
	values.Set("rate", "20")
	values.Set("balance", "-20")
	values.Del("mute")
	if save() != 200 {
		t.Fatal("clamped save failed")
	}
	cue, _ = ctp.GetCue("1")
	if cue.Rate != 4 || cue.Balance != -1 || cue.Mute {
		t.Fatalf("clamp/unmute: %+v", cue)
	}
	values.Set("rate", "0.01")
	values.Set("balance", "20")
	if save() != 200 {
		t.Fatal("clamped save failed")
	}
	cue, _ = ctp.GetCue("1")
	if cue.Rate != 0.25 || cue.Balance != 1 {
		t.Fatalf("clamps: %+v", cue)
	}
}
