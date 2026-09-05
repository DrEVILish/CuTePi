package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBroadcastReachesConnectedClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(Handle))
	defer server.Close()

	url := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	// Handle sends an initial sync message when the client connects.
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read initial sync: %v", err)
	}

	Broadcast()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read broadcast: %v", err)
	}
	var message map[string]string
	if err := json.Unmarshal(payload, &message); err != nil || message["type"] != "sync" {
		t.Fatalf("broadcast message = %s, want sync", payload)
	}

	BroadcastMedia()
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read media broadcast: %v", err)
	}
	if err := json.Unmarshal(payload, &message); err != nil || message["type"] != "media" {
		t.Fatalf("media broadcast message = %s, want media", payload)
	}
}
