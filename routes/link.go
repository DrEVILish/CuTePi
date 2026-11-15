package routes

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/ws"
)

// Linked units (DESIGN §6.8c). Paired units run in one of two modes:
//
//   - Synced playback: own shows, own media. GO on any unit fires every
//     unit's own selected cue for one instant linkLead() ahead (each applies
//     its own GO-advance); Stop, ESC and Panic are mirrored.
//   - Active/active backup: one shared show. One unit is the primary: every
//     show edit, selection change and transport command made on any unit is
//     forwarded to it, it applies it, sequences the resulting show (term,
//     seq) and sends the whole replicated show to the others, which apply
//     versions in order. Transport commands are replayed to the others,
//     fires for one shared instant.
//
// Peer calls are plain HTTP authenticated with the pairing secret
// (X-CuTePi-Key: <sender id> <secret>). A request a peer sends with
// ?linked=1 is acted on here and not passed on.

const (
	linkModeOff    = ""
	linkModeSynced = "synced"
	linkModeBackup = "backup"

	// Timeouts (DESIGN §6.8c): every unit polls every peer each linkPoll; a
	// peer silent for linkDeadAfter is down. A follower whose primary is
	// down refuses edits, waits linkSettle so every unit has seen the loss
	// (they poll the same primary on the same timeout), then the
	// best-ranked unit it can reach takes over: the newest show version,
	// then the lowest stable id. Takeover: 2.5-3 s after the last answer, or
	// ~1.5 s after a forwarded request finds the primary gone.
	linkPoll      = 500 * time.Millisecond
	linkDeadAfter = 1500 * time.Millisecond
	linkSettle    = 1 * time.Second
	linkHelloWait = 400 * time.Millisecond

	peerKeyHeader  = "X-CuTePi-Key"
	pageKeyHeader  = "X-CuTePi-Link"
	ctxLinkPeer    = "linkPeer"
	pageTokenLife  = 12 * time.Hour
	linkClockEvery = 10 * time.Second
	linkMediaEvery = 30 * time.Second
	clockWarnMs    = 5.0
)

var (
	peerClient    = &http.Client{Timeout: 2 * time.Second, CheckRedirect: noRedirect}
	forwardClient = &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect}
	helloClient   = &http.Client{Timeout: linkHelloWait, CheckRedirect: noRedirect}
)

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// peerHello is what a unit tells its peers about itself every poll.
type peerHello struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Mode     string  `json:"mode"`
	Primary  string  `json:"primary"` // the unit it takes for primary ("" = unclear)
	Term     uint64  `json:"term"`
	LastTerm uint64  `json:"lastTerm"` // version of the show it holds
	LastSeq  uint64  `json:"lastSeq"`
	PrepMs   float64 `json:"prepMs"` // its recent cue load time (linkLead)
}

// peerState is a paired unit and what this unit last learnt about it.
type peerState struct {
	ctp.LinkPeer
	seen     time.Time
	hello    peerHello
	err      string
	offsetMs float64
	rttMs    float64
	slowMs   float64 // slowest recent hello round trip, decaying (linkLead)
	clockAt  time.Time
	media    mediaDiff
	queue    chan peerCall
}

type peerCall struct {
	method, path, ctype string
	body                []byte
}

type pairRequest struct {
	ID, Name, URL, Mode, Nonce string
	at                         time.Time
}

var link struct {
	mu       sync.Mutex
	self     string
	mode     string
	peers    map[string]*peerState
	term     uint64
	lastTerm uint64
	lastSeq  uint64
	primary  string
	lostAt   time.Time
	incoming map[string]pairRequest // by requester id, awaiting accept here
	outgoing map[string]string      // nonce -> peer URL, awaiting accept there
	origins  map[string]string      // browser origin -> peer id (failover pages)
	syncNote string                 // media sync progress
}

// sequencing (primary): the last show snapshot sent, as of which version.
// Following (backup): the last version applied, how it reads back here, and
// as of which local sheet version that was checked.
var (
	seqMu   sync.Mutex
	seqVer  uint64
	seqSnap []byte

	applyMu      sync.Mutex
	appliedSnap  []byte
	appliedLocal []byte
	driftVer     uint64
)

func init() {
	link.peers = map[string]*peerState{}
	link.incoming = map[string]pairRequest{}
	link.outgoing = map[string]string{}
	link.origins = map[string]string{}
	ws.AllowOrigin = linkOriginAllowed
}

// RunLink loads the link state, migrates the old linked_peers setting,
// starts mDNS and runs the peer poll. Call once at startup.
func RunLink() {
	link.mu.Lock()
	link.self = ctp.LinkState("id")
	if link.self == "" {
		link.self = randomHex(8)
		if err := ctp.SetLinkState("id", link.self); err != nil {
			logs.PrintfWarn(logs.LNKWarn, "saving link id: %v", err)
		}
	}
	link.mode = ctp.LinkState("mode")
	link.term = parseU(ctp.LinkState("term"))
	link.lastTerm = parseU(ctp.LinkState("lastTerm"))
	link.lastSeq = parseU(ctp.LinkState("lastSeq"))
	self := link.self
	link.mu.Unlock()
	migrateLinkedPeers()
	reloadPeers()
	startMDNS(config.Port(), self, InstanceName()+" "+self[:6])
	t := time.NewTicker(linkPoll)
	defer t.Stop()
	for range t.C {
		linkTick()
	}
}

// migrateLinkedPeers turns the old linked_peers URLs into Synced playback
// peers (no pairing secret: a password in the URL still authenticates them).
func migrateLinkedPeers() {
	old := config.LinkedPeers()
	if len(old) == 0 {
		return
	}
	for _, u := range old {
		p := ctp.LinkPeer{ID: "url:" + config.RedactURL(u), Name: hostOf(u), URL: u}
		if err := ctp.SaveLinkPeer(p); err != nil {
			logs.PrintfWarn(logs.LNKWarn, "migrating linked unit %s: %v", config.RedactURL(u), err)
			return
		}
	}
	if link.mode == linkModeOff {
		setLinkMode(linkModeSynced)
	}
	if err := config.SetLinkedPeers(nil); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "clearing linked_peers: %v", err)
	}
	logs.Printf(logs.LNKInfo, "migrated %d linked unit(s) to Synced playback", len(old))
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Hostname()
	}
	return u
}

