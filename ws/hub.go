package ws

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	upgrader = websocket.Upgrader{
		// Same-origin only: a page on another site must not be able to open
		// the push channel with the operator's cached credentials. Clients
		// that send no Origin (non-browser tools) are allowed.
		CheckOrigin: sameOrigin,
	}
	mu      sync.Mutex
	clients = make(map[*client]bool)
)

// sameOrigin reports whether the handshake's Origin (if any) names the
// request's own Host.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// client is one connected browser. Broadcasts never write to the socket
// directly: they set a pending flag and wake the client's own writer
// goroutine, so one stalled peer can never delay the others (or the ctp/gsp
// bump paths that broadcast). Flags coalesce: messages are idempotent
// "re-fetch" hints, so N syncs queued behind a slow write collapse to one.
type client struct {
	conn         *websocket.Conn
	wake         chan struct{} // cap 1
	pmu          sync.Mutex
	pendingSync  bool
	pendingMedia bool
}

// writeDeadline caps every client write: a dead phone or half-closed laptop
// is evicted instead of pinning its writer forever.
const writeDeadline = 2 * time.Second

// readLimit caps a client->server frame. Clients send nothing but control
// frames; anything large is abuse.
const readLimit = 4 << 10

// Handle upgrades the HTTP request to a WebSocket and registers the client.
// It blocks until the client disconnects. Mount as GET /api/ws.
func Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(readLimit)
	c := &client{conn: conn, wake: make(chan struct{}, 1)}

	// Register BEFORE the connect hint: a broadcast racing the handshake
	// then lands as a pending flag the writer delivers after the hint,
	// instead of being lost. The hint is written here, before the writer
	// goroutine starts, so conn still has a single writer.
	mu.Lock()
	clients[c] = true
	mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err := conn.WriteJSON(map[string]string{"type": "sync"}); err != nil {
		drop(c)
		return
	}

	done := make(chan struct{})
	go c.writer(done)

	// Read loop: we don't expect client messages, but reading detects close.
	// Idle policy: the writer pings every 4 minutes; a browser answers with
	// pong, which refreshes the 5-minute read deadline — a dead client (no
	// pongs) is evicted, a healthy one lives forever.
	const readTimeout = 5 * time.Minute
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	close(done)
	drop(c)
}

// writer is the only goroutine that writes to c.conn after registration.
func (c *client) writer(done <-chan struct{}) {
	ping := time.NewTicker(4 * time.Minute)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeDeadline)); err != nil {
				drop(c)
				return
			}
		case <-c.wake:
			c.pmu.Lock()
			sendSync, sendMedia := c.pendingSync, c.pendingMedia
			c.pendingSync, c.pendingMedia = false, false
			c.pmu.Unlock()
			for _, kind := range []struct {
				send bool
				msg  []byte
			}{{sendSync, syncMsg}, {sendMedia, mediaMsg}} {
				if !kind.send {
					continue
				}
				_ = c.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
				if err := c.conn.WriteMessage(websocket.TextMessage, kind.msg); err != nil {
					drop(c)
					return
				}
			}
		}
	}
}

// drop unregisters c and closes its socket (idempotent).
func drop(c *client) {
	mu.Lock()
	delete(clients, c)
	mu.Unlock()
	_ = c.conn.Close()
}

// ClientCount reports how many WebSocket clients are currently connected
// (header connection-status tooltip).
func ClientCount() int {
	mu.Lock()
	defer mu.Unlock()
	return len(clients)
}

// Broadcast notifies all connected clients that server state changed.
// Callers (ctp bump, gsp bump) should invoke this after committing the new
// state so browsers can re-fetch the authoritative server rendering.
// Never blocks on the network.
func Broadcast() {
	broadcast(false)
}

// BroadcastMedia notifies clients that the media pool partial should be
// refreshed without forcing an unrelated cuesheet refresh.
func BroadcastMedia() {
	broadcast(true)
}

var (
	syncMsg, _  = json.Marshal(map[string]string{"type": "sync"})
	mediaMsg, _ = json.Marshal(map[string]string{"type": "media"})
)

func broadcast(media bool) {
	mu.Lock()
	snapshot := make([]*client, 0, len(clients))
	for c := range clients {
		snapshot = append(snapshot, c)
	}
	mu.Unlock()

	for _, c := range snapshot {
		c.pmu.Lock()
		if media {
			c.pendingMedia = true
		} else {
			c.pendingSync = true
		}
		c.pmu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default: // a wake is already queued; it will see this flag
		}
	}
}
