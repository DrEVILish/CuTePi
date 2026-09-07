package media

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestKindFromExtension(t *testing.T) {
	cases := map[string]Kind{
		"clip.mp4":     KindVideo,
		"clip.MKV":     KindVideo,
		"song.mp3":     KindAudio,
		"song.WAV":     KindAudio,
		"photo.jpg":    KindImage,
		"photo.PNG":    KindImage,
		"document.pdf": KindUnknown,
		"noext":        KindUnknown,
	}
	for filename, want := range cases {
		if got := KindFromExtension(filename); got != want {
			t.Errorf("KindFromExtension(%q) = %q, want %q", filename, got, want)
		}
	}
}

func TestThumbnailSeek(t *testing.T) {
	cases := []struct {
		name     string
		duration float64
		want     float64
	}{
		{"long clip uses default 5s", 120, 5},
		{"exactly 10s uses default 5s", 10, 5},
		{"short clip uses midpoint", 4, 2},
		{"very short clip uses midpoint", 1, 0.5},
		{"zero duration falls back to default", 0, 5},
		{"unknown/negative duration falls back to default", -1, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := thumbnailSeek(c.duration); got != c.want {
				t.Errorf("thumbnailSeek(%v) = %v, want %v", c.duration, got, c.want)
			}
		})
	}
}

// Regression test: this is the bug found during live testing where any
// clip under 5s made ffmpeg seek past EOF and fail every single retry.
func TestThumbnailSeekNeverExceedsShortDuration(t *testing.T) {
	for _, d := range []float64{0.5, 1, 2, 3, 4.9} {
		if seek := thumbnailSeek(d); seek >= d {
			t.Errorf("thumbnailSeek(%v) = %v, must be < duration to avoid seeking past EOF", d, seek)
		}
	}
}

func TestIsImageFormat(t *testing.T) {
	cases := map[string]bool{
		"image2":                  true,
		"png_pipe":                true,
		"mov,mp4,m4a,3gp,3g2,mj2": false,
		"wav":                     false,
	}
	for formatName, want := range cases {
		if got := isImageFormat(formatName); got != want {
			t.Errorf("isImageFormat(%q) = %v, want %v", formatName, got, want)
		}
	}
}

// TestGeneratePeaks is the runnable check for the waveform-envelope logic in
// GeneratePeaks: fixed bucket count, values normalized to 0..1, a loud
// portion near 1.0 and a silent portion near 0.0. It builds a two-part WAV
// (loud then silent) in Go and decodes it with ffmpeg, so a broken analysis
// pipeline shows up as an all-zero or all-one envelope.
func TestGeneratePeaks(t *testing.T) {
	const sampleRate = 8000
	// 2.0s loud 440 Hz sine then 2.0s silence.
	seconds := 4
	data := make([]byte, 0, seconds*sampleRate*2)
	for s := 0; s < seconds*sampleRate; s++ {
		var v int16
		if s < 2*sampleRate {
			v = int16(6000 * sine(s, sampleRate, 440))
		}
		binary.LittleEndian.PutUint16(b[:], uint16(v))
		data = append(data, b[:]...)
	}
	wav := buildWav(data, sampleRate)
	path := filepath.Join(t.TempDir(), "peaks.wav")
	if err := os.WriteFile(path, wav, 0o644); err != nil {
		t.Fatalf("writing test wav: %v", err)
	}

	peaks, err := GeneratePeaks(path)
	if err != nil {
		t.Fatalf("GeneratePeaks: %v", err)
	}
	if len(peaks) != 300 {
		t.Fatalf("expected 300 peak buckets, got %d", len(peaks))
	}
	var max, min float64 = 0, 1
	for _, p := range peaks {
		if p < 0 || p > 1 {
			t.Fatalf("peak %v out of normalized range 0..1", p)
		}
		if p > max {
			max = p
		}
		if p < min {
			min = p
		}
	}
	if max < 0.95 {
		t.Fatalf("loud half should normalize near 1, max=%v", max)
	}
	if min > 0.05 {
		t.Fatalf("silent half should stay near 0, min=%v", min)
	}
}

