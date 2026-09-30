package routes

// QLab 5 workspace emulation for OSC over TCP (§12.8). Controllers that
// speak QLab's reply protocol — Companion's QLab module (figure53-qlab-
// advance) and QLab Remote — connect to TCP 53000, ask for /version,
// /workspaces, /connect, /updates and /cueLists, and then drive the show by
// cue number or unique ID while polling the running cue.
//
// CuTePi answers as QLab 5 with one workspace holding one cue list, the cue
// sheet: every sheet row is a cue (unique ID from its database id, number
// from its cue number, type Video or Audio). Every message over TCP gets a
// "/reply{address}" message carrying QLab's JSON envelope
// {workspace_id, address, status, data}; unknown addresses answer
// status "error", as QLab does. A client that sent "/updates 1" also gets
// QLab's push messages: /update/workspace/{id}/cue_id/{cue} when a cue's
// state changes and .../cueList/{list}/playbackPosition when the playhead
// (the selected cue) moves.
//
// There is no passcode (the listeners are unauthenticated LAN features, see
// remote.go), so /connect always grants view, edit and control.

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

const qlabVersion = "5.4.0"

// qlabWriteTimeout bounds one write to a QLab client.
const qlabWriteTimeout = 2 * time.Second

// qlabClient is one OSC/TCP connection.
type qlabClient struct {
	conn    net.Conn
	wmu     sync.Mutex
	updates atomic.Bool
}

var (
	qlabRegMu   sync.RWMutex
	qlabClients = map[*qlabClient]bool{}
	qlabLoop    sync.Once
)

func qlabRegister(c *qlabClient) {
	qlabLoop.Do(func() { go qlabUpdateLoop() })
	qlabRegMu.Lock()
	qlabClients[c] = true
	qlabRegMu.Unlock()
}

func qlabUnregister(c *qlabClient) {
	qlabRegMu.Lock()
	delete(qlabClients, c)
	qlabRegMu.Unlock()
}

// send writes one OSC message, SLIP-framed (END before and after, as QLab
// and osc.js do). A failed write closes the connection.
func (c *qlabClient) send(addr string, args ...any) bool {
	pkt := slipEncode(oscEncodeMsg(addr, args...))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(qlabWriteTimeout))
	if _, err := c.conn.Write(pkt); err != nil {
		_ = c.conn.Close()
		return false
	}
	return true
}

// oscEncodeMsg builds one OSC message: string, int32, float32 and bool args.
func oscEncodeMsg(addr string, args ...any) []byte {
	pad := func(b []byte, s string) []byte {
		b = append(b, s...)
		n := 4 - len(s)%4
		return append(b, make([]byte, n)...)
	}
	tags := ","
	var body []byte
	for _, a := range args {
		switch v := a.(type) {
		case string:
			tags += "s"
			body = pad(body, v)
		case int:
			tags += "i"
			body = binary.BigEndian.AppendUint32(body, uint32(int32(v)))
		case float64:
			tags += "f"
			body = binary.BigEndian.AppendUint32(body, math.Float32bits(float32(v)))
		case bool:
			if v {
				tags += "T"
			} else {
				tags += "F"
			}
		}
	}
	out := pad(nil, addr)
	out = pad(out, tags)
	return append(out, body...)
}

func slipEncode(pkt []byte) []byte {
	out := make([]byte, 0, len(pkt)+8)
	out = append(out, slipEND)
	for _, b := range pkt {
		switch b {
		case slipEND:
			out = append(out, slipESC, slipESCEnd)
		case slipESC:
			out = append(out, slipESC, slipESCEsc)
		default:
			out = append(out, b)
		}
	}
	return append(out, slipEND)
}

// --- identity -------------------------------------------------------------------

