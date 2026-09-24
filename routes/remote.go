package routes

// Remote control: the same transport the Web UI drives, over two wire
// protocols show-control networks use to run a deck.
//
//   - HyperDeck Ethernet Protocol (TCP 9993): Blackmagic's line protocol,
//     spoken by Bitfocus's HyperDeck module, CM controllers and show-control
//     environments. Framing follows the real decks: a 500 greeting on
//     connect, "{code} {name}:" responses carrying "{param}: {value}" lines.
//     Cue positions (1..) are the deck's "clip ids"; the cue sheet is the
//     deck's clip list.
//   - QLab OSC (UDP 53000): the address paths Companion's QLab module sends.
//     The socket half lives in qlab.go; both halves call these cores.
//
// CuTePi is a playback device, so "record" answers 104 (no recorder), and
// there is no rewind: negative play speeds are refused, while "goto"
// positions the playhead (0 = top of the clip) like a deck's goto does.
//
// Both listeners share the HTTP API's trust boundary: anyone who can reach
// the web UI can already drive the same transport, so the extra listeners
// add no new access path. A bind failure is logged and skipped — losing a
// control listener must never stop the show controller from starting.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// MediaFPS is the clock roll rate used for HyperDeck "display timecode"
// (real decks roll at the project frame rate; CuTePi reports 25).
const MediaFPS = 25

// FireSelected is the GO action from /cue/selected/play, renderless: fire
// the selected group's playlist (or cue), advance the selection when
// goAdvance is on. HTTP handlers wrap the result in cuesheet HTML; remote
// protocols have no session to re-render — clients pick the change up from
// WS sync / the cuesheet poller.
func FireSelected() error {
	if gid, gerr := ctp.SelectedGroupPos(); gerr == nil && gid > 0 {
		if g, err := ctp.GetGroup(gid); err == nil {
			playGroup(g)
			if ctp.GetGoAdvance() {
				_ = ctp.SelectStep(1)
			}
			return nil
		}
	}
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		gsp.Play() // nothing selected: resume the loaded transport
		return nil
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		gsp.Play()
		return nil
	}
	if err := loadAndPlayCue(cue); err != nil {
		return err
	}
	// Deck-style preload: after the advance, the newly-selected cue (audio-
	// only) sits prerolled, so the NEXT GO lands instantly instead of paying
	// a build+preroll.
	if ctp.GetGoAdvance() {
		_ = ctp.SelectStep(1)
	}
	if pos, perr := ctp.SelectedCuePos(); perr == nil && pos != 0 {
		armNextCue(gsp.Generation(), pos)
	}
	return nil
}

// FireCue plays cue cuePos without touching the selection, mirroring
// /cue/:cuePos/play including its fade-out-then-start behaviour.
func FireCue(cuePos string) error {
	cue, err := ctp.GetCue(cuePos)
	if err != nil {
		return err
	}
	if gsp.CurrentPlaying() != "" && cue.FadeOut > 0 {
		goSafe(func() {
			gsp.FadeAndStop(cue.FadeOut)
			// Identity guard: something else started meanwhile.
			if cur := gsp.CurrentCuePos(); cur != 0 && cur != cue.CuePos {
				return
			}
			if err := loadAndPlayCue(cue); err != nil {
				logs.Printf(logs.RTECuePlay, "queued play failed pos=%d error=%v", cue.CuePos, err)
			}
		})
		return nil
	}
	return loadAndPlayCue(cue)
}

// remotePanic is the PANIC action: panic-hold image when configured, dead
// black otherwise. Shared by the HTTP /panic handler and both protocols
// (QLab's /panic is the same gesture).
func remotePanic() error {
	if hold := ctp.GetPanicHoldImage(); hold != "" {
		if err := gsp.LoadWithOpts(hold, gsp.LoadOpts{Hold: true}); err == nil {
			return nil
		}
		logs.Printf(logs.RTEPanic, "panic hold image %q unavailable - cutting to black", hold)
	}
	gsp.Panic()
	return nil
}

// StartRemote launches both protocol listeners.
func StartRemote() {
	go ListenHyperdeck(":9993")
	go ListenOSC(":53000")
}

