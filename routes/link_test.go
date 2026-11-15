package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/gsp"
)

// resetLink puts the link state back to a fresh unit "m" in mode, with the
// given peers (not started: no queue workers, no network).
func resetLink(t *testing.T, mode string, peers ...*peerState) {
	t.Helper()
	link.mu.Lock()
	defer link.mu.Unlock()
	link.self, link.mode = "m", mode
	link.term, link.lastTerm, link.lastSeq = 1, 1, 5
	link.primary, link.lostAt = "", time.Time{}
	link.peers = map[string]*peerState{}
	for _, p := range peers {
		p.queue = make(chan peerCall, 64)
		link.peers[p.ID] = p
	}
	link.incoming = map[string]pairRequest{}
	link.outgoing = map[string]string{}
	link.origins = map[string]string{}
}

func alivePeer(id string, h peerHello) *peerState {
	h.ID, h.Mode = id, linkModeBackup
	return &peerState{LinkPeer: ctp.LinkPeer{ID: id, Name: id, URL: "http://" + id, Secret: "s-" + id}, seen: time.Now(), hello: h}
}

// Takeover: a lost primary leaves no primary (edits refused) for
// linkSettle; then the best-ranked reachable unit takes over: the newest
// show version, then the lowest id.
func TestLinkTakeover(t *testing.T) {
	setupTestServer(t)
	z := alivePeer("z", peerHello{Term: 1, LastTerm: 1, LastSeq: 5})
	p := alivePeer("a", peerHello{Term: 1, Primary: "a", LastTerm: 1, LastSeq: 5})
	resetLink(t, linkModeBackup, p, z)
	linkElect()
	if link.primary != "a" {
		t.Fatalf("following %q, want a", link.primary)
	}
	if _, ok := linkPrimary(); !ok {
		t.Fatal("edits refused with a live primary")
	}

	p.seen = time.Now().Add(-3 * time.Second) // the primary stops answering
	linkElect()
	if _, ok := linkPrimary(); ok {
		t.Fatal("edits accepted while no unit is primary")
	}
	link.lostAt = time.Now().Add(-linkSettle - time.Millisecond)
	linkElect()
	if link.primary != "m" || link.term != 2 {
		t.Fatalf("primary %q term %d, want m (lowest id of m, z) at term 2", link.primary, link.term)
	}

	// A unit holding a newer show takes over before a lower id.
	resetLink(t, linkModeBackup, alivePeer("z", peerHello{Term: 1, LastTerm: 1, LastSeq: 6}))
	link.lostAt = time.Now().Add(-linkSettle - time.Millisecond)
	linkElect()
	if link.primary != "" {
		t.Fatalf("m (seq 5) took over from z (seq 6)")
	}

	// Two primaries (a healed partition): the higher term wins.
	resetLink(t, linkModeBackup, alivePeer("z", peerHello{Term: 3, Primary: "z", LastTerm: 3, LastSeq: 1}))
	link.primary = "m"
	linkElect()
	if link.primary != "z" || link.term != 3 {
		t.Fatalf("primary %q term %d after a higher-term claim, want z 3", link.primary, link.term)
	}
}

