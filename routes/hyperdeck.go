package routes

// HyperDeck Ethernet Protocol (TCP 9993, §12.8). CuTePi presents itself as a
// Blackmagic HyperDeck Studio Mini speaking protocol 1.11, the model and
// version Bitfocus Companion's HyperDeck module (hyperdeck-connection) is
// built around:
//
//   - greeting "500 connection info" on connect, then the client's
//     "watchdog: period: N" (the connection closes after N seconds without
//     a command) and a "ping" every few seconds;
//   - commands arrive single-line ("play: speed: 100") or multi-line
//     ("notify:" + one "{param}: {value}" line each + a blank line);
//   - replies are "{code} {name}" or "{code} {name}:" + param lines + a blank
//     line; errors are the protocol's numbered codes (1xx);
//   - notifications ride 5xx frames (508 transport, 502 slot, 510 remote,
//     511 configuration, 513 display timecode, 515 playrange) to clients that
//     subscribed with "notify:".
//
// The cue sheet in play order is the deck's clip list (or the media pool,
// per Settings): clip ids are 1..N in that order, as on a deck's timeline.
// Slot 1 holds that list; slot 2 is an empty card slot, as on a Studio Mini
// with one card inserted.
//
// CuTePi is a playback device: "record" and "format" answer 103
// (unsupported), and there is no rewind, so negative speeds answer 103 as
// well. "stop" stops the cue (the show-controller meaning); a paused cue is
// reported like a stopped deck holding its frame (status stopped, clip id
// kept), so a Play/Stop toggle resumes it.

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

const (
	deckModel           = "HyperDeck Studio Mini"
	deckProtocolVersion = "1.11"
	deckSlotCount       = 2
)

// deckGotoClip is the clip id a HyperDeck "goto" cued and a bare "play" has
// yet to start (0 = none).
var deckGotoClip atomic.Int32

// MediaFPS is the clock roll rate used for HyperDeck timecodes (real decks
// roll at the project frame rate; CuTePi reports 1080p25).
const MediaFPS = 25

const deckVideoFormat = "1080p25"

// Protocol error replies (single line, as real decks send them).
const (
	deckOK             = "200 ok\r\n"
	deckSyntaxError    = "100 syntax error\r\n"
	deckUnsupportedPar = "101 unsupported parameter\r\n"
	deckInvalidValue   = "102 invalid value\r\n"
	deckUnsupported    = "103 unsupported\r\n"
	deckNoDisk         = "105 no disk\r\n"
	deckTimelineEmpty  = "107 timeline empty\r\n"
	deckInternalError  = "108 internal error\r\n"
	deckOutOfRange     = "109 out of range\r\n"
	deckRemoteDisabled = "111 remote control disabled\r\n"
	deckRejected       = "120 connection rejected\r\n"
)

// --- listener -----------------------------------------------------------------

// ListenHyperdeck runs the HyperDeck TCP listener until it is closed
// (exported for the test harness). Bind failures log and return.
func ListenHyperdeck(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logs.Printf(logs.RTEDeckErr, "hyperdeck listener: %v", err)
		return
	}
	serveHyperdeck(ln)
}

// startHyperdeck binds synchronously (so the caller sees bind errors) and
// serves in the background; closing the returned listener stops it.
func startHyperdeck(addr string) (io.Closer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go serveHyperdeck(ln)
	return ln, nil
}

var deckNotifyOnce sync.Once

func serveHyperdeck(ln net.Listener) {
	deckNotifyOnce.Do(func() { go deckNotifyLoop() })
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveHyperdeckConn(conn)
	}
}

// Notification subscriptions ("notify: {param}: true").
const (
	notifyTransport uint32 = 1 << iota
	notifySlot
	notifyRemote
	notifyConfiguration
	notifyDroppedFrames
	notifyDisplayTimecode
	notifyTimelinePosition
	notifyPlayrange
	notifyCache
	notifyDynamicRange
)

var notifyParams = []struct {
	name string
	bit  uint32
}{
	{"remote", notifyRemote},
	{"transport", notifyTransport},
	{"slot", notifySlot},
	{"configuration", notifyConfiguration},
	{"dropped frames", notifyDroppedFrames},
	{"display timecode", notifyDisplayTimecode},
	{"timeline position", notifyTimelinePosition},
	{"playrange", notifyPlayrange},
	{"cache", notifyCache},
	{"dynamic range", notifyDynamicRange},
}

// deckClient is one HyperDeck control connection. Replies are written by
// the connection's goroutine; notifications queue on out and are written by
// the client's own pusher, so a slow client never blocks the notify loop and
// frames stay whole (wmu).
type deckClient struct {
	conn     net.Conn
	wmu      sync.Mutex
	notify   atomic.Uint32
	watchdog atomic.Int64 // seconds; 0 = off
	out      chan string
}

// deckWriteTimeout bounds every write to a control client: a peer that
// stops reading is dropped instead of pinning goroutines forever.
const deckWriteTimeout = 2 * time.Second