// cueByNum resolves a HyperDeck "clip id" / OSC cue reference: the row
// position (CuePos) exactly as it stands in the sheet, falling back to the
// show's human cue number (CueNum, e.g. "3.1").
func cueByNum(ref string) (ctp.Cue, bool) {
	cues, err := ctp.GetCuesheet()
	if err != nil {
		return ctp.Cue{}, false
	}
	if pos, e := strconv.Atoi(strings.TrimSpace(ref)); e == nil {
		for _, c := range cues.Cues {
			if c.CuePos == pos {
				return c, true
			}
		}
	}
	for _, c := range cues.Cues {
		if c.CueNum == ref {
			return c, true
		}
	}
	return ctp.Cue{}, false
}

// --- HyperDeck (TCP) --------------------------------------------------------

// ListenHyperdeck runs the HyperDeck TCP listener (exported for the test
// harness; StartRemote launches it). Bind failures log and return.
func ListenHyperdeck(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logs.Printf(logs.RTEDeckErr, "hyperdeck listener: %v", err)
		return
	}
	go deckNotifyLoop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveHyperdeckConn(conn)
	}
}

// serveHyperdeckConn handles one client: greeting, then a line loop until
// the client quits or hangs up.
// deckClient is one HyperDeck control connection; notify= one subscribed to
// async transport pushes.
type deckClient struct {
	conn   net.Conn
	wmu    sync.Mutex
	notify bool
}

func (d *deckClient) write(s string) bool {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_, err := io.WriteString(d.conn, s)
	return err == nil
}

// deckRegistry: connected (and optionally subscribed) clients. Deck sessions
// are rare — a handful of controllers — so an RW-mutexed map beats channels.
var (
	deckRegMu   sync.RWMutex
	deckClients = map[*deckClient]bool{}
)

func deckDrop(d *deckClient) {
	deckRegMu.Lock()
	delete(deckClients, d)
	deckRegMu.Unlock()
}

func deckNotifyAll(body string) {
	deckRegMu.RLock()
	subs := make([]*deckClient, 0, len(deckClients))
	for c := range deckClients {
		if c.notify {
			subs = append(subs, c)
		}
	}
	deckRegMu.RUnlock()
	for _, s := range subs {
		go s.write(body) // per-write goroutine: a dead control client can't block the loop
	}
}

func serveHyperdeckConn(conn net.Conn) {
	defer conn.Close()
	d := &deckClient{conn: conn}
	deckRegMu.Lock()
	deckClients[d] = true
	deckRegMu.Unlock()
	defer deckDrop(d)
	if _, err := io.WriteString(conn, "500 connection info:\r\nprotocol version: 1.9\r\nmodel: CuTePi\r\n\r\n"); err != nil {
		return
	}
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		reply, quit := handleDeckLine(d, strings.TrimSuffix(line, "\n"))
		if !d.write(reply) || quit {
			return
		}
	}
}

// deckNotifyLoop drives async transport pushes: whenever the playback state
// version moves, subscribers get a "500 transport info" frame. Polls the
// version counter (0.25s) rather than cross-wiring gsp internals.
func deckNotifyLoop() {
	last := uint64(0)
	for {
		time.Sleep(250 * time.Millisecond)
		v := gsp.StateVersion()
		if v != last {
			last = v
			deckNotifyAll("500 transport info:\r\n" + transportInfoBody() + "\r\n")
		}
	}
}

// parseDeckLine splits "{cmd}[ {param}: {value} ...]" into a command and a
// lowercase param map. Token walk: key words accumulate until a token ends
// with ':', then the value accumulates until the next colon-token. Values
// that end in a colon (none this device speaks) would confuse it — timecode
// values end in digits, so "+00:00:02:00" parses as one value token.
func parseDeckLine(line string) (string, map[string]string) {
	head, rest, found := strings.Cut(line, ":")
	cmd := strings.ToLower(strings.TrimSpace(head))
	params := map[string]string{}
	if !found {
		return cmd, params
	}
	var key, val strings.Builder
	flush := func() {
		if key.Len() > 0 {
			params[strings.ToLower(key.String())] = strings.TrimSpace(val.String())
			key.Reset()
			val.Reset()
		}
	}
	inKey := true
	for _, tok := range strings.Fields(rest) {
		ends := strings.HasSuffix(tok, ":")
		w := strings.TrimSuffix(tok, ":")
		if inKey {
			if key.Len() > 0 {
				key.WriteByte(' ')
			}
			key.WriteString(w)
			if ends {
				inKey = false
			}
			continue
		}
		if ends { // a new key opened before any value: missing value
			flush()
			key.Reset()
			key.WriteString(w)
			continue
		}
		if val.Len() > 0 {
			val.WriteByte(' ')
		}
		val.WriteString(w)
	}
	flush()
	return cmd, params
}