// reloadPeers syncs the in-memory peers with link_peer.
func reloadPeers() {
	list, err := ctp.LinkPeers()
	if err != nil {
		logs.PrintfWarn(logs.LNKWarn, "reading linked units: %v", err)
		return
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	keep := map[string]bool{}
	for _, p := range list {
		keep[p.ID] = true
		if st, ok := link.peers[p.ID]; ok {
			st.LinkPeer = p
			continue
		}
		st := &peerState{LinkPeer: p, queue: make(chan peerCall, 256)}
		link.peers[p.ID] = st
		go st.run()
	}
	for id, st := range link.peers {
		if !keep[id] {
			close(st.queue)
			delete(link.peers, id)
		}
	}
}

// run delivers this peer's queued calls in order (replays, show pushes).
func (st *peerState) run() {
	for call := range st.queue {
		resp, err := peerDo(context.Background(), st.LinkPeer, peerClient, call.method, call.path, bytes.NewReader(call.body), call.ctype)
		if err != nil {
			logs.PrintfWarn(logs.LNKWarn, "linked unit %s: %s %s: %s", st.Name, call.method, call.path, redactURLErr(err, st.URL))
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			logs.PrintfWarn(logs.LNKWarn, "linked unit %s: %s %s: HTTP %d", st.Name, call.method, call.path, resp.StatusCode)
		}
	}
}

// peerDo makes one authenticated call to p.
func peerDo(ctx context.Context, p ctp.LinkPeer, cl *http.Client, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.URL+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if p.Secret != "" {
		req.Header.Set(peerKeyHeader, selfID()+" "+p.Secret)
	}
	return cl.Do(req)
}

func peerJSON(p ctp.LinkPeer, cl *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	resp, err := peerDo(context.Background(), p, cl, method, path, body, "application/json")
	if err != nil {
		return errors.New(redactURLErr(err, p.URL))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// enqueue sends a call to the given peers (all when ids is nil), in order.
func enqueue(ids []string, method, path, ctype string, body []byte) {
	link.mu.Lock()
	defer link.mu.Unlock()
	for id, st := range link.peers {
		if ids != nil && !contains(ids, id) {
			continue
		}
		if path == "/api/peer/show" && time.Since(st.seen) > linkDeadAfter {
			continue // a unit that is down catches up when it is back
		}
		select {
		case st.queue <- peerCall{method, path, ctype, body}:
		default:
			logs.PrintfWarn(logs.LNKWarn, "linked unit %s: queue full, dropped %s", st.Name, path)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func selfID() string {
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.self
}

func linkMode() string {
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.mode
}

func setLinkMode(mode string) {
	link.mu.Lock()
	link.mode = mode
	link.primary, link.lostAt = "", time.Time{}
	link.mu.Unlock()
	if err := ctp.SetLinkState("mode", mode); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "saving link mode: %v", err)
	}
	logs.Printf(logs.LNKInfo, "linked units: mode %q", mode)
}

func saveLinkVersion() {
	link.mu.Lock()
	kv := []string{"term", strconv.FormatUint(link.term, 10), "lastTerm", strconv.FormatUint(link.lastTerm, 10),
		"lastSeq", strconv.FormatUint(link.lastSeq, 10)}
	link.mu.Unlock()
	if err := ctp.SetLinkState(kv...); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "saving link state: %v", err)
	}
}

func parseU(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// peerIDFromKey returns the paired unit that r's X-CuTePi-Key names and
// proves, or "".
func peerIDFromKey(r *http.Request) string {
	id, secret, ok := strings.Cut(r.Header.Get(peerKeyHeader), " ")
	if !ok || secret == "" {
		return ""
	}
	link.mu.Lock()
	st := link.peers[id]
	link.mu.Unlock()
	if st == nil || st.Secret == "" || subtle.ConstantTimeCompare([]byte(st.Secret), []byte(secret)) != 1 {
		return ""
	}
	return id
}

// fromLink reports whether c is a call a paired unit made with ?linked=1:
// act on it here, do not pass it on.
func fromLink(c *gin.Context) bool {
	return c.GetString(ctxLinkPeer) != "" && c.Query("linked") == "1"
}

// ---- failover pages (phase 2) ----
//
// A page loaded from unit A also talks to its paired units directly. A gives
// the page a token per peer, bound to the page's origin and an expiry and
// signed with the pairing secret (the secret itself never reaches the page);
// A tells the peers that origin, which they then allow for CORS, WebSockets
// and the cross-site check. No Host check is involved.

func pageMAC(secret, origin string, exp int64) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "page\n%s\n%d", origin, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// pageTokenOK reports whether r carries a failover page token (header, or
// on the WebSocket the subprotocol after "cutepi-link": never in the URL,
// which the request log records) that a paired unit issued for r's Origin.
func pageTokenOK(r *http.Request) bool {
	tok := r.Header.Get(pageKeyHeader)
	if p := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ","); tok == "" && len(p) == 2 && strings.TrimSpace(p[0]) == "cutepi-link" {
		tok = strings.TrimSpace(p[1])
	}
	parts := strings.Split(tok, ".")
	origin := r.Header.Get("Origin")
	if len(parts) != 3 || origin == "" {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	link.mu.Lock()
	st := link.peers[parts[0]]
	link.mu.Unlock()
	return st != nil && st.Secret != "" && hmac.Equal([]byte(pageMAC(st.Secret, origin, exp)), []byte(parts[2]))
}

// linkOriginAllowed reports whether origin ("scheme://host") served a
// failover page of a paired unit.
func linkOriginAllowed(origin string) bool {
	link.mu.Lock()
	defer link.mu.Unlock()
	_, ok := link.origins[strings.ToLower(origin)]
	return ok
}

// linkCORS answers for a paired unit's page origin: CORS headers, and the
// preflight itself. Returns true when it handled the request.
func linkCORS(c *gin.Context) bool {
	origin := c.GetHeader("Origin")
	if origin == "" || !linkOriginAllowed(origin) {
		return false
	}
	h := c.Writer.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "*")
	if c.Request.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH")
		h.Set("Access-Control-Allow-Headers", c.GetHeader("Access-Control-Request-Headers"))
		h.Set("Access-Control-Max-Age", "600")
		c.AbortWithStatus(http.StatusNoContent)
		return true
	}
	return false
}

// ---- the request middleware ----

// Mode 1: the actions mirrored to every unit (GO goes through fireSelected).
var syncedMirror = map[string]bool{
	"/api/stop": true, "/api/esc": true, "/api/fadeOut": true, "/api/panic": true,
}

// Mode 2: the requests that change the shared show (edit) or the shared
// playback (play). Everything else (settings, display, audio, network, test
// patterns, panic image, media pool, live-page keys) stays per unit.
const (
	routeEdit = 1
	routePlay = 2
)

var backupRoutes = map[string]int{
	"/api/cue/add/:filename": routeEdit, "/api/cue/add/:filename/*cuePos": routeEdit,
	"/api/cue/selectall": routeEdit, "/api/cue/bulk": routeEdit, "/api/cue/bulkgroupnew": routeEdit,
	"/api/sheet/drop": routeEdit, "/api/cue/sort": routeEdit, "/api/cue/renumber": routeEdit,
	"/api/setting/goadvance": routeEdit, "/api/setting/showmode": routeEdit, "/api/clear": routeEdit,
	"/api/cue/next": routeEdit, "/api/cue/prev": routeEdit, "/api/cue/inspector/:cuePos": routeEdit,
	"/api/cue/:cuePos/replace": routeEdit, "/api/cue/:cuePos": routeEdit, "/api/cue/:cuePos/edit/:col": routeEdit,
	"/api/cue/:cuePos/schedule": routeEdit, "/api/cue/:cuePos/group": routeEdit, "/api/cue/live": routeEdit,
	"/api/show/import": routeEdit,
	"/api/group/add":   routeEdit, "/api/group/:id": routeEdit, "/api/group/:id/edit/name": routeEdit,
	"/api/group/:id/name": routeEdit, "/api/group/:id/move": routeEdit, "/api/group/:id/color": routeEdit,
	"/api/group/:id/collapse": routeEdit, "/api/group/:id/select": routeEdit, "/api/group/selected/open": routeEdit,
	"/api/group/selected/close": routeEdit, "/api/group/:id/edit/cueNum": routeEdit, "/api/group/:id/cue_num": routeEdit,
	// GO: the primary fires the selected cue on every unit (fireSelected).
	"/api/cue/selected/play": routeEdit,

	"/api/play": routePlay, "/api/cue/play": routePlay, "/api/pause": routePlay, "/api/togglePause": routePlay,
	"/api/seek": routePlay, "/api/volume": routePlay, "/api/fadeOut": routePlay, "/api/panic": routePlay,
	"/api/stop": routePlay, "/api/esc": routePlay, "/api/cue/:cuePos/play": routePlay,
	"/api/group/:id/play": routePlay, "/api/activecues/:cuePos/stop": routePlay,
	"/api/activecues/:cuePos/fade": routePlay, "/api/play/:filename": routePlay,
}

// LinkMiddleware mirrors (Synced playback) or forwards and replays
// (Active/active backup) the requests above.
func LinkMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		route := c.FullPath()
		switch linkMode() {
		case linkModeSynced:
			if !syncedMirror[route] || c.Query("linked") == "1" {
				c.Next()
				return
			}
			c.Next()
			if c.Writer.Status() < 400 {
				enqueue(nil, c.Request.Method, c.Request.URL.Path+"?linked=1", "", nil)
			}
		case linkModeBackup:
			kind := backupRoutes[route]
			if kind == 0 || fromLink(c) {
				c.Next()
				return
			}
			primary, ok := linkPrimary()
			// A request forwarded here is never forwarded on: two units each
			// taking the other for primary (for one poll) would bounce it.
			if !ok || (primary != nil && c.GetString(ctxLinkPeer) != "") {
				c.Header("X-CuTePi-Link-Election", "1")
				respondError(c, http.StatusServiceUnavailable, "linked units are choosing a primary: try again in a moment")
				c.Abort()
				return
			}
			if primary != nil {
				linkForward(c, primary)
				return
			}
			var body []byte
			if kind == routePlay {
				body, _ = io.ReadAll(c.Request.Body)
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				if (route == "/api/cue/:cuePos/play" || route == "/api/group/:id/play") && c.Query("at") == "" {
					q := c.Request.URL.Query()
					q.Set("at", strconv.FormatInt(time.Now().Add(linkLead()).UnixMilli(), 10))
					c.Request.URL.RawQuery = q.Encode()
				}
			}
			c.Next()
			if kind == routePlay && c.Writer.Status() < 400 {
				q := c.Request.URL.Query()
				q.Set("linked", "1")
				enqueue(nil, c.Request.Method, c.Request.URL.EscapedPath()+"?"+q.Encode(), c.GetHeader("Content-Type"), body)
			}
			linkSequence()
		default:
			c.Next()
		}
	}
}