// deckMaxLine caps one command line. Real commands are a few dozen bytes;
// an endless line would otherwise grow the read buffer without bound.
const deckMaxLine = 4 << 10

// deckMaxParams caps the parameter lines of one multi-line command.
const deckMaxParams = 32

// deckWatchdogGrace is added to the client's watchdog period before an idle
// connection is closed, so a ping delayed by a busy controller isn't fatal.
const deckWatchdogGrace = 2 * time.Second

func (d *deckClient) write(s string) bool {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	return d.writeLocked(s)
}

func (d *deckClient) writeLocked(s string) bool {
	_ = d.conn.SetWriteDeadline(time.Now().Add(deckWriteTimeout))
	_, err := io.WriteString(d.conn, s)
	if err != nil {
		_ = d.conn.Close() // unblocks the read loop, which deregisters
	}
	return err == nil
}

// deckRegistry: connected clients. Deck sessions are rare — a handful of
// controllers — so an RW-mutexed map beats channels.
var (
	deckRegMu   sync.RWMutex
	deckClients = map[*deckClient]bool{}
)

// deckMaxClients caps concurrent control connections. Like a real deck (and
// hyperdeck-server-connection), one client at a time by default; the
// watchdog frees the slot of a controller that vanished.
var deckMaxClients = 1

// deckDropAll hangs up every client (listener disabled from Settings).
func deckDropAll() {
	deckRegMu.RLock()
	conns := make([]net.Conn, 0, len(deckClients))
	for c := range deckClients {
		conns = append(conns, c.conn)
	}
	deckRegMu.RUnlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func deckDrop(d *deckClient) {
	deckRegMu.Lock()
	delete(deckClients, d)
	deckRegMu.Unlock()
}

// deckSubscribed reports whether any client subscribed to bit.
func deckSubscribed(bit uint32) bool {
	deckRegMu.RLock()
	defer deckRegMu.RUnlock()
	for c := range deckClients {
		if c.notify.Load()&bit != 0 {
			return true
		}
	}
	return false
}

// deckNotifyAll queues frame for every client subscribed to bit. A client
// whose queue is full (not reading) misses the frame; its write deadline
// drops it soon anyway.
func deckNotifyAll(bit uint32, frame string) {
	deckRegMu.RLock()
	defer deckRegMu.RUnlock()
	for c := range deckClients {
		if c.notify.Load()&bit == 0 {
			continue
		}
		select {
		case c.out <- frame:
		default:
		}
	}
}

func serveHyperdeckConn(conn net.Conn) {
	defer conn.Close()
	d := &deckClient{conn: conn, out: make(chan string, 64)}
	deckRegMu.Lock()
	if deckMaxClients > 0 && len(deckClients) >= deckMaxClients {
		deckRegMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(deckWriteTimeout))
		_, _ = io.WriteString(conn, deckRejected)
		return
	}
	deckClients[d] = true
	deckRegMu.Unlock()
	defer deckDrop(d)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case f := <-d.out:
				if !d.write(f) {
					return
				}
			}
		}
	}()
	logs.Printf(logs.RTEDeck, "hyperdeck client %s connected", conn.RemoteAddr())
	defer logs.Printf(logs.RTEDeck, "hyperdeck client %s disconnected", conn.RemoteAddr())
	if !d.write("500 connection info:\r\nprotocol version: " + deckProtocolVersion + "\r\nmodel: " + deckModel + "\r\n\r\n") {
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 512), deckMaxLine) // over-long line: Scan fails, client dropped
	var (
		blockCmd    string
		blockParams map[string]string
	)
	for {
		if wd := d.watchdog.Load(); wd > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(wd)*time.Second + deckWatchdogGrace))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
				logs.Printf(logs.RTEDeck, "hyperdeck client %s: watchdog expired", conn.RemoteAddr())
			}
			return
		}
		line := strings.TrimSpace(sc.Text())
		var reply string
		var quit bool
		// The write lock spans the command, so the reply goes out before
		// any notification the command itself triggers (as on a deck).
		d.wmu.Lock()
		switch {
		case blockParams != nil:
			// Inside a multi-line command: param lines until the blank line.
			if line != "" {
				if len(blockParams) >= deckMaxParams {
					d.wmu.Unlock()
					return
				}
				k, v, _ := strings.Cut(line, ":")
				blockParams[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
				d.wmu.Unlock()
				continue
			}
			reply, quit = handleDeckCommand(d, blockCmd, blockParams)
			blockParams = nil
		case line == "":
			d.wmu.Unlock()
			continue // stray blank lines between commands are ignored
		case strings.HasSuffix(line, ":") && !strings.Contains(strings.TrimSuffix(line, ":"), ":"):
			// "{command}:" opens a multi-line command.
			blockCmd = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(line, ":")))
			blockParams = map[string]string{}
			d.wmu.Unlock()
			continue
		default:
			reply, quit = handleDeckLine(d, line)
		}
		ok := d.writeLocked(reply)
		d.wmu.Unlock()
		if !ok || quit {
			return
		}
	}
}