// handleDeckLine executes one HyperDeck line; quit returns close=true.
func handleDeckLine(client *deckClient, line string) (reply string, close bool) {
	cmd, params := parseDeckLine(line)
	switch cmd {
	case "quit":
		return "200 ok\r\n\r\n", true
	case "ping":
		return "200 ok\r\n\r\n", false
	case "remote", "remote: override", "play on startup", "preview":
		// Accept and ignore: remote enable/override is meaningless on a
		// device whose web UI is always the same authority, but answering
		// keeps controllers from treating the session as wedged.
		return "200 ok\r\n\r\n", false
	case "notify":
		// notify: transport: {true|false} — subscribe to async "500
		// transport info" frames, the protocol's feedback channel; Companion
		// and CM button state lights receive these.
		if client != nil {
			sub := params["transport"] == "true"
			client.notify = sub
			logs.Printf(logs.RTEDeck, "hyperdeck notify transport: %v", sub)
		}
		return "200 ok\r\n\r\n", false
	case "commands":
		// Machine-readable discovery, minimal but the same XML shape, so
		// auto-configuring controllers don't treat the device as alien.
		return commandsXML(), false
	case "help", "?":
		return "200 ok:\r\n" + strings.Join([]string{
			"quit", "ping", "device info", "transport info", "clips get", "clips count",
			"play[: clip id: {n}][: timecode: {t}][: speed: {pct}][: loop: {b}]",
			"play: speed: 0 pauses; negative speed unsupported",
			"stop", "pause",
			"goto[: clip id: {n}][: timecode: {HH:MM:SS:FF | seconds}]",
			"record: not implemented (playback device, answers 104)",
			"",
		}, "\r\n") + "\r\n", false
	case "device info":
		return "201 device info:\r\nprotocol version: 1.9\r\nmodel: CuTePi\r\nfriendly name: CuTePi show controller\r\n\r\n", false
	case "transport info":
		return transportInfo() + "\r\n", false
	case "clips get", "disk list":
		s, err := clipListResponse(params["clip id"], params["count"])
		if err != nil {
			return "103 database failure:\r\n\r\n", false
		}
		return s, false
	case "clips count":
		n, err := clipCount()
		if err != nil {
			return "103 database failure:\r\n\r\n", false
		}
		return fmt.Sprintf("210 clips count: %d\r\n\r\n", n), false
	case "play":
		if v, ok := params["clip id"]; ok {
			id, err := clipOffset(v)
			if err != nil {
				return "100 syntax error: clip id " + v + "\r\n\r\n", false
			}
			if perr := fireClip(id, params["timecode"]); perr != nil {
				logs.Printf(logs.RTEDeckErr, "hyperdeck play clip %s: %v", v, perr)
				return "105 load failure: no such clip id: " + v + "\r\n\r\n", false
			}
		} else if v, ok := params["timecode"]; ok {
			// play: timecode: X — position the transport, then start it.
			seconds, perr := parseTimecode(v)
			if perr != nil {
				return "100 syntax error: " + perr.Error() + "\r\n\r\n", false
			}
			gsp.Seek(seconds)
			gsp.Play()
		} else {
			gsp.Play()
		}
		if v, ok := params["speed"]; ok {
			pct, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return "100 syntax error: speed is a percentage\r\n\r\n", false
			}
			if err := applySpeed(pct); err != nil {
				logs.Printf(logs.RTEDeckErr, "hyperdeck speed %d: %v", pct, err)
				return "115 speed failure: " + err.Error() + "\r\n\r\n", false
			}
		}
		if v, ok := params["loop"]; ok {
			gsp.SetLoop(v == "true")
		}
		return "200 ok\r\n\r\n", false
	case "record", "record spill":
		logs.Printf(logs.RTEDeck, "hyperdeck record: refused (no recorder)")
		return "104 record failed: no recorder (CuTePi is playback only)\r\n\r\n", false
	case "stop":
		logs.Printf(logs.RTEStop, "STOP (hyperdeck)")
		gsp.Stop()
		return "200 ok\r\n\r\n", false
	case "pause":
		logs.Printf(logs.RTEPause, "PAUSE (hyperdeck)")
		gsp.Pause()
		return "200 ok\r\n\r\n", false
	case "goto":
		if v, ok := params["clip id"]; ok {
			id, err := clipOffset(v)
			if err != nil {
				return "100 syntax error: clip id " + v + "\r\n\r\n", false
			}
			// goto: clip id: {n|±n}: select (±n = step from the current clip,
			// the deck way of saying next/previous) — playhead only, no start.
			cue, cerr := cueAtSheetPos(id)
			if cerr != nil {
				return "105 load failure: no such clip id: " + v + "\r\n\r\n", false
			}
			if serr := ctp.SetCue(strconv.Itoa(cue.CuePos)); serr != nil {
				return "105 load failure: no such clip id: " + v + "\r\n\r\n", false
			}
			return "200 ok\r\n\r\n", false
		}
		if v, ok := params["timecode"]; ok {
			seconds, perr := parseTimecode(v)
			if perr != nil {
				return "100 syntax error: " + perr.Error() + "\r\n\r\n", false
			}
			gsp.Seek(seconds)
			return "200 ok\r\n\r\n", false
		}
		return "200 ok\r\n\r\n", false
	default:
		logs.Printf(logs.RTEDeckErr, "hyperdeck unknown command: %q", line)
		return "100 unknown command: " + strings.TrimSpace(line) + "\r\n\r\n", false
	}
}