// Ordering: a unit applies show versions in (term, seq) order, from its
// primary (or a newer term), and ignores older or repeated ones.
func TestLinkApplyOrder(t *testing.T) {
	setupTestServer(t)
	resetLink(t, linkModeBackup, alivePeer("a", peerHello{}), alivePeer("x", peerHello{}))
	link.primary = "a"
	if err := ctp.ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	snap := func(goAdvance string) json.RawMessage {
		return json.RawMessage(`{"cues":[],"groups":[],"state":{"goAdvance":"` + goAdvance + `"}}`)
	}
	steps := []struct {
		from      string
		term, seq uint64
		goAdv     string
		want      bool // GO-advance after the step
	}{
		{"a", 1, 6, "0", false}, // newer: applied
		{"a", 1, 6, "1", false}, // repeated seq: ignored
		{"a", 1, 4, "1", false}, // older: ignored
		{"x", 1, 7, "1", false}, // not the primary, same term: ignored
		{"a", 1, 8, "1", true},  // newer: applied
		{"x", 2, 1, "0", false}, // newer term (x took over): applied
		{"a", 1, 9, "1", false}, // the old primary's late version: ignored
	}
	for i, s := range steps {
		if err := applyShow(s.from, showMsg{Term: s.term, Seq: s.seq, Show: snap(s.goAdv)}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := ctp.GetGoAdvance(); got != s.want {
			t.Fatalf("step %d (%s %d.%d): GO-advance %v, want %v", i, s.from, s.term, s.seq, got, s.want)
		}
	}
	if link.primary != "x" || link.lastTerm != 2 || link.lastSeq != 1 {
		t.Fatalf("primary %q version %d.%d, want x 2.1", link.primary, link.lastTerm, link.lastSeq)
	}
	// A change made here, not through the primary, is undone.
	if err := ctp.SetGoAdvance(true); err != nil {
		t.Fatal(err)
	}
	linkUndoDrift()
	if ctp.GetGoAdvance() {
		t.Fatal("local change outside the primary not undone")
	}
}

// Sequencing: every change on the primary gets the next seq; a request that
// changes nothing does not.
func TestLinkSequence(t *testing.T) {
	setupTestServer(t)
	resetLink(t, linkModeBackup)
	link.primary = "m"
	seqSnap, seqVer = nil, 0
	linkSequence()
	base := link.lastSeq
	linkSequence()
	if link.lastSeq != base {
		t.Fatalf("seq moved without a change: %d -> %d", base, link.lastSeq)
	}
	for i := 1; i <= 3; i++ {
		if err := ctp.SetGoAdvance(i%2 == 0); err != nil {
			t.Fatal(err)
		}
		linkSequence()
		if link.lastSeq != base+uint64(i) || link.lastTerm != link.term {
			t.Fatalf("change %d: version %d.%d, want %d.%d", i, link.lastTerm, link.lastSeq, link.term, base+uint64(i))
		}
	}
}

// Pairing auth: peer routes need the pairing secret; the pairing answer
// needs the requester's nonce; a failover page token works only for the
// origin it was issued to, and only registered origins get CORS.
func TestLinkAuth(t *testing.T) {
	r := setupTestServer(t)
	resetLink(t, linkModeBackup, alivePeer("a", peerHello{}))
	do := func(method, path, key string, hdr map[string]string, body []byte) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if key != "" {
			req.Header.Set(peerKeyHeader, key)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if c := do("GET", "/api/peer/hello", "", nil, nil); c != http.StatusForbidden {
		t.Fatalf("hello without a key: %d", c)
	}
	if c := do("GET", "/api/peer/hello", "a wrong", nil, nil); c != http.StatusForbidden {
		t.Fatalf("hello with a wrong secret: %d", c)
	}
	if c := do("GET", "/api/peer/hello", "a s-a", nil, nil); c != http.StatusOK {
		t.Fatalf("hello with the secret: %d", c)
	}
	accept, _ := json.Marshal(gin.H{"id": "b", "name": "b", "nonce": "guess", "secret": "0123456789abcdef0123456789abcdef"})
	if c := do("POST", "/api/peer/pair/accept", "", nil, accept); c != http.StatusForbidden {
		t.Fatalf("pairing answer with an unknown nonce: %d", c)
	}

	exp := time.Now().Add(time.Hour).Unix()
	tok := "a." + strconv.FormatInt(exp, 10) + "." + pageMAC("s-a", "http://unit-a", exp)
	check := func(origin string) bool {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set(pageKeyHeader, tok)
		return pageTokenOK(req)
	}
	if !check("http://unit-a") || check("http://evil") {
		t.Fatal("page token not bound to its origin")
	}
	ws := httptest.NewRequest("GET", "/api/ws", nil)
	ws.Header.Set("Origin", "http://unit-a")
	ws.Header.Set("Sec-WebSocket-Protocol", "cutepi-link, "+tok)
	if !pageTokenOK(ws) {
		t.Fatal("page token as a WebSocket subprotocol refused")
	}
	pre := map[string]string{"Origin": "http://unit-a", "Access-Control-Request-Method": "POST"}
	eng := gin.New()
	eng.Use(SameOrigin(), AuthMiddleware())
	eng.POST("/api/stop", func(c *gin.Context) { c.Status(http.StatusOK) })
	run := func(method string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/stop", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		eng.ServeHTTP(w, req)
		return w
	}
	if w := run("POST", pre); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST from an unregistered origin: %d", w.Code)
	}
	link.origins["http://unit-a"] = "a"
	if w := run("OPTIONS", pre); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "http://unit-a" {
		t.Fatalf("preflight from a paired page origin: %d %v", w.Code, w.Header())
	}
	if w := run("POST", pre); w.Code != http.StatusForbidden {
		t.Fatalf("paired origin without token: %d", w.Code)
	}
	pre[pageKeyHeader] = tok
	if w := run("POST", pre); w.Code != http.StatusOK {
		t.Fatalf("paired origin with token: %d", w.Code)
	}
	if w := run("OPTIONS", map[string]string{"Origin": "http://evil"}); w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS for an unregistered origin")
	}
}

