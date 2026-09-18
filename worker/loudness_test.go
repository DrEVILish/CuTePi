package worker

import (
	"math"
	"os/exec"
	"path/filepath"
	"testing"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

func TestImportMeasuresAndStoresLoudness(t *testing.T) {
	dir := t.TempDir()
	config.SetDbLocation(":memory:")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "loudness.wav")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=1000:duration=3", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	meta, err := media.Probe(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ctp.RegisterMedia("loudness.wav", 100, meta, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := ctp.PendingThumbnails()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %v", pending, err)
	}
	processOne(pending[0])
	gain, err := ctp.MediaLoudnessGain("loudness.wav")
	// ffmpeg's default sine is about -21.1 LUFS: correction should be -1.9 dB.
	if err != nil || math.Abs(gain-(-1.9)) > 0.3 {
		t.Fatalf("gain=%v err=%v", gain, err)
	}
	pending, err = ctp.PendingThumbnails()
	if err != nil || len(pending) != 0 {
		t.Fatalf("work not complete: %v %v", pending, err)
	}
	if gain, err := media.MeasureLoudness(filepath.Join(dir, "missing.wav")); err == nil || gain != 0 {
		t.Fatalf("failure gain=%v err=%v", gain, err)
	}
	cmd = exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "anullsrc", "-t", "1", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("silence: %v: %s", err, out)
	}
	if gain, err := media.MeasureLoudness(path); err == nil || gain != 0 {
		t.Fatalf("silence gain=%v err=%v", gain, err)
	}
}