// parseTimecode accepts "HH:MM:SS(:FF|)" (25fps frame rate) and plain
// seconds ("12", "12.5"). Returns seconds.
func parseTimecode(v string) (float64, error) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "+"))
	parts := strings.Split(v, ":")
	switch len(parts) {
	case 1:
		return strconv.ParseFloat(v, 64)
	case 3, 4:
		vals := make([]float64, len(parts))
		for i, p := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				return 0, fmt.Errorf("timecode %q: %v", v, err)
			}
			vals[i] = f
		}
		s := vals[0]*3600 + vals[1]*60 + vals[2]
		if len(vals) == 4 {
			s += vals[3] / float64(MediaFPS)
		}
		return s, nil
	default:
		return 0, fmt.Errorf("timecode %q not HH:MM:SS(:FF) or seconds", v)
	}
}

// applySpeed puts a "speed: {pct}" percentage onto the transport. Negative
// speeds (rewind) are refused: the pipeline cannot shuttle backwards. 0
// pauses (that is how a paused deck is represented on the wire, too).
func applySpeed(pct int) error {
	switch {
	case pct < 0:
		return fmt.Errorf("speed %d unsupported: cannot shuttle backwards", pct)
	case pct == 0:
		gsp.Pause()
	default:
		gsp.SetRate(float64(pct) / 100)
	}
	return nil
}

// clipOffset parses a deck clip id: a plain integer row position, or ±N
// relative to the currently selected clip (the protocol's formal way to say
// next/previous).
func clipOffset(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, errors.New("empty clip id")
	}
	if v[0] == '+' || v[0] == '-' {
		off, err := strconv.Atoi(v)
		if err != nil {
			return 0, err
		}
		// Anchor = the playing clip, else the selected playhead.
		pos := gsp.CurrentCuePos()
		if pos == 0 {
			pos, _ = ctp.SelectedCuePos()
		}
		return cueNextSheetPos(pos, off), nil
	}
	return strconv.Atoi(v)
}

