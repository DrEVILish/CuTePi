// Package logs provides stable, greppable error codes for the CuTePi log
// output. Codes follow a <SUBSYS>-E<NNN> scheme so a code stays stable across
// releases and can be searched in logs/tickets. The full code-meaning table
// lives in PROJECT.md -> Decisions -> Error codes for logging.
//
// Once assigned, a code must never be reused or renumbered for a different
// operation; add a new code instead.
package logs

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Level is a filter for which log records are recorded (the runtime
// switch in the UI lowers what is written, not just what is displayed).
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	default:
		return "info"
	}
}

func ParseLevel(s string) (Level, bool) {
	switch s {
	case "debug":
		return LevelDebug, true
	case "warn", "error":
		return LevelWarn, true
	case "info":
		return LevelInfo, true
	default:
		return LevelInfo, false
	}
}

// Entry is one buffered log/audit record served to the Web UI viewer.
type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Audit   bool      `json:"audit"` // structured playback event (cue start/stop etc)
}

// AuditEvent is the structured payload for playback audit records, which are
// included in .CTP exports. It stays JSON-only (no log text) so exports carry
// clean, machine-readable show history.
type AuditEvent struct {
	Event string `json:"event"` // e.g. "cue_start", "cue_stop"
	Pos   int    `json:"pos"`   // cue position (0 for transport/source events)
	Title string `json:"title"` // cue / file title
}

const bufferSize = 5000

// auditCapacity caps the in-memory audit trail. A show-day of cue events
// fits easily; beyond that the oldest entries roll off. The trail is a
// process-lifetime record - the buffer ring above is the model to copy.
const auditCapacity = 10000

var (
	mu    sync.Mutex
	level = LevelInfo
	buf   = make([]Entry, 0, bufferSize)
	audit []AuditEvent // full structured audit trail for exports
)

// CurrentLevel returns the current recording level.
func CurrentLevel() Level {
	mu.Lock()
	defer mu.Unlock()
	return level
}

// SetLevel changes the recording level at runtime (writes fewer logs during a
// show). Records at or above the level are kept.
func SetLevel(l Level) {
	mu.Lock()
	defer mu.Unlock()
	level = l
}

// Printf logs a message prefixed with its stable error code, e.g.
// "[GSP-E100] error pausing: <err>". It always writes to the standard log
// output (with the standard timestamp/prefix) and buffers it for the viewer.
func Printf(code, format string, args ...any) {
	record(LevelInfo, code, false, fmt.Sprintf(format, args...))
}

// PrintfWarn logs a warning (level: warn) to stderr and the viewer.
func PrintfWarn(code, format string, args ...any) {
	record(LevelWarn, code, false, fmt.Sprintf(format, args...))
}

// PrintfDebug logs a debug detail (level: debug) to the viewer buffer. Debug
// lines are the first to disappear when the runtime level is raised.
func PrintfDebug(code, format string, args ...any) {
	record(LevelDebug, code, false, fmt.Sprintf(format, args...))
}

// Emit appends a structured audit record (kept for exports) and a buffered log
// entry so the event also appears in the viewer. Oldest audit entries roll
// off once auditCapacity is reached (the slice would otherwise grow for the
// whole process lifetime and be copied on every export).
func Emit(a AuditEvent) {
	mu.Lock()
	if len(audit) >= auditCapacity {
		audit = audit[len(audit)-auditCapacity+1:]
	}
	audit = append(audit, a)
	msg := fmt.Sprintf("%s pos=%d title=%q", a.Event, a.Pos, a.Title)
	mu.Unlock()
	record(LevelInfo, AUDAudit, true, msg)
}

// Recorded returns the buffered entries, most recent last, filtered by the
// current recording level. audit flag records are always included.
func Recorded() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, len(buf))
	for _, e := range buf {
		if entryLevel(e.Level) < level && !e.Audit {
			continue
		}
		out = append(out, e)
	}
	return out
}

// AuditTrail returns the full structured playback audit history.
func AuditTrail() []AuditEvent {
	mu.Lock()
	defer mu.Unlock()
	out := make([]AuditEvent, len(audit))
	copy(out, audit)
	return out
}

// Clear empties the buffered log viewer records (the audit trail is kept; it
// is a show record and is exported, not cleared with the transient viewer).
func Clear() {
	mu.Lock()
	defer mu.Unlock()
	buf = buf[:0]
}

