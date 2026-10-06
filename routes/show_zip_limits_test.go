package routes

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipWith writes a .CTP-shaped archive with n empty media entries.
func zipWith(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("cutepi.json")
	w.Write([]byte(`{"version":1,"cues":[]}`))
	for i := 0; i < n; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("media/f%d.wav", i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A show with more entries than any real show is refused before its
// entries are extracted.
func TestShowImportRefusesTooManyEntries(t *testing.T) {
	r := setupTestServer(t)
	w := postCTP(t, r, zipWith(t, maxShowEntries+1), "overwrite")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "too many entries") {
		t.Fatalf("import = %d %q, want 422 too many entries", w.Code, w.Body.String())
	}
}

// The entry count is read from the end record, including the ZIP64 one an
// archive needs past 65535 entries.
func TestZipDeclaredEntries(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{3, 66000} {
		p := filepath.Join(dir, fmt.Sprint(n)+".zip")
		if err := os.WriteFile(p, zipWith(t, n), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := zipDeclaredEntries(p)
		if !ok || got != uint64(n+1) {
			t.Fatalf("%d media entries: declared %d (ok %v), want %d", n, got, ok, n+1)
		}
	}
}

// Declared ZIP64 sizes that wrap the running total are refused, not summed
// past the free-space check.
func TestShowImportRefusesOverflowingSizes(t *testing.T) {
	r := setupTestServer(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("cutepi.json")
	w.Write([]byte(`{"version":1,"cues":[]}`))
	for i, size := range []uint64{1<<64 - 1, 2} { // sum wraps to 1
		if _, err := zw.CreateRaw(&zip.FileHeader{Name: fmt.Sprintf("media/f%d.wav", i), Method: zip.Store,
			UncompressedSize64: size, CompressedSize64: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	resp := postCTP(t, r, buf.Bytes(), "overwrite")
	if resp.Code < 400 || !strings.Contains(resp.Body.String(), "overflow") {
		t.Fatalf("import = %d %q, want refused: declared sizes overflow", resp.Code, resp.Body.String())
	}
}