// --- notifications ---------------------------------------------------------------

// deckNotifyLoop drives the async pushes. It polls the playback state version
// (transport), the position (display timecode) and, once a second, the clip
// list (slot) rather than cross-wiring gsp internals.
func deckNotifyLoop() {
	var lastVersion, seenVersion uint64
	var lastTC, lastClips string
	tick := 0
	for {
		time.Sleep(100 * time.Millisecond)
		tick++
		// A state change is pushed once the version has held for a tick, so
		// the half-built state of a load (clip not yet known) isn't sent.
		if v := gsp.StateVersion(); v != seenVersion {
			seenVersion = v
		} else if v != lastVersion {
			lastVersion = v
			if deckSubscribed(notifyTransport) {
				deckNotifyAll(notifyTransport, "508 transport info:\r\n"+transportInfoBody()+"\r\n")
			}
		}
		if deckSubscribed(notifyDisplayTimecode) {
			if tc := timecode(gsp.CurrentPosition()); tc != lastTC {
				lastTC = tc
				deckNotifyAll(notifyDisplayTimecode, "513 display timecode:\r\ndisplay timecode: "+tc+"\r\n\r\n")
			}
		}
		if tick%10 == 0 && deckSubscribed(notifySlot) {
			// A changed clip list is announced like a card change: the
			// controller re-reads slot info and the clip list.
			if clips, err := clipListResponse("", "", 1); err == nil && clips != lastClips {
				if lastClips != "" {
					deckNotifyAll(notifySlot, "502 slot info:\r\n"+slotInfoBody(1)+"\r\n")
				}
				lastClips = clips
			}
		}
	}
}

// --- deck state that exists only on the wire -----------------------------------

// deckState holds the settings a HyperDeck controller can read back but that
// have no meaning on CuTePi (recording configuration, remote enable). They
// live for the life of the process, like a deck's front-panel state.
var deckState = struct {
	sync.Mutex
	remoteEnabled  bool
	remoteOverride bool
	config         map[string]string
}{
	remoteEnabled: true,
	config: map[string]string{
		"video input":          "SDI",
		"audio input":          "embedded",
		"file format":          "H.264High",
		"audio codec":          "AAC",
		"timecode input":       "internal",
		"timecode preset":      "00:00:00:00",
		"audio input channels": "2",
		"record trigger":       "none",
		"append timestamp":     "false",
	},
}

var deckConfigOrder = []string{
	"video input", "audio input", "file format", "audio codec", "timecode input",
	"timecode preset", "audio input channels", "record trigger", "record prefix",
	"append timestamp",
}

func deckRemoteEnabled() bool {
	deckState.Lock()
	defer deckState.Unlock()
	return deckState.remoteEnabled
}

func remoteInfoBody() string {
	deckState.Lock()
	defer deckState.Unlock()
	return fmt.Sprintf("enabled: %t\r\noverride: %t\r\n", deckState.remoteEnabled, deckState.remoteOverride)
}

