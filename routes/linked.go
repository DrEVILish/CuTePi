package routes

import (
	"net/http"
	"net/url"
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
)

// Linked units, Synced playback (DESIGN §6.8c): a cue's play button fires
// the same cue number on every linked unit for one shared instant a little
// ahead, and here at that instant too, so all the walls start together (to
// their clock sync, plus at most one refresh); GO fires each unit's own
// selected cue (linkMirrorGo). A unit fired by a link does not pass it on.

// The shared instant of a linked fire is linkLead() ahead (DESIGN §6.8c):
// far enough for the slowest unit to get the request (the slowest recent
// hello round trip) and still make a timed start after loading the cue (the
// slowest unit's recent load time, plus gsp's floors), and no further.
const (
	linkPrepDefault = 250.0 // ms, a unit's load time until it has fired
	linkLeadMargin  = 10.0  // ms
	// A slower unit starts late (and logs it) rather than every GO waiting on it.
	linkLeadMax = 500 * time.Millisecond
)

// prep holds this unit's last cue load times (ms), newest at prep.ms[prep.n%5].
var prep struct {
	sync.Mutex
	ms [5]float64
	n  int
}

// notePrep records a cue load that started at t0 (defer notePrep(time.Now())).
func notePrep(t0 time.Time) {
	prep.Lock()
	prep.ms[prep.n%len(prep.ms)] = float64(time.Since(t0)) / 1e6
	prep.n++
	prep.Unlock()
}

// prepMs is the median of the last load times: one cold load does not
// stretch every later fire.
func prepMs() float64 {
	prep.Lock()
	defer prep.Unlock()
	if prep.n == 0 {
		return linkPrepDefault
	}
	v := append([]float64(nil), prep.ms[:min(prep.n, len(prep.ms))]...)
	sort.Float64s(v)
	return v[len(v)/2]
}

// leadFor is the lead for a slowest round trip rtt and load time prep (ms):
// a peer arms the instant on arrival (gsp.StartAt needs MinStartLead) and
// applies it after loading (applyStartAt needs MinStartLead/2 left).
func leadFor(rtt, prep float64) time.Duration {
	floor := float64(gsp.MinStartLead) / 1e6
	d := time.Duration((rtt + max(floor, prep+floor/2) + linkLeadMargin) * 1e6)
	return min(d, linkLeadMax)
}

// linkLead is the current lead over the units that answer.
func linkLead() time.Duration {
	p := prepMs()
	link.mu.Lock()
	rtt := 0.0
	for _, st := range link.peers {
		if time.Since(st.seen) < linkDeadAfter {
			rtt, p = max(rtt, st.slowMs), max(p, st.hello.PrepMs)
		}
	}
	link.mu.Unlock()
	return leadFor(rtt, p)
}

// fireLinked (Synced playback) fires the cue numbered like cue on every
// linked unit and returns the shared instant (zero when no unit is linked or
// the cue has no number to match on).
func fireLinked(cue ctp.Cue) time.Time {
	if linkMode() != linkModeSynced || strings.TrimSpace(cue.CueNum) == "" {
		return time.Time{}
	}
	link.mu.Lock()
	n := len(link.peers)
	link.mu.Unlock()
	if n == 0 {
		return time.Time{}
	}
	at := time.Now().Add(linkLead())
	enqueue(nil, http.MethodPost, "/api/cue/bynum/"+url.PathEscape(cue.CueNum)+"/play?linked=1&at="+strconv.FormatInt(at.UnixMilli(), 10), "", nil)
	return at
}

// redactURLErr keeps a peer's password out of an error's text.
func redactURLErr(err error, peer string) string {
	return strings.ReplaceAll(err.Error(), peer, config.RedactURL(peer))
}

// armLinkedStart fires the linked units for cue (unless this fire came from
// a link) and arms the same instant here.
func armLinkedStart(cue ctp.Cue, fromLink bool) {
	if fromLink {
		return
	}
	if at := fireLinked(cue); !at.IsZero() {
		gsp.StartAt(at)
	}
}

func registerLinkedRoutes(rg *gin.RouterGroup) {
	// A linked unit's GO: fire the cue numbered :num at ?at.
	rg.POST("/cue/bynum/:num/play", func(c *gin.Context) {
		at, err := parseStartAt(c.Query("at"))
		if err != nil {
			respondError(c, http.StatusBadRequest, err.Error())
			return
		}
		sheet, err := ctp.GetCuesheet()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		num := c.Param("num")
		for _, cue := range sheet.Cues {
			if cue.CueNum == num {
				logs.Printf(logs.RTECuePlay, "Play Cue%d (cue %s, linked)", cue.CuePos, num)
				// Answer at once: the leader must not wait on this unit's
				// load (its own start is already armed for the instant).
				pos := strconv.Itoa(cue.CuePos)
				goSafe(func() {
					gsp.StartAt(at)
					if err := FireCue(pos); err != nil {
						logs.PrintfWarn(logs.RTECuePlay, "linked fire of cue %s: %v", num, err)
					}
				})
				c.Status(http.StatusAccepted)
				return
			}
		}
		respondError(c, http.StatusNotFound, "no cue numbered "+num)
	})
}