var qlabIDs = sync.OnceValues(func() (string, string) {
	name, _ := os.Hostname()
	uuid := func(seed string) string {
		h := sha1.Sum([]byte(seed))
		return fmt.Sprintf("%X-%X-%X-%X-%X", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
	}
	return uuid("cutepi-workspace/" + name), uuid("cutepi-cuelist/" + name)
})

func qlabWorkspaceID() string { ws, _ := qlabIDs(); return ws }
func qlabCueListID() string   { _, cl := qlabIDs(); return cl }

func qlabWorkspaceName() string {
	name, _ := os.Hostname()
	if name == "" {
		return "CuTePi"
	}
	return name
}

// qlabCueID is a cue's stable unique ID (its database id).
func qlabCueID(c ctp.Cue) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", c.Cue_id)
}

// qlabCueByID resolves a unique ID from qlabCueID.
func qlabCueByID(id string) (ctp.Cue, bool) {
	const prefix = "00000000-0000-4000-8000-"
	if !strings.HasPrefix(id, prefix) {
		return ctp.Cue{}, false
	}
	n, err := strconv.Atoi(strings.TrimLeft(strings.TrimPrefix(id, prefix), "0"))
	if err != nil {
		return ctp.Cue{}, false
	}
	cues, err := ctp.GetCuesheet()
	if err != nil {
		return ctp.Cue{}, false
	}
	for _, c := range cues.Cues {
		if c.Cue_id == n {
			return c, true
		}
	}
	return ctp.Cue{}, false
}

// qlabNumber is the cue number QLab clients see and address: the cue's
// number when it has one, else its sheet position.
func qlabNumber(c ctp.Cue) string {
	if c.CueNum != "" {
		return c.CueNum
	}
	return strconv.Itoa(c.CuePos)
}

// qlabColorNames maps the cue palette onto QLab colour names.
var qlabColorNames = map[string]string{
	"#dc3545": "red", "#fd7e14": "orange", "#ffc107": "yellow", "#28a745": "green",
	"#20c997": "seafoam green", "#17a2b8": "cyan", "#007bff": "blue", "#6610f2": "indigo",
	"#a855f7": "purple", "#e83e8c": "hot pink", "#795548": "wenge", "#6c757d": "grey",
}

// --- cue objects --------------------------------------------------------------------

// qlabCue renders a cue as QLab's cue dictionary (the keys the controllers
// request with valuesForKeys, which are also sent in lists).
func qlabCue(c ctp.Cue) map[string]any {
	typ := "Video"
	if strings.HasPrefix(c.Mimetype, "audio/") {
		typ = "Audio"
	}
	name := c.Title
	if name == "" {
		name = c.Filename
	}
	color := qlabColorNames[strings.ToLower(c.Color)]
	if color == "" {
		color = "none"
	}
	dur := float64(ctp.EffectiveCueDuration(c)) / 1000
	current := gsp.CurrentCuePos() == c.CuePos && gsp.CurrentPlaying() != ""
	// A clip parked on its last frame (a still, or hold at end) is still a
	// running cue in QLab's terms; only an operator pause is isPaused.
	paused := current && gsp.IsPaused() && !gsp.HeldAtEnd()
	elapsed, pct := 0.0, 0.0
	if current {
		// Progress through the cue's trimmed span, in cue (wall) time.
		in := float64(c.PosStart) / 1000
		span := gsp.CurrentDuration() - in
		if c.PosEnd > c.PosStart {
			span = float64(c.PosEnd-c.PosStart) / 1000
		}
		if span > 0 {
			pct = math.Max(0, math.Min(1, (gsp.CurrentPosition()-in)/span))
		}
		elapsed = pct * dur
	}
	continueMode := 0
	if c.AutoContinue {
		continueMode = 2 // auto-follow: the next cue starts when this one ends
	}
	return map[string]any{
		"uniqueID":             qlabCueID(c),
		"number":               qlabNumber(c),
		"name":                 name,
		"listName":             name,
		"type":                 typ,
		"colorName":            color,
		"flagged":              false,
		"armed":                true,
		"notes":                "",
		"mode":                 0,
		"isRunning":            current && !paused,
		"isPaused":             paused,
		"isLoaded":             false,
		"isBroken":             c.Missing,
		"isAuditioning":        false,
		"duration":             dur,
		"actionElapsed":        elapsed,
		"percentActionElapsed": pct,
		"preWait":              float64(c.PreWait) / 1000,
		"postWait":             float64(c.PostWait) / 1000,
		"parent":               qlabCueListID(),
		"autoLoad":             false,
		"continueMode":         continueMode,
		"cartPosition":         []int{0, 0},
		"infiniteLoop":         c.Loop && c.LoopCount == 0,
		"holdLastFrame":        c.Hold,
	}
}

