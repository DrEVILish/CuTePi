package routes

// QLab remote control over OSC: QLab's standard port 53000. The paths
// are the ones QLab documents in its OSC dictionary and the ones Bitfocus's
// QLab Companion modules actually send — workspace-scoped or rootless, so
// both forms land on exactly the same actions the buttons do.
//
// Datagrams arrive as OSC messages (NUL-terminated address, comma typetags,
// big-endian args, each element padded to a 4-byte boundary). Every action
// routes into the same cores the Web UI and the HyperDeck half use.

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// ListenOSC serves OSC datagrams until the socket closes. No-op replies
// are the norm here: controllers fire-and-forget UDP actions.
func ListenOSC(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		logs.Printf(logs.RTEDeckErr, "osc listener: %v", err)
		return
	}
	serveOSC(pc)
}

// startOSC binds synchronously and serves in the background; closing the
// returned socket stops it.
func startOSC(addr string) (io.Closer, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	go serveOSC(pc)
	return pc, nil
}

func serveOSC(pc net.PacketConn) {
	buf := make([]byte, 8192)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if err := handleOSCDgram(buf[:n]); err != nil {
			logs.Printf(logs.RTEDeckErr, "osc %s: %v", from, err)
		}
	}
}

// ListenOSCTCP serves the OSC dictionary over TCP, SLIP-framed (RFC 1055
// END-delimited) as QLab 5 does, for the controllers that want replies —
// Companion's QLab module and QLab Remote. Every message is answered with
// QLab's reply envelope (qlabws.go). Shares the port number with the UDP
// listener (different protocol, no conflict).
func ListenOSCTCP(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logs.Printf(logs.RTEDeckErr, "osc/tcp listener: %v", err)
		return
	}
	serveOSCListener(ln)
}

// startOSCTCP binds synchronously and serves in the background; closing the
// returned closer stops the listener AND hangs up every connected client
// (switching the listener off in Settings must actually cut control).
func startOSCTCP(addr string) (io.Closer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &oscTCPServer{ln: ln, conns: map[net.Conn]bool{}}
	go srv.serve()
	return srv, nil
}

// oscTCPServer is a running OSC/TCP listener plus its live connections.
type oscTCPServer struct {
	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]bool
}