// linkPrimary returns the primary to forward to (nil: this unit is the
// primary); ok is false while it is unclear who the primary is.
func linkPrimary() (primary *ctp.LinkPeer, ok bool) {
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.primary == "" {
		return nil, false
	}
	if link.primary == link.self {
		return nil, true
	}
	st := link.peers[link.primary]
	if st == nil || time.Since(st.seen) > linkDeadAfter {
		return nil, false
	}
	p := st.LinkPeer
	return &p, true
}

// forwardHeaders are the request headers a forwarded request keeps.
var forwardHeaders = []string{"Content-Type", "Accept", "Content-Length"}

// linkForward sends c's request to the primary, brings this unit's show up
// to the version it produced, then answers with the primary's response.
func linkForward(c *gin.Context, primary *ctp.LinkPeer) {
	defer c.Abort()
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, primary.URL+c.Request.URL.RequestURI(), c.Request.Body)
	if err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	req.ContentLength = c.Request.ContentLength
	for k, v := range c.Request.Header {
		if contains(forwardHeaders, k) || strings.HasPrefix(k, "Hx-") {
			req.Header[k] = v
		}
	}
	if primary.Secret != "" {
		req.Header.Set(peerKeyHeader, selfID()+" "+primary.Secret)
	}
	resp, err := forwardClient.Do(req)
	if err != nil {
		// Taken as down until it answers a poll again: the next requests are
		// refused at once, and the takeover starts now.
		link.mu.Lock()
		if st := link.peers[primary.ID]; st != nil {
			st.seen = time.Time{}
		}
		link.mu.Unlock()
		logs.PrintfWarn(logs.LNKWarn, "forwarding %s to the primary %s: %s", c.Request.URL.Path, primary.Name, redactURLErr(err, primary.URL))
		c.Header("X-CuTePi-Link-Election", "1")
		respondError(c, http.StatusServiceUnavailable, "the primary unit is not answering: try again in a moment")
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		respondError(c, http.StatusBadGateway, "reading the primary's answer: "+err.Error())
		return
	}
	linkPull(*primary)
	for k, v := range resp.Header {
		if k == "Content-Type" || k == "Location" || strings.HasPrefix(k, "Hx-") || strings.HasPrefix(k, "X-") {
			c.Writer.Header()[k] = v
		}
	}
	// The sheet's version is this unit's counter, not the primary's: the
	// page compares it with this unit's later signals.
	body = sheetVersionRe.ReplaceAll(body, []byte(`id="cuesheet" data-version="`+strconv.FormatUint(ctp.CuesheetVersion(), 10)+`"`))
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

