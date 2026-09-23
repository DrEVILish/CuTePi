package routes

// QLab remote control over OSC: QLab's standard UDP port 53000. The paths
// are the ones QLab documents in its OSC dictionary and the ones Bitfocus's
// QLab Companion modules actually send — workspace-scoped or rootless, so
// both forms land on exactly the same actions the buttons do.
//
// Datagrams arrive as OSC messages (NUL-terminated address, comma typetags,
// big-endian args, each element padded to a 4-byte boundary). Every action
// routes into the same cores the Web UI and the HyperDeck half use.

import (
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"strings"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// ListenOSC serves OSC datagrams until the process exits. No-op replies
// are the norm here: controllers fire-and-forget UDP actions.
func ListenOSC(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		logs.Printf(logs.RTEDeckErr, "osc listener: %v", err)
		return
	}
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

func handleOSCDgram(data []byte) error {
	addr, args, err := oscDecode(data)
	if err != nil || addr == "" {
		return err
	}
	command(addr, args)
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
				args = append(args, int(binary.BigEndian.Uint32(data)))
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
	if strings.HasPrefix(addr, "/cue/") {
		cueAction(addr, args)
		return
	}
	switch qlabPath(addr) {
	case "/go":
		logs.Printf(logs.RTEDeck, "osc GO")
		// /go {cue_number}: jump playhead to that cue, then GO.
		if n, ok := argFloat(args, 0); ok {
			if cue, found := cueByNum(strconv.Itoa(int(n))); found {
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

// cueAction: /cue/{number}/{command}. {number} is the cue's human number as
// operators put it on buttons (CueNum), with row position as a fallback.
func cueAction(addr string, args []any) {
	segments := strings.Split(strings.TrimPrefix(qlabPath(addr), "/cue/"), "/")
	if len(segments) < 2 || segments[0] == "" {
		return
	}
	num := segments[0]
	if num == "*" || strings.Contains(num, "*") || num == "selected" || num == "playhead" {
		return // wildcards/multi-target are not mappable onto a transport
	}
	cue, ok := cueByNum(num)
	if !ok {
		return
	}
	switch segments[1] {
	case "start", "go", "load":
		logs.Printf(logs.RTEDeck, "osc start cue %s", num)
		if err := FireCue(strconv.Itoa(cue.CuePos)); err != nil {
			logs.Printf(logs.RTEDeckErr, "osc start cue %s: %v", num, err)
		}
	case "panic", "stop":
		// QLab's /cue/{x}/panic kills only that cue; matching CuTePi, if
		// that specific cue is the one on the board, stop it — with a mild
		// fade when the cue carries a fadeOut to do it with.
		if gsp.CurrentCuePos() != cue.CuePos {
			return
		}
		logs.Printf(logs.RTEDeck, "osc stop cue %s", num)
		if cue.FadeOut > 0 {
			gsp.FadeAndStop(cue.FadeOut)
		} else {
			gsp.Stop()
		}
	case "select":
		logs.Printf(logs.RTEDeck, "osc select cue %s", num)
		if err := ctp.SetCue(strconv.Itoa(cue.CuePos)); err != nil {
			logs.Printf(logs.RTEDeckErr, "osc select cue %s: %v", num, err)
		}
	default:
		// /cue/{x}/load, /formik styles and everything unspoken: ignore.
	}
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