func entryLevel(s string) Level {
	if l, ok := ParseLevel(s); ok {
		return l
	}
	return LevelInfo
}

// record buffers and prints one line. printed Controls whether stdout got it.
func record(l Level, code string, isAudit bool, msg string) {
	mu.Lock()
	if l >= level || isAudit {
		if len(buf) == bufferSize {
			copy(buf, buf[1:])
			buf[0] = Entry{}
			buf = buf[:bufferSize-1]
		}
		buf = append(buf, Entry{Time: time.Now(), Level: l.String(), Code: code, Message: msg, Audit: isAudit})
	}
	mu.Unlock()
	if !isAudit {
		log.Printf("[%s] %s", code, msg)
	}
}

// GSP - GStreamer playback manager (gsp/gsp.go).
const (
	GSPPauseErr    = "GSP-E100" // error pausing pipeline
	GSPToggleErr   = "GSP-E110" // error toggling pause
	GSPStopErr     = "GSP-E120" // error stopping pipeline
	GSPPipeDebug   = "GSP-E130" // GStreamer pipeline error debug string
	GSPPipeStopped = "GSP-E140" // pipeline stopped (end-of-stream or error)
	GSPPadAdded    = "GSP-E150" // stream pad-added detection detail
	GSPWarm        = "GSP-E160" // prewarmed pipeline activated (deck-style cue load)
	GSPFireTiming  = "GSP-E161" // cue fire latency measurement (build/preroll ms)
)

// YDL - YouTube/yt-dlp import stages.
const (
	YDLRequest  = "YDL-E400" // YouTube import request received
	YDLResolve  = "YDL-E410" // output filename resolution
	YDLDownload = "YDL-E420" // media download
	YDLProbe    = "YDL-E430" // downloaded media probe
	YDLRegister = "YDL-E440" // mediapool registration
	YDLRender   = "YDL-E450" // mediapool response rendering
	YDLFailed   = "YDL-E490" // YouTube import failure
	YDLRename   = "YDL-E460" // YouTube download rename (post-import)
)

// RTE - HTTP route actions (routes/api.go).
const (
	RTEPlay       = "RTE-E200" // POST /api/play
	RTEPlayAlias  = "RTE-E201" // POST /api/cue/play (spacebar)
	RTEPause      = "RTE-E202" // POST /api/pause
	RTEToggle     = "RTE-E203" // POST /api/togglePause
	RTEFadeOut    = "RTE-E204" // POST /api/fadeOut
	RTEPanic      = "RTE-E205" // POST /api/panic
	RTEClear      = "RTE-E206" // POST /api/clear (clear cuesheet)
	RTEStop       = "RTE-E209" // POST /api/stop
	RTETest       = "RTE-E210" // POST /api/test/*pattern
	RTEDirect     = "RTE-E211" // POST /api/play/:filename (direct play)
	RTELoad       = "RTE-E212" // POST /api/load/:filename
	RTEAddCue     = "RTE-E213" // POST /api/cue/add
	RTEDelete     = "RTE-E214" // DELETE /api/media/:filename
	RTEDeleteBusy = "RTE-E215" // cannot delete currently playing file
	RTECueNext    = "RTE-E216" // POST /api/cue/next
	RTECuePrev    = "RTE-E217" // POST /api/cue/prev
	RTECuePlay    = "RTE-E218" // POST /api/cue/:cuePos/play
	RTEUp         = "RTE-E219" // POST /api/cue/:cuePos/move/up
	RTEDown       = "RTE-E220" // POST /api/cue/:cuePos/move/down
	RTEUse        = "RTE-E221" // POST /api/cue/:cuePos (selected)
	RTEEdit       = "RTE-E222" // POST /api/cue/:cuePos/edit/:col
	RTEUpdate     = "RTE-E223" // PUT /api/cue/:cuePos/edit/:col
	RTERemove     = "RTE-E224" // DELETE /api/cue/:cuePos
	RTERestart    = "RTE-E225" // POST /api/restart
	RTEShutdown   = "RTE-E226" // POST /api/shutdown
	RTEDeck       = "RTE-E227" // remote control action (HyperDeck/QLab)
	RTEDeckErr    = "RTE-E228" // remote control error
)

// NET - network startup info (network.go).
const (
	NETListErr = "NET-E300" // error fetching network interfaces
)

// AUD - structured playback audit records (also exposed to the Web UI).
const (
	AUDAudit = "AUD-E500" // structured playback audit event (cue start/stop etc)
)