var sheetVersionRe = regexp.MustCompile(`id="cuesheet" data-version="\d+"`)

// ---- replication ----

type showMsg struct {
	Term uint64          `json:"term"`
	Seq  uint64          `json:"seq"`
	Show json.RawMessage `json:"show"`
}

// linkSequence (primary): when the show changed since the last version,
// number the new version and send it to every unit. Called after each
// forwarded or local request, each poll (changes by the scheduler or the
// remote protocols), and before serving the show to a peer.
func linkSequence() {
	link.mu.Lock()
	primary := link.mode == linkModeBackup && link.primary == link.self
	link.mu.Unlock()
	if !primary {
		return
	}
	seqMu.Lock()
	defer seqMu.Unlock()
	v := ctp.CuesheetVersion()
	if v == seqVer && seqSnap != nil {
		return
	}
	snap, err := ctp.ExportShowSnapshot()
	if err != nil {
		logs.PrintfWarn(logs.LNKWarn, "reading the show: %v", err)
		return
	}
	seqVer = v
	if bytes.Equal(snap, seqSnap) {
		return
	}
	seqSnap = snap
	link.mu.Lock()
	link.lastTerm = link.term
	link.lastSeq++
	msg, _ := json.Marshal(showMsg{link.term, link.lastSeq, snap})
	link.mu.Unlock()
	saveLinkVersion()
	enqueue(nil, http.MethodPost, "/api/peer/show", "application/json", msg)
}

// newer reports whether version (t, s) is after (t0, s0).
func newer(t, s, t0, s0 uint64) bool {
	return t > t0 || (t == t0 && s > s0)
}

// applyShow applies a sequenced show version from the primary when it is
// newer than the one held here.
func applyShow(from string, m showMsg) error {
	applyMu.Lock()
	defer applyMu.Unlock()
	link.mu.Lock()
	ok := link.mode == linkModeBackup && from != link.self && (from == link.primary || m.Term > link.term) &&
		newer(m.Term, m.Seq, link.lastTerm, link.lastSeq)
	link.mu.Unlock()
	if !ok {
		return nil
	}
	was, _ := ctp.SelectedCuePos()
	if err := ctp.ApplyShowSnapshot(m.Show); err != nil {
		return err
	}
	// The primary's GO advanced the selection: warm the next cue here too,
	// as fireSelected does on the primary, so the next shared instant finds
	// it prerolled.
	if pos, err := ctp.SelectedCuePos(); err == nil && pos != 0 && pos != was {
		armNextCue(gsp.Generation(), pos)
	}
	appliedSnap = m.Show
	appliedLocal, _ = ctp.ExportShowSnapshot()
	driftVer = ctp.CuesheetVersion()
	link.mu.Lock()
	link.lastTerm, link.lastSeq = m.Term, m.Seq
	if m.Term > link.term {
		link.term, link.primary = m.Term, from
	}
	link.mu.Unlock()
	saveLinkVersion()
	return nil
}

// linkUndoDrift (follower): a change made here outside replication (a
// pool file deleted with its cues, a remote protocol's edit) is undone by
// applying the primary's last version again: the shared show changes only
// through the primary.
func linkUndoDrift() {
	applyMu.Lock()
	defer applyMu.Unlock()
	v := ctp.CuesheetVersion()
	if appliedSnap == nil || v == driftVer {
		return
	}
	driftVer = v
	if local, err := ctp.ExportShowSnapshot(); err != nil || bytes.Equal(local, appliedLocal) {
		return
	}
	logs.PrintfWarn(logs.LNKWarn, "linked units: a show change made here outside the primary was undone (edits go through the primary)")
	if err := ctp.ApplyShowSnapshot(appliedSnap); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "restoring the shared show: %v", err)
		return
	}
	appliedLocal, _ = ctp.ExportShowSnapshot()
	driftVer = ctp.CuesheetVersion()
}

// linkPull fetches and applies the primary's current show if it is newer.
func linkPull(primary ctp.LinkPeer) {
	var m showMsg
	if err := peerJSON(primary, peerClient, http.MethodGet, "/api/peer/show", nil, &m); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "fetching the show from %s: %v", primary.Name, err)
		return
	}
	if err := applyShow(primary.ID, m); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "applying the show from %s: %v", primary.Name, err)
	}
}