func configurationBody() string {
	deckState.Lock()
	defer deckState.Unlock()
	var b strings.Builder
	for _, k := range deckConfigOrder {
		if v := deckState.config[k]; v != "" {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	return b.String()
}

// deckConfigValues lists the accepted values per configuration parameter
// (nil = free text). Values are the Studio Mini's.
var deckConfigValues = map[string][]string{
	"video input":          {"SDI"},
	"audio input":          {"embedded"},
	"file format":          nil,
	"audio codec":          {"PCM", "AAC"},
	"timecode input":       {"external", "embedded", "preset", "clip", "internal"},
	"timecode preset":      nil,
	"audio input channels": {"2", "4", "8", "16"},
	"record trigger":       {"none", "recordbit", "timecoderun"},
	"record prefix":        nil,
	"append timestamp":     {"true", "false"},
}

// --- commands ---------------------------------------------------------------------

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

// handleDeckLine executes one single-line HyperDeck command; quit returns
// close=true.
func handleDeckLine(client *deckClient, line string) (reply string, close bool) {
	cmd, params := parseDeckLine(line)
	return handleDeckCommand(client, cmd, params)
}

// deckControlCommands change the transport or the deck; with remote control
// disabled ("remote: enable: false") they answer 111, as on a deck.
var deckControlCommands = map[string]bool{
	"play": true, "stop": true, "pause": true, "goto": true, "jog": true,
	"shuttle": true, "record": true, "record spill": true, "playrange set": true,
	"playrange clear": true, "slot select": true, "format": true, "preview": true,
	"clips add": true, "clips remove": true, "clips clear": true,
}

// handleDeckCommand executes one parsed command.
func handleDeckCommand(client *deckClient, cmd string, params map[string]string) (reply string, close bool) {
	if deckControlCommands[cmd] && !deckRemoteEnabled() {
		return deckRemoteDisabled, false
	}
	switch cmd {
	case "quit":
		return deckOK, true
	case "ping":
		return deckOK, false
	case "watchdog":
		v, ok := params["period"]
		n, err := strconv.Atoi(v)
		if !ok || err != nil || n < 0 {
			return deckInvalidValue, false
		}
		if client != nil {
			client.watchdog.Store(int64(n))
		}
		return deckOK, false
	case "device info":
		return deviceInfo(), false
	case "commands":
		return commandsXML(), false
	case "help", "?":
		return helpText(), false
	case "notify":
		return deckNotify(client, params), false
	case "remote":
		if len(params) == 0 {
			return "210 remote info:\r\n" + remoteInfoBody() + "\r\n", false
		}
		return deckRemote(params), false
	case "configuration":
		if len(params) == 0 {
			return "211 configuration:\r\n" + configurationBody() + "\r\n", false
		}
		return deckConfigure(params), false
	case "slot info":
		id := 1
		if v, ok := params["slot id"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return deckInvalidValue, false
			}
			id = n
		}
		if id < 1 || id > deckSlotCount {
			return deckOutOfRange, false
		}
		return "202 slot info:\r\n" + slotInfoBody(id) + "\r\n", false
	case "slot select":
		if v, ok := params["slot id"]; ok {
			switch v {
			case "1":
			case "2":
				return deckNoDisk, false
			default:
				return deckOutOfRange, false
			}
		}
		if v, ok := params["video format"]; ok && v != deckVideoFormat {
			return deckInvalidValue, false
		}
		return deckOK, false
	case "transport info":
		return transportInfo() + "\r\n", false
	case "clips get":
		version := 1
		if v, ok := params["version"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 2 {
				return deckInvalidValue, false
			}
			version = n
		}
		s, err := clipListResponse(params["clip id"], params["count"], version)
		if err != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck clips get: %v", err)
			return deckInternalError, false
		}
		return s, false
	case "clips count":
		n, err := clipCount()
		if err != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck clips count: %v", err)
			return deckInternalError, false
		}
		return fmt.Sprintf("214 clips count:\r\nclip count: %d\r\n\r\n", n), false
	case "disk list":
		if v, ok := params["slot id"]; ok && v != "1" {
			if v == "2" {
				return deckNoDisk, false
			}
			return deckOutOfRange, false
		}
		s, err := diskListResponse()
		if err != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck disk list: %v", err)
			return deckInternalError, false
		}
		return s, false
	case "play":
		return deckPlay(params), false
	case "shuttle":
		v, ok := params["speed"]
		if !ok {
			return deckSyntaxError, false
		}
		return deckSpeed(v), false
	case "record", "record spill", "format", "clips add", "clips remove", "clips clear":
		logs.Printf(logs.RTEDeck, "hyperdeck %s: refused (CuTePi is playback only)", cmd)
		return deckUnsupported, false
	case "stop":
		logs.Printf(logs.RTEStop, "STOP (hyperdeck)")
		deckGotoClip.Store(0)
		gsp.Stop()
		return deckOK, false
	case "pause":
		logs.Printf(logs.RTEPause, "PAUSE (hyperdeck)")
		gsp.Pause()
		return deckOK, false
	case "goto":
		return deckGoto(params), false
	case "jog":
		v, ok := params["timecode"]
		if !ok {
			return deckSyntaxError, false
		}
		if err := seekTimecode(v); err != nil {
			return deckInvalidValue, false
		}
		return deckOK, false
	case "playrange":
		// No play range is ever set (see "playrange set").
		return "219 playrange info:\r\ntimeline in: 0\r\ntimeline out: 0\r\n\r\n", false
	case "playrange clear":
		return deckOK, false
	case "playrange set":
		return deckUnsupported, false
	case "play option":
		if v, ok := params["stop mode"]; ok {
			if v != "lastframe" {
				return deckUnsupported, false
			}
			return deckOK, false
		}
		return "220 play option:\r\nstop mode: lastframe\r\n\r\n", false
	case "play on startup":
		if len(params) > 0 {
			if params["enable"] == "true" {
				return deckUnsupported, false
			}
			return deckOK, false
		}
		return "218 play on startup:\r\nenable: false\r\nsingle clip: false\r\n\r\n", false
	case "dynamic range":
		if v, ok := params["playback override"]; ok && v != "off" {
			return deckUnsupported, false
		}
		if len(params) > 0 {
			return deckOK, false
		}
		return "222 dynamic range:\r\nplayback override: off\r\n\r\n", false
	case "preview", "identify":
		return deckOK, false
	case "uptime":
		return fmt.Sprintf("201 uptime:\r\nuptime: %d\r\n\r\n", int(time.Since(deckStarted).Seconds())), false
	default:
		logs.Printf(logs.RTEDeckErr, "hyperdeck unknown command: %q %v", cmd, params)
		return deckSyntaxError, false
	}
}

var deckStarted = time.Now()