func TestLinkLead(t *testing.T) {
	ms := func(d time.Duration) float64 { return float64(d) / 1e6 }
	floor := ms(gsp.MinStartLead)
	// A slow load (140 ms) over a wired LAN: load + applyStartAt's floor + trip + margin.
	if got := ms(leadFor(1, 140)); got != 1+140+floor/2+linkLeadMargin {
		t.Errorf("slow lead = %v ms", got)
	}
	// A warm swap (2 ms) over the mesh is held to StartAt's floor on arrival.
	if got := ms(leadFor(12, 2)); got != 12+floor+linkLeadMargin {
		t.Errorf("warm lead = %v ms", got)
	}
	if got := leadFor(50, 5000); got != linkLeadMax {
		t.Errorf("capped lead = %v", got)
	}
	// The median of the last loads: one cold load does not stretch the lead.
	prep.Lock()
	prep.n, prep.ms = 0, [5]float64{}
	prep.Unlock()
	if got := prepMs(); got != linkPrepDefault {
		t.Errorf("no loads: %v", got)
	}
	for _, d := range []float64{140, 650, 150} {
		prep.Lock()
		prep.ms[prep.n%5] = d
		prep.n++
		prep.Unlock()
	}
	if got := prepMs(); got != 150 {
		t.Errorf("median = %v", got)
	}
}

func TestBackupGroupMirrorIncludesSharedInstant(t *testing.T) {
	peer := alivePeer("other", peerHello{})
	resetLink(t, linkModeBackup, peer)
	t.Cleanup(func() { resetLink(t, linkModeOff) })
	link.mu.Lock()
	link.primary = link.self
	link.mu.Unlock()
	at := linkMirrorGo(0, 7)
	if at.IsZero() {
		t.Fatal("no shared instant")
	}
	select {
	case call := <-peer.queue:
		want := "/api/group/7/play?linked=1&at=" + strconv.FormatInt(at.UnixMilli(), 10)
		if call.path != want {
			t.Fatalf("mirrored group request = %q, want %q", call.path, want)
		}
	default:
		t.Fatal("group GO was not mirrored")
	}
}

func TestLinkPageRejectsForeignOrigin(t *testing.T) {
	setupTestServer(t)
	peer := alivePeer("other", peerHello{})
	resetLink(t, linkModeBackup, peer)
	t.Cleanup(func() { resetLink(t, linkModeOff) })
	r := gin.New()
	UseMiddleware(r)
	registerLinkRoutes(r.Group("/api"))
	req := httptest.NewRequest(http.MethodGet, "http://cutepi.example/api/link/page", nil)
	req.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin got %d", w.Code)
	}
	if len(peer.queue) != 0 {
		t.Fatal("foreign origin was sent to a peer")
	}
	// Reverse-proxy names still work and can register their own page.
	req = httptest.NewRequest(http.MethodGet, "http://cutepi-test.drevilish.com/api/link/page", nil)
	req.Header.Set("Origin", "http://cutepi-test.drevilish.com")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("proxy origin got %d", w.Code)
	}
}

func TestUnpairedOriginCannotUseOldGrant(t *testing.T) {
	setupTestServer(t)
	resetLink(t, linkModeBackup, alivePeer("gone", peerHello{}))
	link.origins["http://gone"] = "gone"
	delete(link.peers, "gone")
	if linkOriginAllowed("http://gone") {
		t.Fatal("origin grant survived peer removal")
	}
}
