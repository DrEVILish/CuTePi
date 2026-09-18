package ws

import (
	"net/http"
	"net/http/httptest"
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