func deviceInfo() string {
	name, _ := os.Hostname()
	if name == "" {
		name = "CuTePi"
	}
	return "204 device info:\r\n" +
		"protocol version: " + deckProtocolVersion + "\r\n" +
		"model: " + deckModel + "\r\n" +
		fmt.Sprintf("unique id: %08x\r\n", crc32.ChecksumIEEE([]byte(name))) +
		fmt.Sprintf("slot count: %d\r\n", deckSlotCount) +
		"software version: CuTePi\r\n" +
		"name: " + name + "\r\n\r\n"
}

func deckNotify(client *deckClient, params map[string]string) string {
	if len(params) == 0 {
		var cur uint32
		if client != nil {
			cur = client.notify.Load()
		}
		var b strings.Builder
		b.WriteString("209 notify:\r\n")
		for _, p := range notifyParams {
			fmt.Fprintf(&b, "%s: %t\r\n", p.name, cur&p.bit != 0)
		}
		return b.String() + "\r\n"
	}
	set, clear := uint32(0), uint32(0)
	for k, v := range params {
		bit := uint32(0)
		for _, p := range notifyParams {
			if p.name == k {
				bit = p.bit
			}
		}
		if bit == 0 {
			return deckUnsupportedPar
		}
		switch v {
		case "true":
			set |= bit
		case "false":
			clear |= bit
		default:
			return deckInvalidValue
		}
	}
	if client != nil {
		for {
			old := client.notify.Load()
			if client.notify.CompareAndSwap(old, old&^clear|set) {
				break
			}
		}
		logs.Printf(logs.RTEDeck, "hyperdeck notify: %v", params)
	}
	return deckOK
}

func deckRemote(params map[string]string) string {
	deckState.Lock()
	for k, v := range params {
		if v != "true" && v != "false" {
			deckState.Unlock()
			return deckInvalidValue
		}
		switch k {
		case "enable":
			deckState.remoteEnabled = v == "true"
		case "override":
			deckState.remoteOverride = v == "true"
		default:
			deckState.Unlock()
			return deckUnsupportedPar
		}
	}
	deckState.Unlock()
	logs.Printf(logs.RTEDeck, "hyperdeck remote: %v", params)
	deckNotifyAll(notifyRemote, "510 remote info:\r\n"+remoteInfoBody()+"\r\n")
	return deckOK
}

func deckConfigure(params map[string]string) string {
	for k, v := range params {
		allowed, known := deckConfigValues[k]
		if !known {
			return deckUnsupportedPar
		}
		if allowed != nil {
			ok := false
			for _, a := range allowed {
				if strings.EqualFold(a, v) {
					ok = true
				}
			}
			if !ok {
				return deckInvalidValue
			}
		}
	}
	deckState.Lock()
	var b strings.Builder
	for k, v := range params {
		deckState.config[k] = v
		b.WriteString(k + ": " + v + "\r\n")
	}
	deckState.Unlock()
	deckNotifyAll(notifyConfiguration, "511 configuration:\r\n"+b.String()+"\r\n")
	return deckOK
}

// deckPlay: "play[: clip id: {n}][: timecode: {t}][: speed: {pct}][: loop:
// {b}][: single clip: {b}]".
func deckPlay(params map[string]string) string {
	// Refuse a bad speed before anything starts.
	if v, ok := params["speed"]; ok {
		pct, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return deckInvalidValue
		}
		if pct < 0 {
			return deckUnsupported
		}
	}
	if v, ok := params["clip id"]; ok {
		deckGotoClip.Store(0)
		id, err := clipOffset(v)
		if err != nil {
			return deckInvalidValue
		}
		if perr := fireClip(id, params["timecode"]); perr != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck play clip %s: %v", v, perr)
			return deckOutOfRange
		}
	} else if v, ok := params["timecode"]; ok {
		// play: timecode: X — position the transport, then start it.
		seconds, perr := parseTimecode(v)
		if perr != nil {
			return deckInvalidValue
		}
		gsp.Seek(seconds)
		gsp.Play()
	} else if id := int(deckGotoClip.Swap(0)); id != 0 && id != playingClipID() {
		// "goto" only cued the playhead; the deck's next bare "play"
		// starts that clip rather than resuming the old one.
		if perr := fireClip(id, ""); perr != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck play goto'd clip %d: %v", id, perr)
			return deckOutOfRange
		}
	} else if gsp.CurrentPlaying() == "" {
		// Nothing loaded: play from the playhead (the selected cue, else
		// the first clip), without advancing the selection.
		clips, err := deckClipList()
		if err != nil || len(clips) == 0 {
			return deckTimelineEmpty
		}
		id := playheadClipID(clips)
		if id == 0 {
			id = 1
		}
		if perr := fireClip(id, ""); perr != nil {
			logs.Printf(logs.RTEDeckErr, "hyperdeck play clip %d: %v", id, perr)
			return deckOutOfRange
		}
	} else {
		gsp.Play()
	}
	if v, ok := params["speed"]; ok {
		if r := deckSpeed(v); r != deckOK {
			return r
		}
	}
	// A deck's loop flag is per playback. Controllers send "loop: false" with
	// every play, so only "true" is applied: it must not cancel a cue whose
	// own settings loop.
	if params["loop"] == "true" {
		gsp.SetClipLoop(true)
	}
	return deckOK
}

