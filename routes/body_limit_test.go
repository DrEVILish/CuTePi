package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Control endpoints get the small body cap, declared or streamed; the media
// routes keep the large one.
func TestSmallBodyCapOnControlRoutes(t *testing.T) {
	r := setupTestServer(t)
	big := `{"names":["` + strings.Repeat("a", 2<<20) + `"]}`

	req := httptest.NewRequest(http.MethodPost, "/upload/check", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared 2 MiB JSON = %d, want 413", w.Code)
	}

	// Chunked (no Content-Length): cut off at the cap while reading.
	req = httptest.NewRequest(http.MethodPost, "/upload/check", io.MultiReader(strings.NewReader(big)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatalf("streamed 2 MiB JSON = %d, want an error", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/upload/check", strings.NewReader(`{"names":["x.mp4"]}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("small JSON = %d: %s", w.Code, w.Body.String())
	}

	for path, want := range map[string]int64{"/upload/": maxBodyBytes, "/api/show/import": maxBodyBytes, "/api/cue/live": maxSmallBodyBytes, "/upload/check": maxSmallBodyBytes} {
		if got := bodyLimit(path); got != want {
			t.Errorf("bodyLimit(%s) = %d, want %d", path, got, want)
		}
	}
}
