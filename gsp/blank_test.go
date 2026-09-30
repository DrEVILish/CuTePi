package gsp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBlankFramebufferZeroFillsStrideTimesHeight(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sys, "stride"), []byte("300\n"), 0o644)
	os.WriteFile(filepath.Join(sys, "virtual_size"), []byte("150,100\n"), 0o644)
	dev := filepath.Join(dir, "fb")
	os.WriteFile(dev, bytes.Repeat([]byte{0xAB}, 300*100+50), 0o644)

	if err := blankFramebuffer(dev, sys); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dev)
	if len(got) != 300*100+50 {
		t.Fatalf("device length changed: %d", len(got))
	}
	if !bytes.Equal(got[:300*100], make([]byte, 300*100)) {
		t.Fatal("frame area not zeroed")
	}
	if got[300*100] != 0xAB {
		t.Fatal("wrote past stride*height")
	}
}

func TestBlankFramebufferMissingSysfsIsAnError(t *testing.T) {
	if err := blankFramebuffer(filepath.Join(t.TempDir(), "fb"), t.TempDir()); err == nil {
		t.Fatal("expected an error without stride/virtual_size")
	}
}
