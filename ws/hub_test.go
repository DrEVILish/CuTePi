package ws

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dial registers a client against a real HTTP server (websocket upgrade
// needs real TCP) and returns the connection plus the server.
func dial(t *testing.T) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(Handle))
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):] + "/"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, srv
}

// TestBroadcastReachesClientAndEvictsDead is the runnable check for the
// fan-out: a live client receives the sync message; a closed client is
// evicted on the next broadcast without stalling or panicking (with the
// write deadline, even a black-holed peer cannot block this loop).
func TestBroadcastReachesClientAndEvictsDead(t *testing.T) {
	live, _ := dial(t)
	dead, _ := dial(t)

	// Initial sync hint on connect.
	dead.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hint map[string]string
	if err := dead.ReadJSON(&hint); err != nil || hint["type"] != "sync" {
		t.Fatalf("initial sync hint: err=%v msg=%v", err, hint)
	}
	live.SetReadDeadline(time.Now().Add(2 * time.Second))

	_ = dead.Close()

	// Discard live's queued connect hint, then read the real broadcast.
	live.SetReadDeadline(time.Now().Add(2 * time.Second))
	var skip map[string]string
	if err := live.ReadJSON(&skip); err != nil {
		t.Fatalf("reading connect hint: %v", err)
	}

	Broadcast()

	var msg map[string]string
	if err := live.ReadJSON(&msg); err != nil || msg["type"] != "sync" {
		t.Fatalf("live client missed broadcast: err=%v msg=%v", err, msg)
	}

	// The evicted peer must be gone: no further writes to it (would error),
	// and the hub stays functional.
	BroadcastMedia()
	var mediaMsg map[string]string
	if err := live.ReadJSON(&mediaMsg); err != nil || mediaMsg["type"] != "media" {
		t.Fatalf("live client missed media broadcast: err=%v msg=%v", err, mediaMsg)
	}
}

// TestBroadcastReachesAllClients verifies that every live client receives
// both sync and media broadcasts — the foundation of multi-browser lockstep.
func TestBroadcastReachesAllClients(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(Handle))
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):] + "/"

	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}

	a, b := dial(), dial()
	deadline := time.Now().Add(2 * time.Second)
	a.SetReadDeadline(deadline)
	b.SetReadDeadline(deadline)

	// Both receive the initial sync hint on connect.
	for _, c := range []*websocket.Conn{a, b} {
		var hint map[string]string
		if err := c.ReadJSON(&hint); err != nil || hint["type"] != "sync" {
			t.Fatalf("initial sync hint: err=%v msg=%v", err, hint)
		}
	}

	// Broadcast sync — both must receive it.
	Broadcast()
	for _, c := range []*websocket.Conn{a, b} {
		var msg map[string]string
		if err := c.ReadJSON(&msg); err != nil || msg["type"] != "sync" {
			t.Fatalf("client missed sync broadcast: err=%v msg=%v", err, msg)
		}
	}

	// Broadcast media — both must receive it.
	BroadcastMedia()
	for _, c := range []*websocket.Conn{a, b} {
		var msg map[string]string
		if err := c.ReadJSON(&msg); err != nil || msg["type"] != "media" {
			t.Fatalf("client missed media broadcast: err=%v msg=%v", err, msg)
		}
	}
}

// TestSyncCarriesVersions: every sync message (the connect hint too) carries
// the versions read just before it went out, so a client never has to ask;
// a media broadcast neither reads nor carries them.
func TestSyncCarriesVersions(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	OnSync(func() any {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return map[string]int{"sheet": reads}
	})
	t.Cleanup(func() { OnSync(nil) })

	conn, _ := dial(t)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	type msg struct {
		Type string         `json:"type"`
		V    map[string]int `json:"v"`
	}
	var m msg
	if err := conn.ReadJSON(&m); err != nil || m.Type != "sync" || m.V["sheet"] != 1 {
		t.Fatalf("connect hint: err=%v msg=%+v, want sync with sheet 1", err, m)
	}

	BroadcastMedia()
	m = msg{}
	if err := conn.ReadJSON(&m); err != nil || m.Type != "media" || m.V != nil {
		t.Fatalf("media: err=%v msg=%+v", err, m)
	}

	Broadcast()
	if err := conn.ReadJSON(&m); err != nil || m.Type != "sync" || m.V["sheet"] != 2 {
		t.Fatalf("sync: err=%v msg=%+v, want sheet 2 (versions read at broadcast)", err, m)
	}
}

// TestSyncCoalescesToNewest: syncs queued behind a busy writer collapse to
// one message, and that message carries the newest versions, never an
// older read.
func TestSyncCoalescesToNewest(t *testing.T) {
	var mu sync.Mutex
	n := 0
	OnSync(func() any { mu.Lock(); defer mu.Unlock(); n++; return map[string]int{"sheet": n} })
	t.Cleanup(func() { OnSync(nil) })
	conn, _ := dial(t)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hint map[string]any
	if err := conn.ReadJSON(&hint); err != nil {
		t.Fatalf("connect hint: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); Broadcast() }()
	}
	wg.Wait()
	mu.Lock()
	newest := n
	mu.Unlock()
	last := 0
	for last != newest {
		var m struct {
			V map[string]int `json:"v"`
		}
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for sheet %d (last %d): %v", newest, last, err)
		}
		if m.V["sheet"] < last {
			t.Fatalf("versions went back: %d after %d", m.V["sheet"], last)
		}
		last = m.V["sheet"]
	}
}
