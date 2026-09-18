package ctp

import (
	"path/filepath"
	"testing"

	"CuTePi/config"
)

func TestAudioSettingsSurviveRestartAndExport(t *testing.T) {
	oldDB, oldLocation := db, config.DbLocation()
	defer func() { db.Close(); db = oldDB; config.SetDbLocation(oldLocation) }()
	config.SetDbLocation(filepath.Join(t.TempDir(), "settings.db"))
	if err := InitDB(); err != nil {
		t.Fatal(err)
	}
	mustRegisterMedia(t, "settings.wav")
	if err := AddCue("settings.wav", ""); err != nil {
		t.Fatal(err)
	}
	cue, err := GetCue("1")
	if err != nil || cue.Rate != 1 || cue.FadeIn != 0 || cue.Balance != 0 || cue.Mute {
		t.Fatalf("defaults: %+v %v", cue, err)
	}
	if err := UpdateCueFields("1", map[string]string{"rate": "1.5", "fadeIn": "2.5", "balance": "-0.5", "mute": "true", "volume": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := StoreLoudnessGain(cue.Media_id, -3.2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := InitDB(); err != nil {
		t.Fatal(err)
	}
	cue, err = GetCue("1")
	if err != nil || cue.Rate != 1.5 || cue.FadeIn != 2500 || cue.Balance != -0.5 || !cue.Mute || cue.Volume != 1 || cue.LoudnessGain != -3.2 {
		t.Fatalf("restart: %+v %v", cue, err)
	}
	exported, _, err := ExportCues()
	if err != nil || len(exported) != 1 {
		t.Fatalf("export: %v %v", exported, err)
	}
	if _, err := AddCueFull(exported[0]); err != nil {
		t.Fatal(err)
	}
	restored, err := GetCue("2")
	if err != nil || restored.Rate != cue.Rate || restored.FadeIn != cue.FadeIn || restored.Balance != cue.Balance || restored.Mute != cue.Mute {
		t.Fatalf("import: %+v %v", restored, err)
	}
	if _, err := AddCueFull(ExportCue{Filename: "settings.wav", Title: "old show", CueNum: "3"}); err != nil {
		t.Fatal(err)
	}
	legacy, _ := GetCue("3")
	if legacy.Rate != 1 {
		t.Fatalf("legacy import rate=%v", legacy.Rate)
	}
}