// deckSpeed puts a "speed: {pct}" percentage onto the transport. Negative
// speeds (rewind) are unsupported: the pipeline cannot play backwards. 0
// pauses.
func deckSpeed(v string) string {
	pct, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return deckInvalidValue
	}
	if err := applySpeed(pct); err != nil {
		logs.Printf(logs.RTEDeckErr, "hyperdeck speed %d: %v", pct, err)
		return deckUnsupported
	}
	return deckOK
}

// deckGoto: "goto: clip id: {n|+n|-n}" | "goto: clip: {start|end|+n|-n}" |
// "goto: timeline: {start|end|n|+n|-n}" | "goto: timecode: {tc|+tc|-tc}".
func deckGoto(params map[string]string) string {
	if len(params) != 1 {
		return deckSyntaxError
	}
	cueClip := func(v string) string {
		id, err := clipOffset(v)
		if err != nil {
			return deckInvalidValue
		}
		c, cerr := deckClipByID(id)
		if cerr != nil {
			return deckOutOfRange
		}
		if c.cuePos != 0 {
			if serr := ctp.SetCue(strconv.Itoa(c.cuePos)); serr != nil {
				return deckOutOfRange
			}
		}
		deckGotoClip.Store(int32(id))
		return deckOK
	}
	if v, ok := params["clip id"]; ok {
		return cueClip(v)
	}
	if v, ok := params["clip"]; ok {
		switch v {
		case "start":
			gsp.Seek(0)
			return deckOK
		case "end":
			if d := gsp.CurrentDuration(); d > 0 {
				gsp.Seek(max(0, d-1.0/MediaFPS))
			}
			return deckOK
		}
		if strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") {
			return cueClip(v)
		}
		return deckInvalidValue
	}
	if v, ok := params["timeline"]; ok {
		clips, err := deckClipList()
		if err != nil || len(clips) == 0 {
			return deckTimelineEmpty
		}
		switch {
		case v == "start":
			return cueClip("1")
		case v == "end":
			return cueClip(strconv.Itoa(len(clips)))
		case strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-"):
			return cueClip(v)
		}
		// Absolute timeline frame: the clip whose span holds it.
		frame, err := strconv.Atoi(v)
		if err != nil || frame < 0 {
			return deckInvalidValue
		}
		at := 0.0
		for _, c := range clips {
			at += c.duration
			if float64(frame)/MediaFPS < at {
				return cueClip(strconv.Itoa(c.id))
			}
		}
		return deckOutOfRange
	}
	if v, ok := params["timecode"]; ok {
		if err := seekTimecode(v); err != nil {
			return deckInvalidValue
		}
		return deckOK
	}
	return deckUnsupportedPar
}

// seekTimecode seeks to an absolute timecode, or by a "+tc"/"-tc" offset from
// the current position.
func seekTimecode(v string) error {
	v = strings.TrimSpace(v)
	sign := 0.0
	if strings.HasPrefix(v, "+") {
		sign = 1
	} else if strings.HasPrefix(v, "-") {
		sign = -1
	}
	seconds, err := parseTimecode(strings.TrimLeft(v, "+-"))
	if err != nil {
		return err
	}
	if sign != 0 {
		seconds = max(0, gsp.CurrentPosition()+sign*seconds)
	}
	gsp.Seek(seconds)
	return nil
}

// parseTimecode accepts "HH:MM:SS(:FF|)" (MediaFPS frame rate) and plain
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

// applySpeed puts a percentage speed onto the transport, relative to the
// cue's own programmed rate (100 = as designed): controllers send "speed:
// 100" with every play, which must not undo a cue's rate. Negative speeds
// (rewind) are refused: the pipeline cannot shuttle backwards. 0 pauses.
func applySpeed(pct int) error {
	switch {
	case pct < 0:
		return fmt.Errorf("speed %d unsupported: cannot shuttle backwards", pct)
	case pct == 0:
		gsp.Pause()
	default:
		gsp.SetRate(cueBaseRate() * float64(pct) / 100)
	}
	return nil
}

// cueBaseRate is the playing cue's programmed rate (1 when none).
func cueBaseRate() float64 {
	if pos := gsp.CurrentCuePos(); pos > 0 {
		if cue, err := ctp.GetCue(strconv.Itoa(pos)); err == nil && cue.Rate > 0 {
			return cue.Rate
		}
	}
	return 1
}

