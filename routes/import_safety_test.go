package routes

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// An upload named ".." must be refused before importMedia ever treats the
// media dir's parent as the target (it used to rename that directory aside).
func TestUploadRejectsDotDotName(t *testing.T) {
	r := setupTestServer(t)
	parent := filepath.Dir(config.MediaLocation())

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("media", "..")
	fw.Write(buildTinyWav(1))
	mw.WriteField("onConflict", "replace")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload/", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("upload of %q = %d, want 422", "..", w.Code)
	}
	if moved, _ := filepath.Glob(parent + ".cutepi-replaced-*"); len(moved) != 0 {
		t.Fatalf("parent directory was set aside: %v", moved)
	}
	if ok, _ := ctp.MediaRegistered(".."); ok {
		t.Fatal(`".." was registered in the pool`)
	}
}

// importMedia itself refuses unsafe names (YouTube and show import call it
// directly) and cleans up the staged file.
func TestImportMediaRejectsUnsafeName(t *testing.T) {
	setupTestServer(t)
	os.MkdirAll(config.TmpDir(), 0o755)
	tmp := filepath.Join(config.TmpDir(), "x.wav")
	if err := os.WriteFile(tmp, buildTinyWav(1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importMedia("..", tmp); err == nil {
		t.Fatal(`importMedia("..") succeeded`)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("staged file left behind: %v", err)
	}
}

// Overwrite import must keep the show's own groups: ClearCueSheet deletes
// every group, so it has to run before ImportGroups.
func TestShowImportOverwriteKeepsGroups(t *testing.T) {
	for _, mode := range []string{"append", "overwrite"} {
		t.Run(mode, func(t *testing.T) {
			r := setupTestServer(t)
			if err := ctp.RegisterMedia("a.wav", 100, media.Metadata{Mimetype: "audio/wav", Duration: 1, Codec: "pcm_s16le"}, "a.wav"); err != nil {
				t.Fatal(err)
			}
			m := showManifest{App: "CuTePi", Version: manifestVersion,
				Cues:   []ctp.ExportCue{{Title: "a", Filename: "a.wav", Parent: 7}},
				Groups: []ctp.ExportGroup{{GroupID: 7, Name: "G"}},
			}
			if w := postCTP(t, r, buildCTPBytes(t, m, nil), mode); w.Code != http.StatusSeeOther {
				t.Fatalf("import = %d: %s", w.Code, w.Body.String())
			}
			cues, _, err := ctp.ExportCues()
			if err != nil || len(cues) != 1 {
				t.Fatalf("cues after import = %d (%v), want 1", len(cues), err)
			}
			groups, _ := ctp.ExportGroups()
			if len(groups) != 1 || groups[0].Name != "G" {
				t.Fatalf("groups after import = %+v, want the show's group G", groups)
			}
			if cues[0].Parent != groups[0].GroupID {
				t.Fatalf("cue parent = %d, want group %d", cues[0].Parent, groups[0].GroupID)
			}
		})
	}
}

// Exported media is stored, not deflated (already-compressed video).
func TestShowExportStoresMedia(t *testing.T) {
	setupTestServer(t)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "s.wav"), buildTinyWav(1), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeShowZip(&buf, showManifest{App: "CuTePi", Version: manifestVersion,
		Cues: []ctp.ExportCue{{Title: "s", Filename: "s.wav"}}}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "media/s.wav" {
			if f.Method != zip.Store {
				t.Fatalf("media entry method = %d, want Store", f.Method)
			}
			return
		}
	}
	t.Fatal("export carries no media/s.wav")
}

// Static dirs never list their contents, and pool files are never cached.
func TestStaticNoListingAndMediaNotCached(t *testing.T) {
	setupTestServer(t)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "pic.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(CachePolicy())
	Public(r)
	if w := get(t, r, "/media/"); w.Code != http.StatusNotFound {
		t.Fatalf("GET /media/ = %d, want 404 (no listing)", w.Code)
	}
	w := get(t, r, "/media/pic.png")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /media/pic.png = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET /media/pic.png Cache-Control = %q, want no-store", cc)
	}
}

func TestInputValidationFixes(t *testing.T) {
	r := setupTestServer(t)
	if w := postForm(t, r, "/api/testpattern/nope.png", "on", "1"); w.Code != http.StatusNotFound {
		t.Fatalf("pinning unknown media = %d, want 404", w.Code)
	}
	if w := get(t, r, "/api/media/nope.wav/wave?from=0&to=1&bins=16"); w.Code != http.StatusNotFound {
		t.Fatalf("wave for unregistered media = %d, want 404", w.Code)
	}
}

// A part over the multipart memory limit is spooled to disk by the parser;
// the upload moves that spool into place instead of copying it again.
func TestUploadSpooledPartIsMovedNotCopied(t *testing.T) {
	r := setupTestServer(t)
	r.MaxMultipartMemory = 1 << 10 // force the WAV part to disk
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("media", "big.wav")
	fw.Write(buildTinyWav(1))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload/", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("upload = %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(config.MediaLocation(), "big.wav")); err != nil {
		t.Fatalf("uploaded file not in the media dir: %v", err)
	}
	if left, _ := os.ReadDir(config.TmpDir()); len(left) != 0 {
		t.Fatalf("staging files left behind: %d", len(left))
	}
}
