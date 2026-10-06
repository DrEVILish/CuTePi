package routes

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"
)

// twoWavCues registers two audio cues (positions 1 and 2) and plays cue 1.
func twoWavCues(t *testing.T) {
	t.Helper()
	for _, name := range []string{"mixa.wav", "mixb.wav"} {
		if err := os.WriteFile(filepath.Join(config.MediaLocation(), name), buildTinyWav(5), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "audio/wav", Duration: 5}, name); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	cue, _ := ctp.GetCue("1")
	if err := loadAndPlayCue(cue); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gsp.Panic)
	for end := time.Now().Add(3 * time.Second); gsp.CurrentCuePos() != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("cue 1 never started")
		}
	}
}

// A live volume change is saved on the cue whose pipeline took it: the
// cue position comes back from the same locked step that applies it.
func TestLiveVolumePersistsOnTheCueItApplied(t *testing.T) {
	r := setupTestServer(t)
	twoWavCues(t)
	if w := postForm(t, r, "/api/volume", "volume", "-6"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/volume = %d", w.Code)
	}
	a, _ := ctp.GetCue("1")
	b, _ := ctp.GetCue("2")
	if a.Volume != -6 || b.Volume != 0 {
		t.Fatalf("volumes after a live change on cue 1: cue1 %v cue2 %v", a.Volume, b.Volume)
	}
	if applied, pos := gsp.SetCueVolume(-3); applied != -3 || pos != 1 {
		t.Fatalf("SetCueVolume = %v on cue %d, want -3 on cue 1", applied, pos)
	}
}

// The Cue Inspector's live re-apply only lands on the cue it edited: once a
// different cue plays, the edited cue's mix leaves it alone.
func TestInspectorMixOnlyAppliesToItsCue(t *testing.T) {
	setupTestServer(t)
	twoWavCues(t)
	gsp.SetCueVolume(0)
	if gsp.ApplyCueMix(2, gsp.CueMix{Volume: -20, Rate: 1}) {
		t.Fatal("cue 2's mix applied while cue 1 plays")
	}
	if v := gsp.Volume(); v != 0 {
		t.Fatalf("playing cue's volume = %v after another cue's mix, want 0", v)
	}
	if !gsp.ApplyCueMix(1, gsp.CueMix{Volume: -12, Rate: 1}) || gsp.Volume() != -12 {
		t.Fatalf("cue 1's own mix not applied (volume %v)", gsp.Volume())
	}
}
