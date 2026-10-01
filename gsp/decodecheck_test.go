package gsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real (if tiny) WAV decodes; bytes that are no media at all are refused
// with a reason.
func TestCheckDecodable(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(wav, silentWAV(8000, 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDecodable(wav); err != nil {
		t.Fatalf("wav refused: %v", err)
	}
	junk := filepath.Join(dir, "junk.mp4")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not a video ", 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckDecodable(junk); err == nil {
		t.Fatal("junk bytes passed the decode check")
	}
}

// silentWAV builds a mono 16-bit PCM WAV of n samples at rate Hz.
func silentWAV(rate, n int) []byte {
	data := n * 2
	b := []byte("RIFF")
	le := func(v, size int) {
		for i := 0; i < size; i++ {
			b = append(b, byte(v>>(8*i)))
		}
	}
	le(36+data, 4)
	b = append(b, "WAVEfmt "...)
	le(16, 4)
	le(1, 2)
	le(1, 2)
	le(rate, 4)
	le(rate*2, 4)
	le(2, 2)
	le(16, 2)
	b = append(b, "data"...)
	le(data, 4)
	return append(b, make([]byte, data)...)
}