// ---- the poll and the election ----

var lastClock, lastMedia time.Time

func linkTick() {
	link.mu.Lock()
	mode := link.mode
	peers := make([]*peerState, 0, len(link.peers))
	for _, st := range link.peers {
		peers = append(peers, st)
	}
	link.mu.Unlock()
	if mode == linkModeOff || len(peers) == 0 {
		if mode == linkModeBackup {
			linkElect() // alone: becomes primary
			linkSequence()
		}
		return
	}
	var wg sync.WaitGroup
	for _, st := range peers {
		wg.Add(1)
		goSafe(func() {
			defer wg.Done()
			var h peerHello
			t0 := time.Now()
			err := peerJSON(st.LinkPeer, helloClient, http.MethodGet, "/api/peer/hello", nil, &h)
			rtt := float64(time.Since(t0)) / 1e6
			link.mu.Lock()
			if err == nil && h.ID == st.ID {
				st.seen, st.hello, st.err = time.Now(), h, ""
				st.slowMs = max(rtt, st.slowMs*0.9)
			} else if err != nil {
				st.err = err.Error()
			} else {
				st.err = "answered as another unit (" + h.ID + ")"
			}
			link.mu.Unlock()
		})
	}
	wg.Wait()
	if mode == linkModeBackup {
		linkElect()
		linkSequence()
		linkCatchUp()
		if p, ok := linkPrimary(); ok && p != nil {
			linkUndoDrift()
		}
	}
	if time.Since(lastClock) > linkClockEvery {
		lastClock = time.Now()
		for _, st := range peers {
			goSafe(func() { measureClock(st) })
		}
	}
	if mode == linkModeBackup && time.Since(lastMedia) > linkMediaEvery {
		lastMedia = time.Now()
		goSafe(refreshMediaDiff)
	}
}

// linkCatchUp (follower): pull when the primary holds another version.
func linkCatchUp() {
	link.mu.Lock()
	st := link.peers[link.primary]
	behind := st != nil && time.Since(st.seen) < linkDeadAfter &&
		(st.hello.LastTerm != link.lastTerm || st.hello.LastSeq != link.lastSeq)
	link.mu.Unlock()
	if behind {
		linkPull(st.LinkPeer)
	}
}

// rankBefore orders candidates for primary: the newest show version first,
// then the lowest stable id.
func rankBefore(t, s uint64, id string, t0, s0 uint64, id0 string) bool {
	if t != t0 || s != s0 {
		return newer(t, s, t0, s0)
	}
	return id < id0
}

// linkElect keeps one primary per set of units that can reach each other
// (DESIGN §6.8c): follow the best primary claim seen; when the primary is
// lost, refuse edits for linkSettle, then the best-ranked reachable unit
// takes over with a new term.
func linkElect() {
	link.mu.Lock()
	defer link.mu.Unlock()
	now := time.Now()
	var alive []*peerState
	var claim *peerState
	for _, st := range link.peers {
		if now.Sub(st.seen) > linkDeadAfter || st.hello.Mode != linkModeBackup {
			continue
		}
		alive = append(alive, st)
		if st.hello.Primary == st.ID && (claim == nil || st.hello.Term > claim.hello.Term ||
			(st.hello.Term == claim.hello.Term && st.ID < claim.ID)) {
			claim = st
		}
	}
	if link.primary == link.self {
		if claim != nil && (claim.hello.Term > link.term || (claim.hello.Term == link.term && claim.ID < link.self)) {
			logs.PrintfWarn(logs.LNKInfo, "linked units: %s is primary (term %d); this unit follows", claim.Name, claim.hello.Term)
			link.primary, link.term = claim.ID, claim.hello.Term
		}
		return
	}
	if claim != nil && claim.hello.Term >= link.term {
		if link.primary != claim.ID {
			logs.Printf(logs.LNKInfo, "linked units: following primary %s (term %d)", claim.Name, claim.hello.Term)
		}
		link.primary, link.term, link.lostAt = claim.ID, claim.hello.Term, time.Time{}
		return
	}
	if link.primary != "" {
		logs.PrintfWarn(logs.LNKWarn, "linked units: primary lost; edits refused until a unit takes over")
		link.primary, link.lostAt = "", now
	}
	if link.lostAt.IsZero() {
		link.lostAt = now
	}
	if now.Sub(link.lostAt) < linkSettle {
		return
	}
	bt, bs, bid := link.lastTerm, link.lastSeq, link.self
	term := link.term
	for _, st := range alive {
		h := st.hello
		if rankBefore(h.LastTerm, h.LastSeq, h.ID, bt, bs, bid) {
			bt, bs, bid = h.LastTerm, h.LastSeq, h.ID
		}
		term = max(term, h.Term)
	}
	if bid != link.self {
		return // the better candidate takes over
	}
	link.term, link.primary, link.lostAt = term+1, link.self, time.Time{}
	logs.Printf(logs.LNKInfo, "linked units: this unit is primary (term %d)", link.term)
	goSafe(func() {
		saveLinkVersion()
		seqMu.Lock()
		seqSnap, seqVer = nil, 0 // the next sequence pass starts from the show held here
		seqMu.Unlock()
	})
}

// ---- clocks ----