func (s *oscTCPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = true
		s.mu.Unlock()
		go func() {
			serveOSCTCP(conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// Close stops accepting and disconnects every client.
func (s *oscTCPServer) Close() error {
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	return err
}

func serveOSCListener(ln net.Listener) {
	(&oscTCPServer{ln: ln, conns: map[net.Conn]bool{}}).serve()
}

func serveOSCTCP(conn net.Conn) {
	defer conn.Close()
	c := &qlabClient{conn: conn}
	qlabRegister(c)
	defer qlabUnregister(c)
	dec := &slipDecoder{}
	buf := make([]byte, 8192)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			for _, pkt := range dec.feed(buf[:n]) {
				addr, args, derr := oscDecode(pkt)
				if derr != nil || addr == "" {
					logs.Printf(logs.RTEDeckErr, "osc/tcp %s: undecodable packet", conn.RemoteAddr())
					continue
				}
				handleQLab(c, addr, args)
			}
		}
		if err != nil {
			return
		}
	}
}

// SLIP framing: END 0xC0 delimits packets, ESC 0xDB escapes (ESC_END 0xDC,
// ESC_ESC 0xDD). Empty packets (back-to-back ENDs, leading END) carry
// nothing and are dropped.
const (
	slipEND    = 0xC0
	slipESC    = 0xDB
	slipESCEnd = 0xDC
	slipESCEsc = 0xDD
)

// slipMaxPacket caps one decoded packet. OSC commands are tiny; a peer
// streaming bytes without ever sending END would otherwise grow buf forever.
// An oversized packet is discarded up to its closing END.
const slipMaxPacket = 64 << 10

type slipDecoder struct {
	buf      []byte
	esc      bool
	overflow bool // current packet exceeded slipMaxPacket: drop until END
}

// put appends one decoded byte, tripping overflow past the cap.
func (d *slipDecoder) put(b byte) {
	if d.overflow {
		return
	}
	if len(d.buf) >= slipMaxPacket {
		d.overflow = true
		d.buf = nil
		return
	}
	d.buf = append(d.buf, b)
}

func (d *slipDecoder) feed(chunk []byte) [][]byte {
	var out [][]byte
	for _, b := range chunk {
		switch {
		case d.esc:
			d.esc = false
			if b == slipESCEnd {
				d.put(slipEND)
			} else if b == slipESCEsc {
				d.put(slipESC)
			} else {
				d.put(b)
			}
		case b == slipESC:
			d.esc = true
		case b == slipEND:
			if len(d.buf) > 0 && !d.overflow {
				out = append(out, d.buf)
			}
			d.buf = nil
			d.overflow = false
		default:
			d.put(b)
		}
	}
	return out
}

// handleOSCDgram runs one UDP message. UDP senders get no replies (QLab's
// UDP mode is fire-and-forget), but the dictionary is the TCP one.
func handleOSCDgram(data []byte) error {
	addr, args, err := oscDecode(data)
	if err != nil || addr == "" {
		return err
	}
	if _, err := qlabRequest(nil, addr, args); err != nil && err != errQlabUnknown {
		return err
	}
	return nil
}

// oscDecode parses one OSC message (top-level only; QLab control sends
// messages, not bundles). args: int→int, f→float64, s→string, T/F→bool.
func oscDecode(data []byte) (string, []any, error) {
	addr, n := oscString(data)
	if n == 0 {
		return "", nil, nil
	}
	data = data[n:]
	tags, n := oscString(data)
	if n == 0 {
		return "", nil, nil
	}
	data = data[n:]
	var args []any
	i := 1 // tags[0] is ','
	for i < len(tags) {
		switch tags[i] {
		case 'i', 'f', 's':
			if len(data) < 4 {
				return "", nil, nil
			}
			if tags[i] == 'i' {
				args = append(args, int(int32(binary.BigEndian.Uint32(data)))) // OSC 'i' is signed
			} else if tags[i] == 'f' {
				args = append(args, float64(math.Float32frombits(binary.BigEndian.Uint32(data))))
			} else {
				s, n := oscString(data)
				args = append(args, s)
				data = data[n:]
				i++
				continue
			}
			data = data[4:]
		case 'T':
			args = append(args, true)
		case 'F':
			args = append(args, false)
		case 't', 'm':
			return "", nil, nil // timetag/midi: never in what we speak
		default:
			return "", nil, nil // unknown type: don't guess the rest
		}
		i++
	}
	return addr, args, nil
}

// oscString reads one NUL-terminated, 4-byte-padded string.
func oscString(b []byte) (string, int) {
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	size := (i + 4) &^ 3
	if size > len(b) {
		return "", 0
	}
	return string(b[:i]), size
}

// command routes one rootless OSC message (workspace or cue level).
func command(addr string, args []any) {
	if p := qlabPath(addr); strings.HasPrefix(p, "/cue/") || strings.HasPrefix(p, "/cue_id/") {
		if _, err := qlabCueRequest(p, args); err != nil {
			logs.Printf(logs.RTEDeckErr, "osc %s: %v", addr, err)
		}
		return
	}
	switch qlabPath(addr) {
	case "/go":
		logs.Printf(logs.RTEDeck, "osc GO")
		// /go {cue_number}: jump playhead to that cue, then GO.
		if n, ok := argFloat(args, 0); ok {
			if cue, found := qlabResolve(strconv.FormatFloat(n, 'f', -1, 64)); found {
				_ = ctp.SetCue(strconv.Itoa(cue.CuePos))
			}
		}
		_ = FireSelected()
	case "/stop":
		logs.Printf(logs.RTEStop, "STOP (qlab)")
		gsp.Stop()
	case "/pause":
		logs.Printf(logs.RTEPause, "PAUSE (qlab)")
		gsp.Pause()
	case "/resume":
		gsp.Play()
	case "/panic":
		logs.Printf(logs.RTEPanic, "PANIC (qlab)")
		_ = remotePanic()
	case "/reset":
		// QLab's reset: stop everything, playhead to the top of the list.
		_ = remotePanic()
		if err := ctp.NextCue(); err != nil {
			logs.Printf(logs.RTEDeckErr, "osc reset: %v", err)
		}
	case "/next", "/playbackPosition/next", "/select/next":
		_ = ctp.SelectStep(1)
	case "/previous", "/playbackPosition/previous", "/select/previous":
		_ = ctp.SelectStep(-1)
	default:
		// Login/handshake/queries arrive constantly (/connect, /read);
		// replying is unnecessary, so unhandled paths only log traces.
	}
}

// qlabPath: drop the "/workspace/{id}" prefix — CuTePi has one workspace, and
// rootless form plus 'any id' form must behave identically.
func qlabPath(addr string) string {
	parts := strings.SplitN(addr, "/", 4)
	if len(parts) == 4 && parts[0] == "" && parts[1] == "workspace" {
		return "/" + parts[3]
	}
	return addr
}

func argFloat(args []any, i int) (float64, bool) {
	if len(args) <= i {
		return 0, false
	}
	switch v := args[i].(type) {
	case int:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, e := strconv.ParseFloat(v, 64)
		return f, e == nil
	}
	return 0, false
}