// qlabCueList renders the workspace's one cue list with its cues.
func qlabCueList(cues []ctp.Cue) map[string]any {
	children := make([]map[string]any, 0, len(cues))
	for _, c := range cues {
		q := qlabCue(c)
		// Companion's QLab module (2.14) dereferences its previous copy of a
		// paused cue, which doesn't exist while it loads the list, and
		// throws. Lists never carry the pause; the module's follow-up
		// /cue/active/valuesForKeys does.
		q["isPaused"] = false
		children = append(children, q)
	}
	return map[string]any{
		"uniqueID":             qlabCueListID(),
		"number":               "",
		"name":                 "Main Cue List",
		"listName":             "Main Cue List",
		"type":                 "Cue List",
		"colorName":            "none",
		"flagged":              false,
		"armed":                true,
		"notes":                "",
		"mode":                 0,
		"isRunning":            gsp.CurrentPlaying() != "" && !(gsp.IsPaused() && !gsp.HeldAtEnd()),
		"isPaused":             gsp.CurrentPlaying() != "" && gsp.IsPaused() && !gsp.HeldAtEnd(),
		"isLoaded":             false,
		"isBroken":             false,
		"isAuditioning":        false,
		"duration":             0.0,
		"actionElapsed":        0.0,
		"percentActionElapsed": 0.0,
		"preWait":              0.0,
		"postWait":             0.0,
		"parent":               "",
		"autoLoad":             false,
		"continueMode":         0,
		"cartPosition":         []int{0, 0},
		"infiniteLoop":         false,
		"holdLastFrame":        false,
		"cues":                 children,
	}
}

// --- request handling -----------------------------------------------------------------

// qlabReply sends QLab's reply envelope for addr.
func (c *qlabClient) reply(addr, status string, data any) {
	env := map[string]any{"address": addr, "status": status}
	if strings.HasPrefix(addr, "/workspace/") || !qlabAppLevel(addr) {
		env["workspace_id"] = qlabWorkspaceID()
	}
	if data != nil {
		env["data"] = data
	}
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	c.send("/reply"+addr, string(b))
}

// qlabAppLevel lists the application-scoped (not workspace) addresses.
func qlabAppLevel(addr string) bool {
	switch addr {
	case "/version", "/workspaces", "/alwaysAudition", "/auditionMonitors",
		"/overrideWindow", "/liveFadePreview", "/auditionWindow", "/thump":
		return true
	}
	return strings.HasPrefix(addr, "/overrides/")
}

// errQlabUnknown marks an address this workspace doesn't answer.
var errQlabUnknown = fmt.Errorf("unknown address")

// handleQLab answers one message from a TCP client and runs its action.
func handleQLab(c *qlabClient, addr string, args []any) {
	data, err := qlabRequest(c, addr, args)
	if err != nil {
		logs.Printf(logs.RTEDeck, "qlab %s: %v", addr, err)
		c.reply(addr, "error", nil)
		return
	}
	c.reply(addr, "ok", data)
}

