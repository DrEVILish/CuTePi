package routes

import (
	"encoding/binary"
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
		{"record", "104 record failed"},
		{"record spill", "104 record failed"},
		{"transport info", "208 transport info:\r\nstatus: stopped"},
		{"clips count", "210 clips count: 0"},
		{"goto: clip id: 9", "105 load failure: no such clip id: 9"},
		{"goto: timecode: 00:00:00:00", "200 ok"},
		{"goto: timecode: 1:00", "100 syntax error"},
		{"gibberish", "100 unknown command"},
		{"", "100"},
	}
	for _, c := range cases {
		got, quit := handleDeckLine(c.line)
		if quit {
			t.Fatalf("%q did not expect quit", c.line)
		}
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("deck %q -> %q, want prefix %q", c.line, got, c.want)
		}
	}
	if got, quit := handleDeckLine("quit"); !quit || !strings.HasPrefix(got, "200 ok") {
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
	if _, quit := handleDeckLine("goto: clip id: 1"); quit {
		t.Fatal("goto closed the session")
	}
	if pos, err := ctp.SelectedCuePos(); err != nil || pos != 1 {
		t.Fatalf("goto clip id 1: selected pos=%v err=%v", pos, err)
	}
	if "none" != currentClipID() {
		t.Errorf("goto must not start playback (clip id=%q)", currentClipID())
	}
	// Clip id outside the sheet is a protocol-legal load failure.
	if got, _ := handleDeckLine("play: clip id: 7"); !strings.HasPrefix(got, "105 load failure") {
		t.Errorf("play clip 7 -> %q", got)
	}
	// Negative speed (rewind) is refused before it can mangle the rate.
	if got, _ := handleDeckLine("play: speed: -100"); !strings.HasPrefix(got, "115 speed failure") {
		t.Errorf("play speed -100 -> %q", got)
	}
	// Cue list: clip id = row position.
	if got, _ := handleDeckLine("clips get"); !strings.Contains(got, "clip count: 1\r\n") ||
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
func TestDeckGreeting(t *testing.T) {
	addr := freeListenAddr(t)
	go ListenHyperdeck(addr)
	time.Sleep(50 * time.Millisecond)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _ := conn.Read(buf)
	greet := string(buf[:n])
	if !strings.Contains(greet, "500 connection info:") || !strings.Contains(greet, "model: CuTePi") {
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