// clipOffset parses a deck clip id: a plain clip number, or ±N relative to
// the current clip (the protocol's formal way to say next/previous). A
// relative step that leaves the list yields 0.
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
		clips, err := deckClipList()
		if err != nil {
			return 0, err
		}
		// Anchor = the playing clip, else the playhead; with neither the
		// walk starts before the first clip.
		id := playingClipID()
		if id == 0 {
			id = playheadClipID(clips)
		}
		id += off
		if id < 1 || id > len(clips) {
			return 0, nil
		}
		return id, nil
	}
	return strconv.Atoi(v)
}

// fireClip plays clip id, optionally offset into it by a timecode.
func fireClip(id int, timecode string) error {
	c, err := deckClipByID(id)
	if err != nil {
		return err
	}
	if c.cuePos == 0 {
		if err := firePoolFile(c.file); err != nil {
			return err
		}
	} else {
		logs.Printf(logs.RTEDeck, "hyperdeck play clip id %d (cue %d)", id, c.cuePos)
		if err := FireCue(strconv.Itoa(c.cuePos)); err != nil {
			return err
		}
	}
	if timecode != "" {
		if seconds, perr := parseTimecode(timecode); perr == nil {
			gsp.Seek(seconds)
		}
	}
	return nil
}

// --- transport state ------------------------------------------------------------

// deckTransportState maps the player onto a HyperDeck transport status. A
// paused cue reads like a stopped deck holding its frame (so does a still,
// which is a clip parked on its last frame); speed is relative to the cue's
// programmed rate.
func deckTransportState() (string, int) {
	if gsp.CurrentPlaying() == "" || gsp.IsPaused() {
		return "stopped", 0
	}
	return "play", int(math.Round(gsp.Rate() / cueBaseRate() * 100))
}

func currentClipID() string {
	if id := playingClipID(); id > 0 {
		return strconv.Itoa(id)
	}
	return "none"
}

// playingClipID is the clip id of what is on the board (0 = none, or a file
// that isn't in the list).
func playingClipID() int {
	file := gsp.CurrentPlaying()
	if file == "" {
		return 0
	}
	clips, err := deckClipList()
	if err != nil {
		return 0
	}
	pos := gsp.CurrentCuePos()
	for _, c := range clips {
		if (c.cuePos != 0 && c.cuePos == pos) || (c.cuePos == 0 && c.file == file) {
			return c.id
		}
	}
	return 0
}

// playheadClipID is the clip the playhead sits on: a pending goto, else the
// selected cue (0 = none).
func playheadClipID(clips []deckClip) int {
	if id := int(deckGotoClip.Load()); id > 0 {
		return id
	}
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		return 0
	}
	for _, c := range clips {
		if c.cuePos == pos {
			return c.id
		}
	}
	return 0
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
// (shared by the 208 response and the async 508 push).
func transportInfoBody() string {
	status, speed := deckTransportState()
	pos := timecode(gsp.CurrentPosition())
	return fmt.Sprintf("status: %s\r\nspeed: %d\r\nslot id: 1\r\nclip id: %s\r\nsingle clip: true\r\ndisplay timecode: %s\r\ntimecode: %s\r\nvideo format: %s\r\nloop: %t\r\ninput video format: none\r\n",
		status, speed, currentClipID(), pos, pos, deckVideoFormat, gsp.CurrentPlaying() != "" && gsp.Loop())
}

// transportInfo renders the full "transport info" response.
func transportInfo() string {
	return "208 transport info:\r\n" + transportInfoBody()
}

// slotInfoBody: slot 1 carries the clip list; slot 2 is empty.
func slotInfoBody(id int) string {
	if id != 1 {
		return fmt.Sprintf("slot id: %d\r\nstatus: empty\r\nrecording time: 0\r\nvideo format: %s\r\n", id, deckVideoFormat)
	}
	name := "Cuesheet"
	if deckClipsFromPool() {
		name = "MediaPool"
	}
	return fmt.Sprintf("slot id: 1\r\nstatus: mounted\r\nvolume name: %s\r\nrecording time: 0\r\nvideo format: %s\r\n", name, deckVideoFormat)
}

// commandsXML answers the "commands" discovery probe with the supported set.
func commandsXML() string {
	var b strings.Builder
	b.WriteString("212 commands:\r\n<?xml version=\"1.0\" encoding=\"UTF-8\"?>\r\n<commands>\r\n")
	for _, c := range []string{
		"help", "commands", "device info", "disk list", "quit", "ping", "preview",
		"play", "playrange", "playrange set", "playrange clear", "play on startup",
		"play option", "record", "stop", "clips count", "clips get", "transport info",
		"slot info", "slot select", "dynamic range", "notify", "goto", "jog",
		"shuttle", "remote", "configuration", "uptime", "format", "identify", "watchdog",
	} {
		b.WriteString("    <command name=\"" + c + "\"/>\r\n")
	}
	b.WriteString("</commands>\r\n\r\n")
	return b.String()
}