// measureClock estimates peer's clock offset NTP-style: of 5 exchanges, the
// one with the shortest round trip; offset = peer time - midpoint.
func measureClock(st *peerState) {
	best := -1.0
	var off float64
	for range 5 {
		var r struct {
			T int64 `json:"t"`
		}
		t0 := time.Now()
		if err := peerJSON(st.LinkPeer, helloClient, http.MethodGet, "/api/peer/time", nil, &r); err != nil {
			return
		}
		t3 := time.Now()
		rtt := float64(t3.Sub(t0)) / 1e6
		if best < 0 || rtt < best {
			best = rtt
			mid := t0.UnixNano() + t3.Sub(t0).Nanoseconds()/2
			off = float64(r.T-mid) / 1e6
		}
	}
	link.mu.Lock()
	st.offsetMs, st.rttMs, st.clockAt = off, best, time.Now()
	link.mu.Unlock()
	if abs(off) > clockWarnMs {
		logs.PrintfWarn(logs.LNKWarn, "linked unit %s: clock offset %.1f ms (round trip %.1f ms): sync the clocks (DESIGN §6.8c)", st.Name, off, best)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// ---- GO ----

// linkMirrorGo mirrors a GO made here (not one from a link) and returns the
// shared instant to start at (zero: nothing linked). Synced playback: every
// unit fires its own selected cue. Backup (primary): every unit fires the
// cue at pos (or plays group gid).
func linkMirrorGo(pos, gid int) time.Time {
	link.mu.Lock()
	mode, primary := link.mode, link.primary == link.self
	n := len(link.peers)
	link.mu.Unlock()
	if n == 0 || mode == linkModeOff || (mode == linkModeBackup && !primary) {
		return time.Time{}
	}
	at := time.Now().Add(linkLead())
	ms := strconv.FormatInt(at.UnixMilli(), 10)
	switch {
	case mode == linkModeSynced:
		enqueue(nil, http.MethodPost, "/api/cue/selected/play?linked=1&at="+ms, "", nil)
	case gid > 0:
		enqueue(nil, http.MethodPost, "/api/group/"+strconv.Itoa(gid)+"/play?linked=1&at="+ms, "", nil)
	default:
		enqueue(nil, http.MethodPost, "/api/cue/"+strconv.Itoa(pos)+"/play?linked=1&at="+ms, "", nil)
	}
	return at
}

// linkForwardGo sends a GO made here without HTTP (HyperDeck, OSC) to the
// primary when this unit follows one; true when it did.
func linkForwardGo() bool {
	if linkMode() != linkModeBackup {
		return false
	}
	primary, ok := linkPrimary()
	if !ok || primary == nil {
		return false // primary here, or unclear: fire here
	}
	goSafe(func() {
		if err := peerJSON(*primary, peerClient, http.MethodPost, "/api/cue/selected/play", nil, nil); err != nil {
			logs.PrintfWarn(logs.LNKWarn, "GO via the primary %s: %v", primary.Name, err)
		}
	})
	return true
}

// ---- pairing ----

// linkBaseURL normalises a typed address to a base URL.
func linkBaseURL(addr string) (string, error) {
	addr = strings.TrimRight(strings.TrimSpace(addr), "/")
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("use an address like 192.168.1.20, cutepi-2.local or http://10.77.0.55:3001")
	}
	return u.String(), nil
}

func validLinkMode(m string) bool {
	return m == linkModeSynced || m == linkModeBackup
}

// becomePrimaryForPairing: the unit that asks for Active/active backup keeps
// its show, so it is primary with a new term; the accepting unit's show is
// replaced.
func becomePrimaryForPairing() {
	link.mu.Lock()
	link.term++
	link.primary, link.lostAt = link.self, time.Time{}
	link.mu.Unlock()
	seqMu.Lock()
	seqSnap, seqVer = nil, 0
	seqMu.Unlock()
	linkSequence()
}

// followForPairing: the accepting unit takes the requester's show.
func followForPairing(primary string) {
	link.mu.Lock()
	link.primary, link.lostAt = primary, time.Time{}
	link.term, link.lastTerm, link.lastSeq = 0, 0, 0
	link.mu.Unlock()
	saveLinkVersion()
}

func registerLinkRoutes(rg *gin.RouterGroup) {
	// --- operator side (Settings > Network > Linked units) ---
	rg.GET("/link/section", func(c *gin.Context) { c.HTML(http.StatusOK, "linkSection.html", linkView()) })
	rg.GET("/link/banner", func(c *gin.Context) { c.HTML(http.StatusOK, "linkBanner.html", linkView()) })
	rg.POST("/link/mode", func(c *gin.Context) {
		mode := c.PostForm("mode")
		if mode != linkModeOff && !validLinkMode(mode) {
			respondError(c, http.StatusBadRequest, "unknown mode")
			return
		}
		was := linkMode()
		setLinkMode(mode)
		b, _ := json.Marshal(gin.H{"mode": mode})
		enqueue(nil, http.MethodPost, "/api/peer/mode", "application/json", b)
		if mode == linkModeBackup && was != linkModeBackup {
			becomePrimaryForPairing() // this unit's show is the one kept
		}
		c.HTML(http.StatusOK, "linkSection.html", linkView())
	})
	rg.POST("/link/pair", func(c *gin.Context) {
		base, err := linkBaseURL(c.PostForm("addr"))
		mode := linkMode()
		if err == nil && !validLinkMode(mode) {
			err = errors.New("choose Synced playback or Active/active backup first")
		}
		if err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		nonce := randomHex(16)
		link.mu.Lock()
		link.outgoing[nonce] = base
		link.mu.Unlock()
		body, _ := json.Marshal(gin.H{"id": selfID(), "name": InstanceName(), "port": config.Port(), "mode": mode, "nonce": nonce})
		req, _ := http.NewRequest(http.MethodPost, base+"/api/peer/pair/request", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if pw := c.PostForm("password"); pw != "" {
			req.SetBasicAuth("cutepi", pw)
		}
		resp, err := peerClient.Do(req)
		note := "Asked " + base + ": accept the link in that unit's Settings › Network."
		if err != nil {
			note = "Could not reach " + base + ": " + redactURLErr(err, base)
		} else {
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusAccepted:
			case http.StatusUnauthorized:
				note = base + " is password-protected: type its password."
			default:
				note = fmt.Sprintf("%s refused the request (HTTP %d).", base, resp.StatusCode)
			}
		}
		v := linkView()
		v.Note = note
		c.HTML(http.StatusOK, "linkSection.html", v)
	})
	rg.POST("/link/accept", func(c *gin.Context) {
		v := linkView()
		v.Note = acceptPair(c.PostForm("id"))
		if v.Note == "" {
			v = linkView()
		}
		c.HTML(http.StatusOK, "linkSection.html", v)
	})
	rg.POST("/link/decline", func(c *gin.Context) {
		link.mu.Lock()
		delete(link.incoming, c.PostForm("id"))
		link.mu.Unlock()
		c.HTML(http.StatusOK, "linkSection.html", linkView())
	})
	rg.POST("/link/unpair", func(c *gin.Context) {
		id := c.PostForm("id")
		link.mu.Lock()
		st := link.peers[id]
		link.mu.Unlock()
		if st != nil {
			_ = peerJSON(st.LinkPeer, peerClient, http.MethodPost, "/api/peer/unpair", nil, nil)
		}
		if err := ctp.DeleteLinkPeer(id); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		reloadPeers()
		c.HTML(http.StatusOK, "linkSection.html", linkView())
	})
	rg.POST("/link/syncmedia", func(c *gin.Context) {
		goSafe(syncMedia)
		v := linkView()
		v.Note = "Media sync started."
		c.HTML(http.StatusOK, "linkSection.html", v)
	})
	// The failover page's peers: their address and a token bound to the
	// page's origin (phase 2). Each peer is told the origin.
	rg.GET("/link/page", func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			scheme := "http"
			if c.Request.TLS != nil {
				scheme = "https"
			}
			origin = scheme + "://" + c.Request.Host
		}
		exp := time.Now().Add(pageTokenLife).Unix()
		type pagePeer struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		out := struct {
			Mode  string     `json:"mode"`
			Self  string     `json:"self"`
			Peers []pagePeer `json:"peers"`
		}{Mode: linkMode(), Self: selfID(), Peers: []pagePeer{}}
		if out.Mode == linkModeBackup {
			link.mu.Lock()
			for _, st := range link.peers {
				if st.Secret != "" {
					out.Peers = append(out.Peers, pagePeer{st.ID, st.Name, st.URL, link.self + "." + strconv.FormatInt(exp, 10) + "." + pageMAC(st.Secret, origin, exp)})
				}
			}
			link.mu.Unlock()
			b, _ := json.Marshal(gin.H{"origin": origin})
			enqueue(nil, http.MethodPost, "/api/peer/origin", "application/json", b)
		}
		c.JSON(http.StatusOK, out)
	})

	// --- peer side ---
	// Asking to pair (the operator password applies, as on any route).
	rg.POST("/peer/pair/request", func(c *gin.Context) {
		var r struct {
			ID, Name, Mode, Nonce string
			Port                  int
		}
		if err := c.ShouldBindJSON(&r); err != nil || r.ID == "" || r.Nonce == "" || !validLinkMode(r.Mode) || r.Port < 1 || r.Port > 65535 {
			respondError(c, http.StatusBadRequest, "invalid pairing request")
			return
		}
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid peer address")
			return
		}
		u := "http://" + net.JoinHostPort(host, strconv.Itoa(r.Port))
		if r.Port == 80 {
			u = "http://" + host
			if strings.Contains(host, ":") {
				u = "http://[" + host + "]"
			}
		}
		link.mu.Lock()
		if len(link.incoming) >= 8 {
			link.mu.Unlock()
			respondError(c, http.StatusTooManyRequests, "too many pending link requests")
			return
		}
		link.incoming[r.ID] = pairRequest{ID: r.ID, Name: r.Name, URL: u, Mode: r.Mode, Nonce: r.Nonce, at: time.Now()}
		link.mu.Unlock()
		logs.Printf(logs.LNKInfo, "link request from %s at %s (%s)", r.Name, u, r.Mode)
		c.Status(http.StatusAccepted)
	})
	// The other unit accepted: the pairing nonce proves it is the one asked.
	rg.POST("/peer/pair/accept", func(c *gin.Context) {
		var r struct{ ID, Name, Nonce, Secret string }
		if err := c.ShouldBindJSON(&r); err != nil || r.ID == "" || len(r.Secret) < 32 {
			respondError(c, http.StatusBadRequest, "invalid pairing")
			return
		}
		link.mu.Lock()
		base, ok := link.outgoing[r.Nonce]
		delete(link.outgoing, r.Nonce)
		link.mu.Unlock()
		if !ok || r.Nonce == "" {
			respondError(c, http.StatusForbidden, "no such link request")
			return
		}
		if err := ctp.SaveLinkPeer(ctp.LinkPeer{ID: r.ID, Name: r.Name, URL: base, Secret: r.Secret}); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		reloadPeers()
		if linkMode() == linkModeBackup {
			becomePrimaryForPairing()
		}
		logs.Printf(logs.LNKInfo, "linked with %s at %s", r.Name, base)
		c.Status(http.StatusNoContent)
	})

	peer := rg.Group("/peer", func(c *gin.Context) {
		if c.GetString(ctxLinkPeer) == "" {
			c.AbortWithStatus(http.StatusForbidden)
		}
	})
	peer.GET("/hello", func(c *gin.Context) { c.JSON(http.StatusOK, selfHello()) })
	peer.GET("/time", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"t": time.Now().UnixNano()}) })
	peer.GET("/show", func(c *gin.Context) {
		linkSequence()
		seqMu.Lock()
		snap := seqSnap
		seqMu.Unlock()
		link.mu.Lock()
		m := showMsg{Term: link.lastTerm, Seq: link.lastSeq, Show: snap}
		primary := link.primary == link.self
		link.mu.Unlock()
		if !primary || snap == nil {
			respondError(c, http.StatusConflict, "not the primary")
			return
		}
		c.JSON(http.StatusOK, m)
	})
	peer.POST("/show", func(c *gin.Context) {
		var m showMsg
		if err := c.ShouldBindJSON(&m); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := applyShow(c.GetString(ctxLinkPeer), m); err != nil {
			logs.PrintfWarn(logs.LNKWarn, "applying the show: %v", err)
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	peer.POST("/mode", func(c *gin.Context) {
		var r struct{ Mode string }
		if err := c.ShouldBindJSON(&r); err != nil || (r.Mode != linkModeOff && !validLinkMode(r.Mode)) {
			respondError(c, http.StatusBadRequest, "unknown mode")
			return
		}
		if r.Mode != linkMode() {
			setLinkMode(r.Mode)
			if r.Mode == linkModeBackup {
				followForPairing(c.GetString(ctxLinkPeer))
			}
		}
		c.Status(http.StatusNoContent)
	})
	peer.POST("/unpair", func(c *gin.Context) {
		if err := ctp.DeleteLinkPeer(c.GetString(ctxLinkPeer)); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		reloadPeers()
		c.Status(http.StatusNoContent)
	})
	peer.POST("/origin", func(c *gin.Context) {
		var r struct{ Origin string }
		if c.ShouldBindJSON(&r) != nil {
			respondError(c, http.StatusBadRequest, "invalid origin")
			return
		}
		if u, err := url.Parse(r.Origin); err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") {
			respondError(c, http.StatusBadRequest, "invalid origin")
			return
		}
		link.mu.Lock()
		if len(link.origins) < 64 {
			link.origins[strings.ToLower(r.Origin)] = c.GetString(ctxLinkPeer)
		}
		link.mu.Unlock()
		c.Status(http.StatusNoContent)
	})
	peer.GET("/media", func(c *gin.Context) {
		list, err := mediaList()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, list)
	})
	peer.POST("/pushmedia", func(c *gin.Context) {
		var r struct{ Names []string }
		if err := c.ShouldBindJSON(&r); err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		id := c.GetString(ctxLinkPeer)
		goSafe(func() { pushFiles(id, r.Names) })
		c.Status(http.StatusAccepted)
	})
}

