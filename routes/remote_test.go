package routes

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// deck tests run against the same in-memory DB the HTTP tests use: the
// remote adapters share the transport cores with the HTTP handlers, so the
// checks are about wiring, framing and protocol syntax rather than the
// pipeline itself.

// deck tests run against the same in-memory DB the HTTP tests use.
func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	config.SetDbLocation(":memory:")
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestDeckLines(t *testing.T) {
	setupTestDB(t)
	cases := []struct{ line, want string }{
		{"ping", "200 ok"},
		{"record", "103 unsupported"},
		{"record spill", "103 unsupported"},
		{"transport info", "208 transport info:\r\nstatus: stopped"},
		{"clips count", "214 clips count:\r\nclip count: 0\r\n"},
		{"goto: clip id: 9", "109 out of range"},
		{"goto: timecode: 00:00:00:00", "200 ok"},
		{"goto: timecode: 1:00", "102 invalid value"},
		{"gibberish", "100 syntax error"},
		{"device info", "204 device info:\r\nprotocol version: 1.11\r\nmodel: HyperDeck Studio Mini\r\n"},
		{"remote", "210 remote info:\r\nenabled: true\r\n"},
		{"configuration", "211 configuration:\r\nvideo input: SDI\r\n"},
		{"slot info: slot id: 1", "202 slot info:\r\nslot id: 1\r\nstatus: mounted\r\n"},
		{"slot info: slot id: 2", "202 slot info:\r\nslot id: 2\r\nstatus: empty\r\n"},
		{"slot info: slot id: 3", "109 out of range"},
		{"watchdog: period: 6", "200 ok"},
		{"notify: transport: maybe", "102 invalid value"},
		{"playrange", "219 playrange info:"},
		{"format: prepare: exFAT", "103 unsupported"},
		{"", "100"},
	}
	for _, c := range cases {
		got, quit := handleDeckLine(nil, c.line)
		if quit {
			t.Fatalf("%q did not expect quit", c.line)
		}
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("deck %q -> %q, want prefix %q", c.line, got, c.want)
		}
	}
	if got, quit := handleDeckLine(nil, "quit"); !quit || !strings.HasPrefix(got, "200 ok") {
		t.Errorf("quit -> %q quit=%v", got, quit)
	}
}

