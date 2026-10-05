package routes

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
)

// originRouter is a minimal engine behind SameOrigin, answering 200.
func originRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SameOrigin())
	r.Any("/api/shutdown", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func originReq(r *gin.Engine, method, host, origin, referer string) int {
	req := httptest.NewRequest(method, "/api/shutdown", nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// C1: a hostile page's cross-site POST is refused; the UI's own same-origin
// requests and non-browser clients (no Origin/Referer) pass.
func TestSameOriginCSRF(t *testing.T) {
	r := originRouter()
	host := "192.168.1.20"
	cases := []struct {
		name, method, origin, referer string
		want                          int
	}{
		{"cross-site form post", "POST", "http://evil.example", "", http.StatusForbidden},
		{"cross-site via referer", "POST", "", "http://evil.example/page", http.StatusForbidden},
		{"opaque origin", "POST", "null", "", http.StatusForbidden},
		{"other port is another origin", "POST", "http://192.168.1.20:8080", "", http.StatusForbidden},
		{"same origin", "POST", "http://192.168.1.20", "", http.StatusOK},
		{"same origin via referer", "DELETE", "", "http://192.168.1.20/", http.StatusOK},
		{"no browser headers (curl, Companion)", "POST", "", "", http.StatusOK},
		{"cross-site GET is not state-changing", "GET", "http://evil.example", "", http.StatusOK},
	}
	for _, tc := range cases {
		if got := originReq(r, tc.method, host, tc.origin, tc.referer); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Any Host name is served (no allow-list), so any reverse proxy in front
// of the app works without configuration.
func TestSameOriginServesAnyHost(t *testing.T) {
	r := originRouter()
	hostname, _ := os.Hostname()
	for _, h := range []string{"192.168.1.20", "192.168.1.20:80", "[::1]:3001", "localhost:3001",
		strings.ToLower(hostname) + ".local", "cutepi.lan:8080",
		"cutepi-test.drevilish.com", "cutepi-dev.drevilish.com"} {
		if got := originReq(r, "GET", h, "", ""); got != http.StatusOK {
			t.Errorf("host %q: got %d, want 200", h, got)
		}
	}
}

// C2: saving an unrelated setting must not re-run the hotspot (which bounces
// the AP and drops every Wi-Fi client); changing the AP settings does.
func TestSettingsSaveOnlyAppliesChangedHotspot(t *testing.T) {
	r := setupTestServer(t)
	calls := 0
	old := applyAP
	applyAP = func() error { calls++; return nil }
	defer func() { applyAP = old }()

	if err := config.SetAP("ShowNet", "password1", true); err != nil {
		t.Fatalf("SetAP: %v", err)
	}
	// Network tab re-posts the same AP values alongside an unrelated change.
	w := postForm(t, r, "/api/settings", "escFadeMs", "500", "apSSID", "ShowNet", "apEnabled", "true")
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("unchanged hotspot re-applied %d time(s)", calls)
	}
	w = postForm(t, r, "/api/settings", "apSSID", "ShowNet2", "apEnabled", "true")
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("changed SSID applied %d time(s), want exactly 1", calls)
	}
}

// A validation failure late in the form must not leave earlier fields saved.
func TestSettingsSaveIsAllOrNothing(t *testing.T) {
	r := setupTestServer(t)
	before := config.Port()
	w := postForm(t, r, "/api/settings", "port", "4555", "audioRate", "999999")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid audio rate: got %d, want 400", w.Code)
	}
	if config.Port() != before {
		t.Fatalf("port saved (%d) despite the rejected form", config.Port())
	}
}

// C3: stdout and stderr pumps write concurrently; every complete line must
// reach onLine exactly once, never interleaved with another stream's line.
func TestLineWriterConcurrentStreams(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	inCallback := 0
	sink := &lineSink{max: 1 << 10, onLine: func(l string) {
		// onLine must never run concurrently (it writes the HTTP response).
		mu.Lock()
		inCallback++
		n := inCallback
		mu.Unlock()
		if n > 1 {
			t.Error("onLine invoked concurrently")
		}
		mu.Lock()
		got[l]++
		inCallback--
		mu.Unlock()
	}}
	out, errw := &lineWriter{sink: sink}, &lineWriter{sink: sink}
	var wg sync.WaitGroup
	for _, w := range []*lineWriter{out, errw} {
		wg.Add(1)
		go func(w *lineWriter, tag string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				w.Write([]byte(tag + "-"))
				w.Write([]byte("line\n"))
			}
		}(w, map[*lineWriter]string{out: "out", errw: "err"}[w])
	}
	wg.Wait()
	if got["out-line"] != 200 || got["err-line"] != 200 || len(got) != 2 {
		t.Fatalf("lines spliced or lost: %v", got)
	}
	if !strings.HasPrefix(sink.Tail(), "…") {
		t.Fatalf("tail not bounded: %d bytes", len(sink.Tail()))
	}
}

// W12: an endless HyperDeck line (no newline) drops the client instead of
// growing the buffer forever.
func TestHyperdeckLongLineDropsClient(t *testing.T) {
	setupTestDB(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go serveHyperdeck(ln)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	rd := bufio.NewReader(conn)
	for { // skip the 500 greeting block
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	junk := strings.Repeat("x", deckMaxLine*2)
	_, _ = conn.Write([]byte(junk))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := rd.ReadString('\n'); err == nil {
		t.Fatal("server kept the connection open after an over-long line")
	}
}

// W12: SLIP frames past slipMaxPacket are discarded whole; the stream
// recovers at the next END.
func TestSlipDecoderBounded(t *testing.T) {
	d := &slipDecoder{}
	big := make([]byte, slipMaxPacket+10)
	if out := d.feed(big); len(out) != 0 {
		t.Fatalf("oversized partial packet emitted")
	}
	if len(d.buf) > slipMaxPacket {
		t.Fatalf("buffer grew past cap: %d", len(d.buf))
	}
	out := d.feed([]byte{slipEND, 'o', 'k', slipEND})
	if len(out) != 1 || string(out[0]) != "ok" {
		t.Fatalf("decoder did not recover after overflow: %q", out)
	}
}

// W10: OSC 'i' arguments are signed int32.
func TestOSCIntIsSigned(t *testing.T) {
	msg := append([]byte("/x"), 0, 0)
	msg = append(msg, ',', 'i', 0, 0)
	msg = binary.BigEndian.AppendUint32(msg, uint32(0xFFFFFFFF)) // -1
	_, args, err := oscDecode(msg)
	if err != nil || len(args) != 1 || args[0] != -1 {
		t.Fatalf("got %v %v, want [-1]", args, err)
	}
}

// W13: only CSS hex colours are accepted anywhere they are stored.
func TestColourValidation(t *testing.T) {
	for _, c := range []string{"", "#fff", "#FFFA", "#a855f7", "#a855f7cc"} {
		if !ctp.ValidColor(c) {
			t.Errorf("%q rejected", c)
		}
	}
	for _, c := range []string{"#000;background:url(//evil)", "red", "#12", "#gggggg", "#fff}", "#fff\"x"} {
		if ctp.ValidColor(c) {
			t.Errorf("%q accepted", c)
		}
	}
	r := setupTestServer(t)
	if w := post(t, r, "/api/group/add"); w.Code != http.StatusOK {
		t.Fatalf("group add: %d", w.Code)
	}
	w := postForm(t, r, "/api/group/1/color", "color", "#000;background:url(//evil)")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("injected colour: got %d, want 400", w.Code)
	}
}

// buildShowZip writes a .CTP with a v2 manifest and the given media entries.
func buildShowZip(t *testing.T, entries ...string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "show.ctp")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	m, _ := zw.Create("cutepi.json")
	json.NewEncoder(m).Encode(showManifest{Version: 2})
	for _, name := range entries {
		w, _ := zw.Create(name)
		w.Write(buildTinyWav(1))
	}
	zw.Close()
	f.Close()
	return zipPath
}

// Older exports repeat a media entry once per cue that uses it; import must
// accept that (extracting the file once).
func TestParseShowZipToleratesDuplicateEntries(t *testing.T) {
	setupTestServer(t)
	_, files, err := parseShowZip(buildShowZip(t, "media/a.wav", "media/a.wav"))
	if err != nil {
		t.Fatalf("duplicate entry rejected: %v", err)
	}
	defer removeMediaTemps(files)
	if len(files) != 1 {
		t.Fatalf("extracted %d files, want 1", len(files))
	}
}

// W8: a .CTP that fails part-way must not leave extracted temp files.
func TestParseShowZipCleansUpOnError(t *testing.T) {
	setupTestServer(t)
	// The good entry is extracted first; the unsafe one then fails the parse.
	if _, _, err := parseShowZip(buildShowZip(t, "media/a.wav", "media/../evil.wav")); err == nil {
		t.Fatal("unsafe media entry accepted")
	}
	entries, _ := os.ReadDir(config.TmpDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "import-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// W9: WIFI: QR fields escape the format's special characters.
func TestWifiQREscape(t *testing.T) {
	if got := wifiQREscape(`a;b,c:d\e"f`); got != `a\;b\,c\:d\\e\"f` {
		t.Fatalf("got %q", got)
	}
}