// acceptPair accepts the pending request from id: a new pairing secret, sent
// to the requester (which proves itself with its nonce). "" on success.
func acceptPair(id string) string {
	link.mu.Lock()
	r, ok := link.incoming[id]
	delete(link.incoming, id)
	link.mu.Unlock()
	if !ok {
		return "That link request has gone."
	}
	secret := randomHex(32)
	p := ctp.LinkPeer{ID: r.ID, Name: r.Name, URL: r.URL}
	body := gin.H{"id": selfID(), "name": InstanceName(), "nonce": r.Nonce, "secret": secret}
	if err := peerJSON(p, peerClient, http.MethodPost, "/api/peer/pair/accept", body, nil); err != nil {
		return "Could not complete the link with " + r.Name + ": " + err.Error()
	}
	p.Secret = secret
	if err := ctp.SaveLinkPeer(p); err != nil {
		return err.Error()
	}
	reloadPeers()
	if linkMode() != r.Mode {
		setLinkMode(r.Mode)
	}
	if r.Mode == linkModeBackup {
		followForPairing(r.ID)
	}
	logs.Printf(logs.LNKInfo, "linked with %s at %s (%s)", r.Name, r.URL, r.Mode)
	return ""
}

func selfHello() peerHello {
	link.mu.Lock()
	defer link.mu.Unlock()
	return peerHello{ID: link.self, Name: InstanceName(), Mode: link.mode, Primary: link.primary,
		Term: link.term, LastTerm: link.lastTerm, LastSeq: link.lastSeq, PrepMs: prepMs()}
}