// qlabRequest runs one message and returns its reply data; c is nil for UDP.
func qlabRequest(c *qlabClient, addr string, args []any) (any, error) {
	path := qlabPath(addr)
	switch path {
	case "/version":
		return qlabVersion, nil
	case "/workspaces":
		return []map[string]any{{
			"uniqueID":     qlabWorkspaceID(),
			"displayName":  qlabWorkspaceName(),
			"hasPasscode":  false,
			"version":      qlabVersion,
			"port":         53000,
			"udpReplyPort": 53001,
		}}, nil
	case "/thump":
		return "thump", nil
	case "/connect":
		return "ok:view|edit|control", nil
	case "/disconnect":
		if c != nil {
			c.updates.Store(false)
		}
		return nil, nil
	case "/updates":
		if n, ok := argFloat(args, 0); ok && c != nil {
			c.updates.Store(n != 0)
			logs.Printf(logs.RTEDeck, "qlab updates %v", n != 0)
		}
		return nil, nil
	case "/alwaysAudition", "/auditionMonitors", "/overrideWindow", "/liveFadePreview", "/auditionWindow":
		return false, nil
	case "/showMode":
		return true, nil
	case "/settings/general/minGoTime", "/doubleGoWindowRemaining":
		return 0.0, nil
	case "/cueLists", "/cueLists/shallow":
		cues, err := ctp.GetCuesheet()
		if err != nil {
			return nil, err
		}
		return []map[string]any{qlabCueList(cues.Cues)}, nil
	case "/selectedCues", "/selectedCues/shallow":
		if cue, ok := qlabSelected(); ok {
			return []map[string]any{qlabCue(cue)}, nil
		}
		return []map[string]any{}, nil
	case "/runningCues", "/runningCues/shallow", "/runningOrPausedCues", "/runningOrPausedCues/shallow":
		if cue, ok := qlabActive(); ok {
			return []map[string]any{qlabCue(cue)}, nil
		}
		return []map[string]any{}, nil
	case "/playheadID", "/playheadId":
		if cue, ok := qlabSelected(); ok {
			return qlabCueID(cue), nil
		}
		return "none", nil
	case "/playhead/next", "/playhead/nextSequence", "/select/next":
		return nil, ctp.SelectStep(1)
	case "/playhead/previous", "/playhead/previousSequence", "/select/previous":
		return nil, ctp.SelectStep(-1)
	case "/go", "/stop", "/pause", "/resume", "/panic", "/reset", "/next", "/previous",
		"/playbackPosition/next", "/playbackPosition/previous":
		command(path, args)
		return nil, nil
	case "/panicInTime":
		qlabPanicInTime(args)
		return nil, nil
	case "/auditionGo", "/togglePause":
		if path == "/togglePause" {
			qlabTogglePause()
		}
		return nil, nil
	}
	if strings.HasPrefix(path, "/overrides/") {
		return true, nil // every output enabled
	}
	for _, p := range []string{"/playheadID/", "/playheadId/", "/playhead/", "/select/", "/selectedCues/"} {
		if strings.HasPrefix(path, p) {
			return qlabSetPlayhead(strings.TrimPrefix(path, p))
		}
	}
	if strings.HasPrefix(path, "/cue/") || strings.HasPrefix(path, "/cue_id/") {
		return qlabCueRequest(path, args)
	}
	return nil, errQlabUnknown
}

// qlabSelected is the playhead: CuTePi's selected cue.
func qlabSelected() (ctp.Cue, bool) {
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		return ctp.Cue{}, false
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	return cue, err == nil
}