func TestDeckPlayAndGoto(t *testing.T) {
	if err := ctp.RegisterMedia("deck-test.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10000, Resolution: "1920x1080", Codec: "h264",
	}, "Deck Test"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("deck-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	// "goto" is playhead-positioning: it must select, never start.
	if _, quit := handleDeckLine(nil, "goto: clip id: 1"); quit {
		t.Fatal("goto closed the session")
	}
	if pos, err := ctp.SelectedCuePos(); err != nil || pos != 1 {
		t.Fatalf("goto clip id 1: selected pos=%v err=%v", pos, err)
	}
	if "none" != currentClipID() {
		t.Errorf("goto must not start playback (clip id=%q)", currentClipID())
	}
	// Clip id outside the sheet is a protocol-legal load failure.
	if got, _ := handleDeckLine(nil, "play: clip id: 7"); !strings.HasPrefix(got, "109 out of range") {
		t.Errorf("play clip 7 -> %q", got)
	}
	// Negative speed (rewind) is refused before it can mangle the rate.
	if got, _ := handleDeckLine(nil, "play: speed: -100"); !strings.HasPrefix(got, "103 unsupported") {
		t.Errorf("play speed -100 -> %q", got)
	}
	// Cue list: clip id = row position.
	if got, _ := handleDeckLine(nil, "clips get"); !strings.Contains(got, "clip count: 1\r\n") ||
		!strings.Contains(got, "1: deck-test.mp4") {
		t.Errorf("clips get -> %q", got)
	}
}

func TestDeckParseLine(t *testing.T) {
	cmd, p := parseDeckLine("play: clip id: 5 timecode: +00:00:02:00 speed: 150")
	if cmd != "play" {
		t.Errorf("cmd = %q", cmd)
	}
	if p["clip id"] != "5" || p["timecode"] != "+00:00:02:00" || p["speed"] != "150" {
		t.Errorf("params = %v", p)
	}
	cmd, _ = parseDeckLine("play")
	if cmd != "play" {
		t.Errorf("bare cmd = %q", cmd)
	}
}

func TestDeckTimecode(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "00:00:00:00"},
		{12.5, "00:00:12:12"},
		{3661.5, "01:01:01:12"},
		{-3, "00:00:00:00"},
	}
	for _, c := range cases {
		if got := timecode(c.sec); got != c.want {
			t.Errorf("timecode(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
	if got, _ := parseTimecode("01:01:01:12"); math.Abs(got-3661.48) > 0.01 {
		t.Errorf("parseTimecode round-trip = %v", got)
	}
	if _, err := parseTimecode("nope"); err == nil {
		t.Error("garbage timecode must fail, not truncate")
	}
}

// --- OSC ----------------------------------------------------------------

// oscEncode hand-builds the wire form: addr, tags, args, 4-byte padded.
func oscEncode(t *testing.T, addr string, args ...any) []byte {
	t.Helper()
	var tags []byte
	var body []byte
	tags = append(tags, ',')
	for _, a := range args {
		switch v := a.(type) {
		case int:
			tags = append(tags, 'i')
			body = binary.BigEndian.AppendUint32(body, uint32(v))
		case string:
			tags = append(tags, 's')
			body = append(body, nullPadded(v)...)
		}
	}
	return append(append(nullPadded(addr), nullPadded(string(tags))...), body...)
}

func nullPadded(s string) []byte {
	b := append([]byte(s), 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func TestOSCDecodeAndRoute(t *testing.T) {
	if err := ctp.RegisterMedia("osc-test.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10000, Resolution: "1920x1080", Codec: "h264",
	}, "OSC Test"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("osc-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	// /cue/{num}/start by human cue number and by row position both fire the
	// right cue (selection untouched).
	if err := ctp.SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	command("/cue/1/start", nil)
	command("/workspace/my show.qlab5/cue/1/start", nil) // workspace id is irrelevant

	// Workspace-scoped spellings of transport verbs resolve like rootless.
	for _, path := range []string{"/go", "/workspace/my show.qlab5/go", "/workspace/anything/next"} {
		// Safe no-ops on an empty transport; routing must not error loudly.
		command(path, nil)
	}
	// Decode round-trip (int arg).
	addr, args, err := oscDecode(oscEncode(t, "/go", 2))
	if err != nil || addr != "/go" || len(args) != 1 || args[0] != 2 {
		t.Fatalf("decode = %q %v err=%v", addr, args, err)
	}
	if _, args, err := oscDecode(oscEncode(t, "/cue/2/panic", "3.1")); err != nil || len(args) != 1 || args[0] != "3.1" {
		t.Fatalf("cue decode = %v err=%v", args, err)
	}
}

// The remote-control factory: TestDeckGreeting dials the real listener to
// verify the greet frame arrives before any command (controllers wait for it).
func TestDeckClipOffsets(t *testing.T) {
	setupTestDB(t)
	for _, name := range []string{"off-a.mp4", "off-b.mp4", "off-c.mp4"} {
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, name); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if err := ctp.SetCue("2"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	if id, err := clipOffset("+1"); err != nil || id != 3 {
		t.Fatalf("clipOffset +1 = %v err=%v", id, err)
	}
	if id, err := clipOffset("-1"); err != nil || id != 1 {
		t.Fatalf("clipOffset -1 = %v err=%v", id, err)
	}
	if id, _ := clipOffset("+9"); id != 0 {
		t.Errorf("overshoot must yield 0, got %d", id)
	}
	// Deck windowing: one clip from clip id 2.
	win, err := clipListResponse("2", "1", 1)
	if err != nil || !strings.Contains(win, "clip count: 1\r\n") || !strings.Contains(win, "2: off-b.mp4") {
		t.Errorf("clip window = %q err=%v", win, err)
	}
}

func TestDeckGreeting(t *testing.T) {
	addr := freeListenAddr(t)
	deck, err := startHyperdeck(addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	defer deck.Close()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _ := conn.Read(buf)
	greet := string(buf[:n])
	if !strings.Contains(greet, "500 connection info:") || !strings.Contains(greet, "model: HyperDeck Studio Mini") {
		t.Fatalf("greeting = %q", greet)
	}
}

// freeListenAddr grabs a free loopback TCP port for the test listener.
func freeListenAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	defer ln.Close()
	return "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func TestGroupTimingStats(t *testing.T) {
	setupTestDB(t)
	g := setupTrackedGroup(t)
	total, remaining := groupTiming(g.GroupID)
	if total == 0 {
		t.Fatal("group total duration must be non-zero from member trims")
	}
	// Not playing: no remaining-time claim.
	if remaining != 0 {
		t.Errorf("idle group must report 0 remaining, got %v", remaining)
	}
}

// setupTrackedGroup makes group + two media-backed members and returns the group.
func setupTrackedGroup(t *testing.T) ctp.Group {
	t.Helper()
	gid, err := ctp.CreateGroup("Timing", 0)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	for _, name := range []string{"timing-a.mp4", "timing-b.mp4"} {
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, name); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
	}
	for _, name := range []string{"timing-a.mp4", "timing-b.mp4"} {
		if err := ctp.AddCueToGroup(name, gid, false); err != nil {
			t.Fatalf("AddCueToGroup: %v", err)
		}
	}
	g, _ := ctp.GetGroup(gid)
	return g
}

// "goto: clip id" cues the playhead; the next bare "play" starts that clip
// (not a resume of whatever was loaded), and a later bare "play" is a plain
// resume again.
func TestDeckGotoThenPlayFiresCuedClip(t *testing.T) {
	if err := ctp.RegisterMedia("deck-goto.wav", 100, media.Metadata{
		Mimetype: "audio/wav", Duration: 1000, Codec: "pcm_s16le",
	}, "Deck Goto"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("deck-goto.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		cues, _ := ctp.GetCuesheet()
		pos = cues.Cues[len(cues.Cues)-1].CuePos
	}
	// Clip ids number the sheet in play order.
	clips, err := deckClipList()
	if err != nil {
		t.Fatalf("deckClipList: %v", err)
	}
	id := 0
	for _, c := range clips {
		if c.cuePos == pos {
			id = c.id
		}
	}
	if got, _ := handleDeckLine(nil, "goto: clip id: "+strconv.Itoa(id)); !strings.HasPrefix(got, "200") {
		t.Fatalf("goto -> %q", got)
	}
	if deckGotoClip.Load() != int32(id) {
		t.Fatalf("goto did not cue clip %d (pending=%d)", id, deckGotoClip.Load())
	}
	if sel, _ := ctp.SelectedCuePos(); sel != pos {
		t.Fatalf("goto clip %d selected cue %d, want %d", id, sel, pos)
	}
	handleDeckLine(nil, "stop")
	if deckGotoClip.Load() != 0 {
		t.Fatalf("stop left a pending goto")
	}
}

// deckReply reads one response the way hyperdeck-connection parses it: a
// header without a trailing colon is a single line; otherwise lines run to
// the blank line.
func deckReply(t *testing.T, rd *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		l, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read reply: %v (so far %q)", err, b.String())
		}
		if b.Len() == 0 && l == "\r\n" {
			continue
		}
		b.WriteString(l)
		if b.Len() == len(l) && !strings.HasSuffix(strings.TrimRight(l, "\r\n"), ":") {
			return b.String()
		}
		if l == "\r\n" {
			return b.String()
		}
	}
}

// The command sequence Companion's HyperDeck module (hyperdeck-connection)
// sends on connect, in its wire form: multi-line commands, the watchdog and
// ping, then the state reads. Every reply must carry the code the library
// waits for, or it drops the connection as failed.
func TestDeckCompanionHandshake(t *testing.T) {
	setupTestDB(t)
	for i := 0; i < 100; i++ { // earlier tests' clients hang up asynchronously
		deckRegMu.RLock()
		n := len(deckClients)
		deckRegMu.RUnlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	addr := freeListenAddr(t)
	deck, err := startHyperdeck(addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	defer deck.Close()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReader(conn)
	if got := deckReply(t, rd); !strings.HasPrefix(got, "500 connection info:\r\nprotocol version: 1.11\r\nmodel: HyperDeck Studio Mini\r\n") {
		t.Fatalf("greeting = %q", got)
	}
	steps := []struct{ send, want string }{
		{"watchdog:\r\nperiod: 6\r\n\r\n", "200 ok\r\n"},
		{"ping\r\n", "200 ok\r\n"},
		{"notify:\r\nremote: true\r\ntransport: true\r\nslot: true\r\nconfiguration: true\r\nplayrange: true\r\n\r\n", "200 ok\r\n"},
		{"device info\r\n", "204 device info:\r\n"},
		{"slot info:\r\nslot id: 1\r\n\r\n", "202 slot info:\r\nslot id: 1\r\nstatus: mounted\r\n"},
		{"slot info:\r\nslot id: 2\r\n\r\n", "202 slot info:\r\nslot id: 2\r\nstatus: empty\r\n"},
		{"transport info\r\n", "208 transport info:\r\nstatus: stopped\r\n"},
		{"configuration\r\n", "211 configuration:\r\n"},
		{"remote\r\n", "210 remote info:\r\nenabled: true\r\noverride: false\r\n"},
		{"clips get\r\n", "205 clips info:\r\nclip count: 0\r\n"},
		{"notify\r\n", "209 notify:\r\nremote: true\r\ntransport: true\r\nslot: true\r\nconfiguration: true\r\ndropped frames: false\r\n"},
		{"configuration:\r\nvideo input: SDI\r\n\r\n", "200 ok\r\n"}, // reply, then its notification
		{"", "511 configuration:\r\nvideo input: SDI\r\n"},
		{"remote:\r\nenable: false\r\n\r\n", "200 ok\r\n"},
		{"", "510 remote info:\r\nenabled: false\r\n"},
		{"stop\r\n", "111 remote control disabled\r\n"},
		{"remote: enable: true\r\n", "200 ok\r\n"},
		{"", "510 remote info:\r\nenabled: true\r\n"},
	}
	for _, s := range steps {
		if s.send != "" {
			if _, err := io.WriteString(conn, s.send); err != nil {
				t.Fatalf("send %q: %v", s.send, err)
			}
		}
		if got := deckReply(t, rd); !strings.HasPrefix(got, s.want) {
			t.Fatalf("%q -> %q, want prefix %q", s.send, got, s.want)
		}
	}
	// The watchdog closes a client that goes quiet past its period.
	io.WriteString(conn, "watchdog: period: 1\r\n")
	if got := deckReply(t, rd); got != "200 ok\r\n" {
		t.Fatalf("watchdog 1 -> %q", got)
	}
	start := time.Now()
	for { // notifications may still arrive; the watchdog ends the stream
		if _, err := rd.ReadString('\n'); err != nil {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("connection still open after the watchdog period")
		}
	}
	if el := time.Since(start); el < time.Second || el > 5*time.Second {
		t.Errorf("watchdog closed after %v, want period 1s + grace", el)
	}
}

// qlabRoundTrip sends one SLIP-framed OSC message and returns the next
// message from the server as (address, JSON envelope).
func qlabRead(t *testing.T, conn net.Conn, dec *slipDecoder, pending *[][]byte) (string, map[string]any) {
	t.Helper()
	buf := make([]byte, 65536)
	for len(*pending) == 0 {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		*pending = append(*pending, dec.feed(buf[:n])...)
	}
	pkt := (*pending)[0]
	*pending = (*pending)[1:]
	addr, args, err := oscDecode(pkt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	env := map[string]any{}
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			if err := json.Unmarshal([]byte(s), &env); err != nil {
				t.Fatalf("%s: bad JSON %q: %v", addr, s, err)
			}
		}
	}
	return addr, env
}

// The connect sequence of Companion's QLab module (figure53-qlab-advance)
// over TCP: every message gets QLab's reply envelope, the cue list carries
// the cue sheet with the keys the module reads, and /updates subscribers
// get playhead pushes.
func TestQLabCompanionHandshake(t *testing.T) {
	setupTestDB(t)
	for _, name := range []string{"ql-a.mp4", "ql-b.wav"} {
		mt := "video/mp4"
		if strings.HasSuffix(name, ".wav") {
			mt = "audio/wav"
		}
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: mt, Duration: 10}, name); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go (&oscTCPServer{ln: ln, conns: map[net.Conn]bool{}}).serve()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	dec := &slipDecoder{}
	var pending [][]byte
	ask := func(addr string, args ...any) map[string]any {
		t.Helper()
		if _, err := conn.Write(slipEncode(oscEncodeMsg(addr, args...))); err != nil {
			t.Fatalf("write: %v", err)
		}
		for {
			got, env := qlabRead(t, conn, dec, &pending)
			if strings.HasPrefix(got, "/update/") {
				continue // pushes may interleave
			}
			if got != "/reply"+addr || env["address"] != addr {
				t.Fatalf("%s -> %s %v", addr, got, env)
			}
			return env
		}
	}
	if env := ask("/version"); env["status"] != "ok" || env["data"] != qlabVersion {
		t.Fatalf("/version = %v", env)
	}
	ws := ask("/workspaces")["data"].([]any)[0].(map[string]any)
	if ws["uniqueID"] != qlabWorkspaceID() || ws["displayName"] == "" {
		t.Fatalf("/workspaces = %v", ws)
	}
	if env := ask("/connect"); env["data"] != "ok:view|edit|control" {
		t.Fatalf("/connect = %v", env)
	}
	if env := ask("/updates", 1); env["status"] != "ok" {
		t.Fatalf("/updates = %v", env)
	}
	lists := ask("/cueLists")["data"].([]any)
	list := lists[0].(map[string]any)
	cues := list["cues"].([]any)
	if list["type"] != "Cue List" || len(cues) != 2 {
		t.Fatalf("/cueLists = %v", list)
	}
	first := cues[0].(map[string]any)
	for _, k := range []string{"number", "uniqueID", "listName", "type", "mode", "isPaused", "duration",
		"actionElapsed", "preWait", "postWait", "parent", "flagged", "notes", "autoLoad", "colorName",
		"isRunning", "isAuditioning", "isLoaded", "armed", "isBroken", "continueMode",
		"percentActionElapsed", "cartPosition", "infiniteLoop", "holdLastFrame"} {
		if _, ok := first[k]; !ok {
			t.Errorf("cue lacks key %q", k)
		}
	}
	if first["type"] != "Video" || cues[1].(map[string]any)["type"] != "Audio" || first["duration"] != 10.0 {
		t.Errorf("cue types/duration = %v / %v", first, cues[1])
	}
	id := first["uniqueID"].(string)
	v := ask("/workspace/"+qlabWorkspaceID()+"/cue_id/"+id+"/valuesForKeys", `["number","uniqueID"]`)
	if v["data"].(map[string]any)["uniqueID"] != id {
		t.Fatalf("valuesForKeys = %v", v)
	}
	if env := ask("/cue/active/valuesForKeys"); env["status"] != "error" {
		t.Errorf("no active cue must answer error, got %v", env)
	}
	if env := ask("/no/such/thing"); env["status"] != "error" {
		t.Errorf("unknown address = %v", env)
	}
	for _, a := range []string{"/alwaysAudition", "/auditionMonitors", "/overrideWindow", "/showMode",
		"/liveFadePreview", "/settings/general/minGoTime", "/overrides/midiOutputEnabled", "/selectedCues"} {
		if env := ask(a); env["status"] != "ok" {
			t.Errorf("%s = %v", a, env)
		}
	}
	// Moving the playhead pushes playbackPosition to the subscriber.
	second := cues[1].(map[string]any)
	if ask("/playheadID")["data"] == second["uniqueID"] {
		second = first
	}
	time.Sleep(400 * time.Millisecond) // let the update loop see the current playhead
	if env := ask("/playheadID/" + second["uniqueID"].(string)); env["status"] != "ok" {
		t.Fatalf("set playhead = %v", env)
	}
	if env := ask("/playheadID"); env["data"] != second["uniqueID"] {
		t.Fatalf("/playheadID = %v", env)
	}
	want := "/update/workspace/" + qlabWorkspaceID() + "/cueList/" + qlabCueListID() + "/playbackPosition"
	for {
		pkt := func() []byte {
			buf := make([]byte, 65536)
			for len(pending) == 0 {
				n, err := conn.Read(buf)
				if err != nil {
					t.Fatalf("waiting for %s: %v", want, err)
				}
				pending = append(pending, dec.feed(buf[:n])...)
			}
			p := pending[0]
			pending = pending[1:]
			return p
		}()
		addr, args, _ := oscDecode(pkt)
		if addr == want {
			if len(args) != 1 || args[0] != second["uniqueID"] {
				t.Fatalf("playbackPosition args = %v", args)
			}
			break
		}
	}
}
