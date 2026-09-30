package routes

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"CuTePi/config"
)

// O4: an htmx request that fails gets the bare message as text/plain with
// the handler's status; a normal navigation still gets the full error page.
func TestRespondErrorPlainTextForHTMX(t *testing.T) {
	r := setupTestServer(t)

	send := func(htmx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/youtube", strings.NewReader("url="))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := send(true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("htmx POST /youtube without URL = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("htmx error Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if body := w.Body.String(); body != "no URL provided" {
		t.Fatalf("htmx error body = %q, want the bare message", body)
	}

	w = send(false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("plain POST /youtube without URL = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("navigation error Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") || !strings.Contains(body, "<pre") || !strings.Contains(body, "no URL provided") {
		t.Fatalf("navigation error should be the full error page, got:\n%s", body)
	}
}

// O4 on a second handler family (the API): a failing htmx cue action is
// plain text, not a document.
func TestAPIErrorPlainTextForHTMX(t *testing.T) {
	r := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/volume", strings.NewReader("volume=loud"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/volume volume=loud = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8 (body %q)", ct, w.Body.String())
	}
	if body := w.Body.String(); body != "volume must be a number" {
		t.Fatalf("body = %q, want the bare message", body)
	}
}

// O5: a file that fails ffprobe is reported by the user's file name only,
// never by the server's temp/media path or the staging file name.
func TestUploadProbeFailureHidesServerPaths(t *testing.T) {
	r := setupTestServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("media", "broken clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("this is not a video file at all"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("upload of a non-media file = %d, want 422 (body %q)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "broken clip.mp4") {
		t.Fatalf("error should name the uploaded file, got %q", body)
	}
	for _, leak := range []string{config.TmpDir(), config.MediaLocation(), config.WorkingDir(), "upload-"} {
		if strings.Contains(body, leak) {
			t.Fatalf("error leaks %q: %q", leak, body)
		}
	}
	// Nothing staged or imported is left behind.
	if entries, _ := os.ReadDir(config.TmpDir()); len(entries) != 0 {
		t.Fatalf("temp dir not cleaned up: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(config.MediaLocation(), "broken clip.mp4")); err == nil {
		t.Fatal("a failed import landed in the media pool")
	}
}

func TestRedactPathsAndImportError(t *testing.T) {
	setupTestServer(t)
	tmp := config.TmpDir()
	mediaDir := config.MediaLocation()

	msg := redactPaths(`media: ffprobe failed for "` + filepath.Join(tmp, "upload-1.mp4") + `" and "` + filepath.Join(mediaDir, "clip.mp4") + `" in ` + tmp)
	for _, leak := range []string{tmp, mediaDir} {
		if strings.Contains(msg, leak) {
			t.Fatalf("redactPaths left %q in %q", leak, msg)
		}
	}
	if !strings.HasPrefix(msg, "media: ffprobe failed for \"upload-1.mp4\"") || !strings.Contains(msg, `"clip.mp4"`) {
		t.Fatalf("redactPaths mangled the message: %q", msg)
	}

	src := filepath.Join(tmp, "upload-42.mov")
	base := errors.New("boom")
	err := importError(errors.Join(errors.New(`media: playability probe failed for "`+src+`": exit status 1: `+src+": Invalid data"), base), "My Clip.mov", src)
	if got := err.Error(); strings.Contains(got, tmp) || strings.Contains(got, "upload-42") || !strings.Contains(got, `"My Clip.mov"`) {
		t.Fatalf("importError = %q, want the user's file name and no server path", got)
	}
	if !errors.Is(err, base) {
		t.Fatal("importError must keep the wrapped error for errors.Is")
	}
	if importError(nil, "x", "y") != nil {
		t.Fatal("importError(nil) must be nil")
	}
}
