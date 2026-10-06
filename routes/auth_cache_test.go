package routes

import (
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// With the operator password on, nothing a shared cache (reverse proxy,
// CDN) could store and replay to an unauthenticated client is ever marked
// public: media is no-store, images private, the 401 itself no-store.
func TestAuthenticatedResponsesAreNeverSharedCacheable(t *testing.T) {
	setupTestServer(t) // data dirs and DB
	// The production middleware stack and static routes (setupTestServer
	// has neither the cache policy nor /media).
	gin.SetMode(gin.TestMode)
	r := gin.New()
	UseMiddleware(r)
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(TemplateFuncs()).ParseGlob("../templates/*")))
	Public(r)
	Api(r.Group("/api"))
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "clip.wav"), []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.ThumbnailLocation(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ThumbnailLocation(), "clip.wav.jpg"), []byte{0xff, 0xd8}, 0o644); err != nil {
		t.Fatal(err)
	}
	config.SetAuthPassword("s3cret")
	t.Cleanup(func() { config.SetAuthPassword("") })

	for _, tc := range []struct{ path, want string }{
		{"/media/clip.wav", "no-store"},
		{"/thumbnails/clip.wav.jpg", "private, max-age=3600"},
		{"/api/cuesheet", "no-store"},
	} {
		w := getWithBasic(t, r, tc.path, "op", "s3cret")
		if w.Code != http.StatusOK {
			t.Fatalf("authenticated GET %s = %d", tc.path, w.Code)
		}
		cc := w.Header().Get("Cache-Control")
		if cc != tc.want || strings.Contains(cc, "public") {
			t.Errorf("authenticated GET %s: Cache-Control %q, want %q", tc.path, cc, tc.want)
		}
		anon := get(t, r, tc.path)
		if anon.Code != http.StatusUnauthorized || anon.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("anonymous GET %s = %d, Cache-Control %q; want 401 no-store", tc.path, anon.Code, anon.Header().Get("Cache-Control"))
		}
	}
}
