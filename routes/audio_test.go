package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// `aplay -L` lists device IDs on non-indented lines, each followed by
// indented description lines. "null" is not a destination.
func TestParseAplayDevices(t *testing.T) {
	const sample = `null
    Discard all samples (playback) or generate zero samples (capture)
hw:CARD=vc4hdmi0,DEV=0
    MAI PCM i2s-hifi-0
    Direct hardware device without any conversions
plughw:CARD=vc4hdmi0,DEV=0
    MAI PCM i2s-hifi-0
    Hardware device with all software conversions
`
	devs := parseAplayDevices(sample)
	if len(devs) != 2 {
		t.Fatalf("parsed %d devices, want 2: %+v", len(devs), devs)
	}
	if devs[0].ID != "hw:CARD=vc4hdmi0,DEV=0" {
		t.Errorf("dev[0].ID = %q", devs[0].ID)
	}
	if devs[0].Label != "hw:CARD=vc4hdmi0,DEV=0 — MAI PCM i2s-hifi-0" {
		t.Errorf("dev[0].Label = %q", devs[0].Label)
	}
	if devs[1].ID != "plughw:CARD=vc4hdmi0,DEV=0" {
		t.Errorf("dev[1].ID = %q", devs[1].ID)
	}
}

// The endpoint never fails: without ALSA tooling it answers an empty list
// and the tab keeps its manual input.
func TestAudioDevicesEndpointShape(t *testing.T) {
	r := setupTestServer(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/audio/devices", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/audio/devices = %d, want 200", w.Code)
	}
	var body struct {
		Devices []audioDevice `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("devices response is not JSON: %v", err)
	}
	if body.Devices == nil {
		t.Errorf("devices must be an array (possibly empty), got null")
	}
}