var b [2]byte

// TestVerifyPlayable is the runnable check for the import-time playability
// probe: a real decodable audio file passes, a garbage/non-media file is
// rejected. Requires ffmpeg (same gate as the GStreamer runtime tests).
func TestVerifyPlayable(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; skipping playability probe test")
	}
	dir := t.TempDir()

	// A valid 1s WAV decodes cleanly.
	const sampleRate = 8000
	data := make([]byte, 0, sampleRate*2)
	for s := 0; s < sampleRate; s++ {
		binary.LittleEndian.PutUint16(b[:], uint16(int16(3000*sine(s, sampleRate, 440))))
		data = append(data, b[:]...)
	}
	wavPath := filepath.Join(dir, "play.wav")
	if err := os.WriteFile(wavPath, buildWav(data, sampleRate), 0o644); err != nil {
		t.Fatalf("writing valid wav: %v", err)
	}
	if err := VerifyPlayable(wavPath); err != nil {
		t.Fatalf("VerifyPlayable(valid wav) = %v, want nil", err)
	}

	// A corrupt "media" file must be rejected at import time.
	badPath := filepath.Join(dir, "corrupt.mp4")
	if err := os.WriteFile(badPath, []byte("definitely not a video file"), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}
	if err := VerifyPlayable(badPath); err == nil {
		t.Fatalf("VerifyPlayable(corrupt file) = nil, want an error")
	}
}

func sine(s int, rate int, freq float64) float64 {
	return math.Sin(2 * math.Pi * freq * float64(s%rate) / float64(rate))
}

func buildWav(data []byte, sampleRate int) []byte {
	const bitsPerSample = 16
	const channels = 1
	header := &bytes.Buffer{}
	header.WriteString("RIFF")
	binary.Write(header, binary.LittleEndian, uint32(36+len(data)))
	header.WriteString("WAVE")
	header.WriteString("fmt ")
	binary.Write(header, binary.LittleEndian, uint32(16))
	binary.Write(header, binary.LittleEndian, uint16(1))
	binary.Write(header, binary.LittleEndian, uint16(channels))
	binary.Write(header, binary.LittleEndian, uint32(sampleRate))
	binary.Write(header, binary.LittleEndian, uint32(sampleRate*channels*bitsPerSample/8))
	binary.Write(header, binary.LittleEndian, uint16(channels*bitsPerSample/8))
	binary.Write(header, binary.LittleEndian, uint16(bitsPerSample))
	header.WriteString("data")
	binary.Write(header, binary.LittleEndian, uint32(len(data)))
	return append(header.Bytes(), data...)
}

// The runnable check for image imports: Probe must accept an image (which
// has NO duration in ffprobe's format section) with Kind=image, resolution
// set and Duration 0 - and must still reject a video/audio file whose
// duration is missing. Image uploads used to fail with a ParseFloat("")
// error because duration was parsed before the kind was known.
func TestProbeImageHasNoDuration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; skipping image-probe test")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "t.jpg")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=red:s=64x48", "-frames:v", "1", img).CombinedOutput(); err != nil {
		t.Skipf("could not generate test image: %v: %s", err, out)
	}
	meta, err := Probe(img)
	if err != nil {
		t.Fatalf("Probe(image) = error %v, want success", err)
	}
	if meta.Kind != KindImage || meta.Duration != 0 || meta.Resolution != "64x48" {
		t.Fatalf("Probe(image) = %+v, want KindImage, Duration 0, 64x48", meta)
	}

	// A real clip must still carry its duration.
	wav := filepath.Join(dir, "t.wav")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", wav).CombinedOutput(); err != nil {
		t.Skipf("could not generate test wav: %v: %s", err, out)
	}
	meta, err = Probe(wav)
	if err != nil {
		t.Fatalf("Probe(wav) = error %v, want success", err)
	}
	if meta.Duration < 0.5 || meta.Duration > 2 {
		t.Fatalf("Probe(wav) duration = %v, want ~1s", meta.Duration)
	}
}