// cueNextSheetPos walks ±off sheet rows from pos through the existing cues
// (like deck clip lists, positions collapse around deletions) — 0 when the
// walk exits the sheet.
func cueNextSheetPos(pos, off int) int {
	if off == 0 {
		return pos
	}
	cues, err := ctp.GetCuesheet()
	if err != nil {
		return 0
	}
	idx := -1
	for i, c := range cues.Cues {
		if c.CuePos == pos {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Nothing selected: the walk starts before the first row (+N from there).
		idx = -1
	}
	i := idx + off
	if i < 0 || i >= len(cues.Cues) {
		return 0
	}
	return cues.Cues[i].CuePos
}

func cueAtSheetPos(pos int) (ctp.Cue, error) {
	return ctp.GetCue(strconv.Itoa(pos))
}

// fireClip plays the clip at sheet position id (offset applied), optionally
// offsetting into it by a timecode.
func fireClip(id int, timecode string) error {
	cue, err := cueAtSheetPos(id)
	if err != nil {
		return err
	}
	logs.Printf(logs.RTEDeck, "hyperdeck play clip id %d", id)
	if err := FireCue(strconv.Itoa(cue.CuePos)); err != nil {
		return err
	}
	if timecode != "" {
		if seconds, perr := parseTimecode(timecode); perr == nil {
			gsp.Seek(seconds)
		}
	}
	return nil
}

// --- transport state (shared by both protocols) ------------------------------

// deckTransportState maps the player onto a HyperDeck transport status.
// HyperDeck has no "paused" state: real decks report play with speed 0,
// which is how CM/Companion controllers expect to render a hold.
func deckTransportState() (string, int) {
	if gsp.CurrentPlaying() == "" {
		return "stopped", 0
	}
	if gsp.IsPaused() {
		return "play", 0
	}
	if rate := gsp.Rate(); rate != 1 {
		return "play", int(rate * 100)
	}
	return "play", 100
}

func currentClipID() string {
	if pos := gsp.CurrentCuePos(); pos > 0 {
		return strconv.Itoa(pos)
	}
	return "none"
}

// timecode renders seconds as HH:MM:SS:FF at the CuTePi clock roll.
func timecode(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	frames := int(seconds * float64(MediaFPS))
	return fmt.Sprintf("%02d:%02d:%02d:%02d",
		frames/(MediaFPS*3600), frames%(MediaFPS*3600)/(MediaFPS*60),
		frames%(MediaFPS*60)/MediaFPS, frames%MediaFPS)
}

// transportInfoBody renders the parameter lines of a transport info frame
// (shared by the 208 response and the async 500 push).
func transportInfoBody() string {
	status, speed := deckTransportState()
	loop := "false"
	if gsp.Loop() {
		loop = "true"
	}
	pos := timecode(gsp.CurrentPosition())
	return fmt.Sprintf("status: %s\r\nspeed: %d\r\nslot id: none\r\nslot name: CuTePi\r\nclip id: %s\r\nsingle clip: true\r\ndisplay timecode: %s\r\ntimecode: %s\r\nvideo format: 1080p25\r\nloop: %s\r\ntimeline: 1\r\n",
		status, speed, currentClipID(), pos, pos, loop)
}

// transportInfo renders the full "transport info" response.
func transportInfo() string {
	return "208 transport info:\r\n" + transportInfoBody()
}

// commandsXML answers the "commands" discovery probe with the supported
// subset (real decks' XML differs per model; auto-configuring controllers
// parse the shape, not the full inventory).
func commandsXML() string {
	return "<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n<commands>\r\n" +
		strings.Join([]string{
			"ping", "quit", "device info", "transport info", "clips get", "clips count",
			"play", "goto", "stop", "pause", "playrange", "notify?", "record?", "prewarm??",
		}, "\r\n") +
		"\r\n</commands>\r\n\r\n"
}

// clipListResponse renders the cue sheet as the deck's clip list: clip id is
// the cue position, so "goto/play: clip id: N" targets the Nth sheet row
// exactly like a deck targets timeline clips.
func clipListResponse(clipID, count string) (string, error) {
	cu, err := ctp.GetCuesheet()
	if err != nil {
		return "", err
	}
	// Optional window: "clips get: clip id: {n}[ count: {m}]", with ±N
	// offsets resolving sheet rows like goto/play.
	start, window := 0, len(cu.Cues)
	if id, err := clipOffset(clipID); err == nil && id > 0 {
		for i, c := range cu.Cues {
			if c.CuePos == id {
				start = i
				break
			}
		}
		if c, cerr := strconv.Atoi(strings.TrimSpace(count)); cerr == nil && c > 0 && start+c < len(cu.Cues) {
			window = start + c
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "205 clips info:\r\nclip count: %d\r\n", window-start)
	for _, cue := range cu.Cues[start:window] {
		dur := float64(cue.PosEnd-cue.PosStart) / 1000
		if dur < 0 || cue.PosEnd <= 0 {
			dur = 0
		}
		name := cue.Filename
		if cue.Title != "" {
			name = cue.Title
		}
		fmt.Fprintf(&b, "%d: %s %s %s\r\n", cue.CuePos, name,
			timecode(float64(cue.PosStart)/1000), timecode(dur))
	}
	b.WriteString("\r\n")
	return b.String(), nil
}

func clipCount() (int, error) {
	cu, err := ctp.GetCuesheet()
	if err != nil {
		return 0, err
	}
	return len(cu.Cues), nil
}
