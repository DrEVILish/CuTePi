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
		CheckOrigin: SameOrigin,
		// A linked unit's failover page sends its token as a subprotocol
		// (DESIGN §6.8d): accepted, so the browser keeps the socket.
		Subprotocols: []string{"cutepi-link"},
	}
	mu       sync.Mutex
	clients  = make(map[*client]bool)
	versions func() any // under mu; see OnSync

	syncMu  sync.Mutex
	syncCur []byte // newest sync message, under syncMu
)

// OnSync sets the reader of the state versions every sync message carries
// ({"type":"sync","v":...}): clients compare them with what they show and
// fetch only the panes that are behind, with no status round trip. It runs
// ahead of each sync fan-out, so it may first fold derived state (which cues
// are running) into those versions, and may itself bump and broadcast: the
// extra signal coalesces into the pending flag.
func OnSync(f func() any) {
	mu.Lock()
	versions = f
	mu.Unlock()
}

// syncMessage reads the versions now and makes that the newest sync message.
// Read and store under one lock, so a slower broadcast can never replace
// newer versions with older ones.
func syncMessage() []byte {
	mu.Lock()
	f := versions
	mu.Unlock()
	syncMu.Lock()
	defer syncMu.Unlock()
	msg := map[string]any{"type": "sync"}
	if f != nil {
		msg["v"] = f()
	}
	syncCur, _ = json.Marshal(msg)
	return syncCur
}

func currentSync() []byte {
	syncMu.Lock()
	defer syncMu.Unlock()
	return syncCur
}

// SameOrigin reports whether r's Origin (or, lacking one, Referer) names
// r's own Host. Requests with neither header are not browser-driven and
// pass. Shared by the upgrader and the HTTP CSRF guard (routes.SameOrigin).
func SameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
		if src == "" {
			return true // not a browser-initiated cross-site request
		}
	}
	if src == "null" {
		return false // sandboxed iframe / opaque origin
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) || (AllowOrigin != nil && AllowOrigin(u.Scheme+"://"+u.Host))
}

// AllowOrigin, when set, admits other origins: a paired unit's failover
// pages (routes, DESIGN §6.8c).
var AllowOrigin func(origin string) bool

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
	if err := conn.WriteMessage(websocket.TextMessage, syncMessage()); err != nil {
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
			}{{sendSync, currentSync()}, {sendMedia, mediaMsg}} {
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

var mediaMsg, _ = json.Marshal(map[string]string{"type": "media"})

func broadcast(media bool) {
	if !media {
		syncMessage() // before any flag is set: every send carries these versions or newer
	}
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