// ---- the view (Settings section, banner) ----

type linkPeerView struct {
	ID, Name, URL, Status, Clock  string
	Up, Primary, Legacy, ClockBad bool
	Media                         mediaDiff
}

type linkViewData struct {
	Mode, Self, Name, Note, Role, SyncNote string
	LeadMs                                 int // a linked fire's shared instant, ms after the press
	Peers                                  []linkPeerView
	Incoming                               []pairRequest
	Found                                  []foundUnit
	Down, MediaBad, ClockBad, Electing     bool
}

func linkView() linkViewData {
	lead := linkLead()
	link.mu.Lock()
	defer link.mu.Unlock()
	v := linkViewData{Mode: link.mode, Self: link.self, Name: InstanceName(), SyncNote: link.syncNote}
	if link.mode == linkModeBackup {
		switch link.primary {
		case "":
			v.Role, v.Electing = "choosing a primary: edits are refused until one takes over", true
		case link.self:
			v.Role = "primary"
		default:
			v.Role = "backup (follows the primary)"
		}
	}
	paired := map[string]bool{}
	for _, st := range link.peers {
		paired[st.ID] = true
		pv := linkPeerView{ID: st.ID, Name: st.Name, URL: config.RedactURL(st.URL), Legacy: st.Secret == "",
			Up: time.Since(st.seen) < linkDeadAfter, Primary: link.mode == linkModeBackup && st.ID == link.primary, Media: st.media}
		switch {
		case pv.Legacy:
			pv.Status = "linked by address (no pairing): link it again to see its state"
		case pv.Up:
			pv.Status = "connected"
		case st.err != "":
			pv.Status = "not answering: " + st.err
		default:
			pv.Status = "not answering"
		}
		if !st.clockAt.IsZero() {
			pv.Clock = fmt.Sprintf("%+.1f ms (round trip %.1f ms)", st.offsetMs, st.rttMs)
			pv.ClockBad = abs(st.offsetMs) > clockWarnMs
		}
		if !pv.Up && !pv.Legacy {
			v.Down = true
		}
		v.MediaBad = v.MediaBad || st.media.Bad()
		v.ClockBad = v.ClockBad || pv.ClockBad
		v.Peers = append(v.Peers, pv)
	}
	sort.Slice(v.Peers, func(i, j int) bool { return v.Peers[i].Name+v.Peers[i].ID < v.Peers[j].Name+v.Peers[j].ID })
	for _, r := range link.incoming {
		if time.Since(r.at) < 10*time.Minute {
			v.Incoming = append(v.Incoming, r)
		}
	}
	for _, f := range discoveredUnits() {
		if !paired[f.ID] {
			v.Found = append(v.Found, f)
		}
	}
	if link.mode == linkModeOff {
		v.Down, v.MediaBad, v.ClockBad, v.Electing = false, false, false, false
	}
	if len(v.Peers) > 0 && v.Mode != linkModeOff {
		v.LeadMs = int(lead / time.Millisecond)
	}
	return v
}
