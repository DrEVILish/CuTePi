package ws

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
		// Allow any origin – trusted local network.
	}
	mu      sync.Mutex
	clients = make(map[*client]bool)
)

type client struct {
	conn  *websocket.Conn
	write sync.Mutex
}

// Handle upgrades the HTTP request to a WebSocket and registers the client.
// It blocks until the client disconnects. Mount as GET /api/ws.
func Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &client{conn: conn}
	mu.Lock()
	clients[c] = true
	mu.Unlock()

	// Send an initial sync hint so the newly-connected client refreshes immediately.
	c.write.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	_ = conn.WriteJSON(map[string]string{"type": "sync"})
	c.write.Unlock()

	// Read loop: we don't expect client messages, but reading detects close.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	mu.Lock()
	delete(clients, c)
	mu.Unlock()
	_ = conn.Close()
}

// Broadcast notifies all connected clients that server state changed.
// Callers (ctp bump, gsp bump) should invoke this after committing the new
// state so browsers can re-fetch the authoritative server rendering.
// Non-blocking: fans out without holding the hub lock during writes.
func Broadcast() {
	broadcast("sync")
}

// BroadcastMedia notifies clients that the media pool partial should be
// refreshed without forcing an unrelated cuesheet refresh.
func BroadcastMedia() {
	broadcast("media")
}

// writeDeadline caps every client write. Without it one stalled TCP peer
// (dead phone, half-closed laptop) blocks broadcast() — which runs on the
// ctp/gsp bump paths — and with it the whole server's HTTP handlers and
// event flow. 2s: slow Wi-Fi clients still get every sync; dead ones are
// evicted at the next broadcast.
// ponytail: serial fan-out means worst case 2s per dead client per
// broadcast; per-client writer goroutines if a real deployment ever
// shows that stall.
const writeDeadline = 2 * time.Second

func broadcast(kind string) {
	mu.Lock()
	if len(clients) == 0 {
		mu.Unlock()
		return
	}
	// Snapshot clients to avoid holding lock during writes.
	snapshot := make([]*client, 0, len(clients))
	for c := range clients {
		snapshot = append(snapshot, c)
	}
	mu.Unlock()

	msg, _ := json.Marshal(map[string]string{"type": kind})
	for _, c := range snapshot {
		c.write.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
		err := c.conn.WriteMessage(websocket.TextMessage, msg)
		c.write.Unlock()
		if err != nil {
			mu.Lock()
			delete(clients, c)
			mu.Unlock()
			_ = c.conn.Close()
		}
	}
}
