package routes

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// resetRemote switches every listener off again so later tests (and the
// real config file of the test dir) start from the §12.8 default.
func resetRemote(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = config.SetRemote(config.RemoteSettings{})
		ApplyRemote()
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// §12.8: every remote listener is off by default, and the Network tab's
// status list says so.
func TestRemoteListenersOffByDefault(t *testing.T) {
	setupTestDB(t)
	resetRemote(t)
	_ = config.SetRemote(config.RemoteSettings{})
	ApplyRemote()
	for _, e := range RemoteStatus() {
		if e.Enabled || e.Running {
			t.Errorf("%s enabled=%v running=%v, want both off by default", e.ID, e.Enabled, e.Running)
		}
	}
	r := config.Remote()
	if r.HyperDeckPort != 9993 || r.OSCUDPPort != 53000 || r.OSCTCPPort != 53000 || r.HyperDeckClips != "cuesheet" {
		t.Errorf("defaults = %+v", r)
	}
}

// Saving the Network tab starts exactly the enabled listeners on their
// configured ports, reports them live, and switching one off stops it.
func TestRemoteSettingsStartStop(t *testing.T) {
	r := setupTestServer(t)
	resetRemote(t)
	port := freePort(t)
	w := postForm(t, r, "/api/settings",
		"remoteBlock", "true",
		"remoteHyperdeck", "true",
		"remoteHyperdeckPort", strconv.Itoa(port),
		"remoteOscUdpPort", "53000",
		"remoteOscTcpPort", "53000",
	)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings = %d: %s", w.Code, w.Body.String())
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatalf("HyperDeck listener not reachable after enabling: %v", err)
	}
	conn.Close()

	var body struct {
		Remote       config.RemoteSettings `json:"remote"`
		RemoteStatus []RemoteStatusEntry   `json:"remoteStatus"`
	}
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &body); err != nil {
		t.Fatalf("settings JSON: %v", err)
	}
	if !body.Remote.HyperDeck || body.Remote.OSCUDP || body.Remote.OSCTCP {
		t.Errorf("remote = %+v, want only HyperDeck on", body.Remote)
	}
	for _, e := range body.RemoteStatus {
		want := e.ID == "hyperdeck"
		if e.Running != want {
			t.Errorf("%s running=%v, want %v", e.ID, e.Running, want)
		}
	}

	// Unchecked toggle (absent field) with the block marker = off.
	if w := postForm(t, r, "/api/settings", "remoteBlock", "true", "remoteHyperdeckPort", strconv.Itoa(port)); w.Code != http.StatusOK {
		t.Fatalf("disable POST = %d", w.Code)
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("HyperDeck listener still accepting after it was switched off")
	}
}

// A settings post without the Network block never touches the listeners.
func TestRemoteSettingsUntouchedWithoutBlock(t *testing.T) {
	r := setupTestServer(t)
	resetRemote(t)
	if err := config.SetRemote(config.RemoteSettings{OSCUDP: true, OSCUDPPort: freePort(t)}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if w := postForm(t, r, "/api/settings", "port", "3001"); w.Code != http.StatusOK {
		t.Fatalf("POST = %d", w.Code)
	}
	if !config.Remote().OSCUDP {
		t.Error("a post without remoteBlock switched OSC UDP off")
	}
}

func TestRemoteSettingsValidation(t *testing.T) {
	r := setupTestServer(t)
	resetRemote(t)
	if w := postForm(t, r, "/api/settings", "remoteBlock", "true", "remoteOscBind", "not-an-ip"); w.Code != http.StatusBadRequest {
		t.Errorf("bad bind address = %d, want 400", w.Code)
	}
	if w := postForm(t, r, "/api/settings", "remoteBlock", "true", "remoteHyperdeckPort", "70000"); w.Code != http.StatusBadRequest {
		t.Errorf("bad port = %d, want 400", w.Code)
	}
}

// Like a real deck, one control client at a time: the second is rejected.
func TestDeckSingleClient(t *testing.T) {
	setupTestDB(t)
	addr := freeListenAddr(t)
	go ListenHyperdeck(addr)
	// Earlier deck tests' clients hang up asynchronously; wait for the
	// registry to drain so the limit counts only this test's clients.
	for i := 0; i < 100; i++ {
		deckRegMu.RLock()
		n := len(deckClients)
		deckRegMu.RUnlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer first.Close()
	buf := make([]byte, 512)
	first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := first.Read(buf); !strings.Contains(string(buf[:n]), "500 connection info") {
		t.Fatalf("first client greeting = %q", buf[:n])
	}
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := second.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "120 connection rejected") {
		t.Fatalf("second client got %q, want 120 connection rejected", buf[:n])
	}
}

// The HyperDeck clip list can come from the media pool instead of the sheet.
func TestDeckClipsFromMediaPool(t *testing.T) {
	setupTestDB(t)
	resetRemote(t)
	for _, name := range []string{"pool-a.mp4", "pool-b.mp4"} {
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "video/mp4", Duration: 4}, name); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
	}
	if err := config.SetRemote(config.RemoteSettings{HyperDeckClips: "mediapool"}); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if n, err := clipCount(); err != nil || n != 2 {
		t.Fatalf("clipCount = %d err=%v, want the 2 pool items (sheet is empty)", n, err)
	}
	list, err := clipListResponse("", "", 1)
	if err != nil || !strings.Contains(list, "clip count: 2\r\n") || !strings.Contains(list, "pool-a.mp4") {
		t.Errorf("pool clip list = %q err=%v", list, err)
	}
}