// qlabActive is the cue on the board (running or paused).
func qlabActive() (ctp.Cue, bool) {
	pos := gsp.CurrentCuePos()
	if pos == 0 || gsp.CurrentPlaying() == "" {
		return ctp.Cue{}, false
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	return cue, err == nil
}

// qlabResolve finds the cue by number (QLab's /cue/{number}), falling back to
// the sheet position for cues without a number.
func qlabResolve(num string) (ctp.Cue, bool) {
	cues, err := ctp.GetCuesheet()
	if err != nil {
		return ctp.Cue{}, false
	}
	for _, c := range cues.Cues {
		if c.CueNum != "" && c.CueNum == num {
			return c, true
		}
	}
	for _, c := range cues.Cues {
		if c.CueNum == "" && strconv.Itoa(c.CuePos) == num {
			return c, true
		}
	}
	return ctp.Cue{}, false
}

func qlabSetPlayhead(ref string) (any, error) {
	cue, ok := qlabCueByID(ref)
	if !ok {
		cue, ok = qlabResolve(ref)
	}
	if !ok {
		return nil, fmt.Errorf("no cue %q", ref)
	}
	return nil, ctp.SetCue(strconv.Itoa(cue.CuePos))
}

// qlabCueRequest: /cue/{number|selected|playhead|active}/{command} and
// /cue_id/{id}/{command}. The cue list itself answers /cue_id/{list}/...
func qlabCueRequest(path string, args []any) (any, error) {
	byID := strings.HasPrefix(path, "/cue_id/")
	rest := strings.TrimPrefix(strings.TrimPrefix(path, "/cue_id/"), "/cue/")
	target, cmd, _ := strings.Cut(rest, "/")
	if byID && target == qlabCueListID() {
		return qlabCueListRequest(cmd, args)
	}
	var cue ctp.Cue
	var ok bool
	switch {
	case target == "selected" || target == "playhead":
		cue, ok = qlabSelected()
	case target == "active":
		cue, ok = qlabActive()
	case byID:
		cue, ok = qlabCueByID(target)
	default:
		cue, ok = qlabResolve(target)
	}
	if !ok {
		return nil, fmt.Errorf("no cue %q", target)
	}
	pos := strconv.Itoa(cue.CuePos)
	current := gsp.CurrentCuePos() == cue.CuePos && gsp.CurrentPlaying() != ""
	switch cmd {
	case "valuesForKeys", "valuesForKeysWithArguments":
		return qlabCue(cue), nil
	case "children", "children/shallow", "children/uniqueIDs", "children/uniqueIDs/shallow":
		return []any{}, nil
	case "start":
		logs.Printf(logs.RTEDeck, "qlab start cue %s", target)
		return nil, FireCue(pos)
	case "go":
		logs.Printf(logs.RTEDeck, "qlab go cue %s", target)
		if err := ctp.SetCue(pos); err != nil {
			return nil, err
		}
		return nil, FireSelected()
	case "stop", "panic", "hardStop":
		if current {
			logs.Printf(logs.RTEStop, "STOP cue %s (qlab)", target)
			if cmd == "stop" && cue.FadeOut > 0 {
				gsp.FadeAndStop(cue.FadeOut)
			} else {
				gsp.Stop()
			}
		}
		return nil, nil
	case "panicInTime":
		if current {
			qlabPanicInTime(args)
		}
		return nil, nil
	case "pause":
		if current {
			gsp.Pause()
		}
		return nil, nil
	case "resume":
		if current {
			gsp.Play()
		}
		return nil, nil
	case "togglePause":
		if current {
			qlabTogglePause()
		}
		return nil, nil
	case "select", "playhead":
		return nil, ctp.SetCue(pos)
	case "load", "preview", "audition", "auditionPreview", "reset":
		return nil, nil // nothing to prepare: cues load when they start
	}
	// Property reads: /cue/{x}/{key} answers that key of the cue dictionary.
	if len(args) == 0 {
		if v, found := qlabCue(cue)[cmd]; found {
			return v, nil
		}
	}
	return nil, errQlabUnknown
}

// qlabCueListRequest: the cue list as a cue (playback commands act on the
// whole show, as QLab's cue-list commands do).
func qlabCueListRequest(cmd string, args []any) (any, error) {
	switch cmd {
	case "valuesForKeys", "valuesForKeysWithArguments":
		cues, err := ctp.GetCuesheet()
		if err != nil {
			return nil, err
		}
		l := qlabCueList(cues.Cues)
		delete(l, "cues")
		return l, nil
	case "children", "children/shallow":
		cues, err := ctp.GetCuesheet()
		if err != nil {
			return nil, err
		}
		return qlabCueList(cues.Cues)["cues"], nil
	case "playheadID", "playheadId":
		if cue, ok := qlabSelected(); ok {
			return qlabCueID(cue), nil
		}
		return "none", nil
	case "go", "next", "previous", "panic", "reset", "stop", "pause", "resume":
		command("/"+cmd, args)
		return nil, nil
	case "panicInTime":
		qlabPanicInTime(args)
		return nil, nil
	case "togglePause":
		qlabTogglePause()
		return nil, nil
	}
	for _, p := range []string{"playheadID/", "playheadId/", "playhead/"} {
		if strings.HasPrefix(cmd, p) {
			return qlabSetPlayhead(strings.TrimPrefix(cmd, p))
		}
	}
	return nil, errQlabUnknown
}

// qlabPanicInTime stops over the given seconds (a fade-out), else now.
func qlabPanicInTime(args []any) {
	logs.Printf(logs.RTEPanic, "PANIC in time (qlab)")
	if s, ok := argFloat(args, 0); ok && s > 0 {
		gsp.FadeAndStop(int(s * 1000))
		return
	}
	_ = remotePanic()
}

func qlabTogglePause() {
	if gsp.CurrentPlaying() == "" {
		return
	}
	if gsp.IsPaused() {
		gsp.Play()
	} else {
		gsp.Pause()
	}
}

// --- updates -------------------------------------------------------------------------

func qlabSubscribers() []*qlabClient {
	qlabRegMu.RLock()
	defer qlabRegMu.RUnlock()
	var out []*qlabClient
	for c := range qlabClients {
		if c.updates.Load() {
			out = append(out, c)
		}
	}
	return out
}

// qlabCueSig is what a controller shows of a cue apart from its run state:
// a change pushes an update for that cue.
func qlabCueSig(c ctp.Cue) string {
	q := qlabCue(c)
	return fmt.Sprint(q["number"], "|", q["name"], "|", q["type"], "|", q["colorName"], "|",
		q["duration"], "|", q["preWait"], "|", q["postWait"], "|", q["continueMode"], "|",
		q["infiniteLoop"], "|", q["isBroken"])
}

// qlabUpdateLoop pushes QLab's update messages to subscribed clients: cue
// state changes (the cue that was and the cue that is on the board), playhead
// moves, and cue sheet edits (the cues whose content changed, plus the list
// when cues were added, removed or reordered).
func qlabUpdateLoop() {
	var lastState, lastSheet uint64
	lastActive, lastPlayhead, lastOrder := "", "", ""
	sigs := map[string]string{}
	tick := 0
	for {
		time.Sleep(100 * time.Millisecond)
		subs := qlabSubscribers()
		if len(subs) == 0 {
			lastState, lastSheet, lastActive, lastPlayhead, lastOrder = 0, 0, "", "", ""
			clear(sigs)
			continue
		}
		ws := "/update/workspace/" + qlabWorkspaceID()
		var msgs []string
		var playhead []any
		if v := ctp.CuesheetVersion(); v != lastSheet {
			first := lastSheet == 0
			lastSheet = v
			if cues, err := ctp.GetCuesheet(); err == nil {
				var order strings.Builder
				seen := map[string]bool{}
				for _, c := range cues.Cues {
					id := qlabCueID(c)
					order.WriteString(id)
					seen[id] = true
					if sig := qlabCueSig(c); sigs[id] != sig {
						if !first {
							msgs = append(msgs, ws+"/cue_id/"+id)
						}
						sigs[id] = sig
					}
				}
				for id := range sigs {
					if !seen[id] {
						delete(sigs, id)
					}
				}
				if o := order.String(); o != lastOrder {
					if !first {
						msgs = append(msgs, ws+"/cue_id/"+qlabCueListID())
					}
					lastOrder = o
				}
			}
		}
		if tick++; tick%3 == 0 || lastPlayhead == "" {
			id := "none"
			if cue, ok := qlabSelected(); ok {
				id = qlabCueID(cue)
			}
			if id != lastPlayhead {
				if lastPlayhead != "" {
					playhead = []any{id}
				}
				lastPlayhead = id
			}
		}
		if v := gsp.StateVersion(); v != lastState {
			lastState = v
			active := ""
			if cue, ok := qlabActive(); ok {
				active = qlabCueID(cue)
			}
			for _, id := range []string{lastActive, active} {
				if id != "" && !slices.Contains(msgs, ws+"/cue_id/"+id) {
					msgs = append(msgs, ws+"/cue_id/"+id)
				}
			}
			lastActive = active
		}
		if len(msgs) == 0 && playhead == nil {
			continue
		}
		for _, c := range subs {
			for _, m := range msgs {
				if !c.send(m) {
					break
				}
			}
			if playhead != nil {
				c.send(ws+"/cueList/"+qlabCueListID()+"/playbackPosition", playhead...)
			}
		}
	}
}