func helpText() string {
	return "201 help:\r\n" + strings.Join([]string{
		"HyperDeck Studio Mini protocol " + deckProtocolVersion + " (CuTePi playback)",
		"clip ids are cue sheet rows (or media pool entries, per Settings)",
		"play [clip id|timecode|speed|loop], stop, goto [clip id|clip|timeline|timecode], jog, shuttle",
		"transport info, clips get [clip id|count|version], clips count, disk list, slot info",
		"device info, notify, remote, configuration, watchdog, ping, quit",
		"record and format are unsupported; speeds below 0 are unsupported",
	}, "\r\n") + "\r\n\r\n"
}

// --- clip lists --------------------------------------------------------------------

// clipSeconds is a cue's playing length (trim, hold time or file, over rate).
func clipSeconds(c ctp.Cue) float64 {
	return float64(ctp.EffectiveCueDuration(c)) / 1000
}

// deckClip is one entry of the deck's clip list.
type deckClip struct {
	id       int // 1..N in play order
	cuePos   int // the cue behind it (0 in media pool mode)
	name     string
	start    float64 // seconds into the file
	duration float64
	file     string
}

func deckClipByID(id int) (deckClip, error) {
	clips, err := deckClipList()
	if err != nil {
		return deckClip{}, err
	}
	if id < 1 || id > len(clips) {
		return deckClip{}, fmt.Errorf("no such clip id %d", id)
	}
	return clips[id-1], nil
}

func deckClipList() ([]deckClip, error) {
	var out []deckClip
	if deckClipsFromPool() {
		pool, err := ctp.GetMediapool()
		if err != nil {
			return nil, err
		}
		for i, m := range pool.Medias {
			out = append(out, deckClip{id: i + 1, name: m.Filename, duration: m.Duration, file: m.Filename})
		}
		return out, nil
	}
	cu, err := ctp.GetCuesheet()
	if err != nil {
		return nil, err
	}
	for i, c := range cu.Cues {
		name := c.Filename
		if c.Title != "" {
			name = c.Title
		}
		out = append(out, deckClip{id: i + 1, cuePos: c.CuePos, name: name, start: float64(c.PosStart) / 1000, duration: clipSeconds(c), file: c.Filename})
	}
	return out, nil
}

// clipListResponse renders "clips get": clip ids number the list in play
// order, as a deck numbers its timeline. Version 1 lines are "{id}: {name} {start} {duration}";
// version 2 lines are "{id}: {start} {duration} {in} {out} {name}".
func clipListResponse(clipID, count string, version int) (string, error) {
	clips, err := deckClipList()
	if err != nil {
		return "", err
	}
	// Optional window: "clips get: clip id: {n}[ count: {m}]", with ±N
	// offsets resolving like goto/play.
	start, end := 0, len(clips)
	if clipID != "" {
		if id, _ := clipOffset(clipID); id >= 1 && id <= len(clips) {
			start = id - 1
		}
		if n, cerr := strconv.Atoi(strings.TrimSpace(count)); cerr == nil && n > 0 && start+n < len(clips) {
			end = start + n
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "205 clips info:\r\nclip count: %d\r\n", end-start)
	for _, c := range clips[start:end] {
		in, dur := timecode(c.start), timecode(c.duration)
		if version == 2 {
			fmt.Fprintf(&b, "%d: %s %s %s %s %s\r\n", c.id, in, dur, in, timecode(c.start+c.duration), c.name)
		} else {
			fmt.Fprintf(&b, "%d: %s %s %s\r\n", c.id, c.name, in, dur)
		}
	}
	b.WriteString("\r\n")
	return b.String(), nil
}

func clipCount() (int, error) {
	clips, err := deckClipList()
	return len(clips), err
}

// diskListResponse renders "disk list" for slot 1: the clip list's files as
// "{id}: {name} {container} {format} {duration}".
func diskListResponse() (string, error) {
	clips, err := deckClipList()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("206 disk list:\r\nslot id: 1\r\n")
	for _, c := range clips {
		container := strings.ToUpper(strings.TrimPrefix(filepath.Ext(c.file), "."))
		container = strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, container)
		if container == "" {
			container = "FILE"
		}
		fmt.Fprintf(&b, "%d: %s %s %s %s\r\n", c.id, c.name, container, deckVideoFormat, timecode(c.duration))
	}
	b.WriteString("\r\n")
	return b.String(), nil
}

// --- MediaPool clip source ----------------------------------------------------
//
// Settings > Network can list the media pool instead of the cue sheet as the
// deck's clips (§12.8): clip ids are 1-based pool positions (newest first,
// the pool's own order) and "play: clip id" plays that file directly.

func deckClipsFromPool() bool {
	return config.Remote().HyperDeckClips == "mediapool"
}

func firePoolFile(filename string) error {
	logs.Printf(logs.RTEDeck, "hyperdeck play pool clip %s", filename)
	gain, err := ctp.MediaLoudnessGain(filename)
	if err != nil {
		return err
	}
	if err := gsp.LoadWithOpts(filename, gsp.LoadOpts{Loop: config.Loop(), LoudnessGain: gain}); err != nil {
		return err
	}
	gsp.Play()
	return nil
}
