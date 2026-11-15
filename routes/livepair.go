package routes

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
	"CuTePi/logs"
)

// Pairing with TimerPi (DESIGN §12.14): CuTePi joins a TimerPi's waiting room
// as a screen (POST /api/waiting/register {name, host, token}); an Event
// Technician captures it in TimerPi (Screens -> Waiting), choosing the room;
// CuTePi's poll of /api/waiting/mine then returns the room code, screen name
// and the screen's key. CuTePi adds a live cue for /d/<room>?screen=<name>
// and keeps the key apart, as a stored credential (ctp.SetScreenKey). The
// token is this pairing's own random secret: only it can claim the capture.
// Neither token nor key is logged, broadcast or returned to the browser.

type livePairing struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	Name   string `json:"name"`
	State  string `json:"state"` // waiting, paired, failed, cancelled
	Error  string `json:"error,omitempty"`
	CuePos int    `json:"cuePos,omitempty"`
	stop   chan struct{}
}

var (
	pairMu   sync.Mutex
	pairings = map[string]*livePairing{}
)

// livePairTimeout is how long CuTePi stays in TimerPi's waiting room.
const livePairTimeout = 15 * time.Minute

var pairClient = &http.Client{Timeout: 5 * time.Second}

// timerPiBase validates the TimerPi address an operator typed: an http(s)
// origin (a bare host gets http://), no path, query or credentials.
func timerPiBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", fmt.Errorf("TimerPi address must be like http://timerpi.local or https://host:port")
	}
	return u.Scheme + "://" + u.Host, nil
}

func startLivePair(server, name string) (*livePairing, error) {
	base, err := timerPiBase(server)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = InstanceName()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b[:])
	host := InstanceName()
	body, _ := json.Marshal(map[string]string{"name": name, "host": host, "token": token})
	resp, err := pairClient.Post(base+"/api/waiting/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("TimerPi at %s did not answer: %w", base, err)
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TimerPi refused the registration (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var id [6]byte
	_, _ = rand.Read(id[:])
	p := &livePairing{ID: hex.EncodeToString(id[:]), Server: base, Name: name, State: "waiting", stop: make(chan struct{})}
	pairMu.Lock()
	pairings[p.ID] = p
	pairMu.Unlock()
	logs.Printf(logs.RTEAddCue, "live pairing: waiting in %s's waiting room as %q", base, name)
	goSafe(func() { pollLivePair(p, host, token) })
	return p, nil
}

func pollLivePair(p *livePairing, host, token string) {
	mine := p.Server + "/api/waiting/mine?" + url.Values{"name": {p.Name}, "host": {host}, "token": {token}}.Encode()
	deadline := time.Now().Add(livePairTimeout)
	finish := func(state, errText string, pos int) {
		pairMu.Lock()
		p.State, p.Error, p.CuePos = state, errText, pos
		pairMu.Unlock()
	}
	for time.Now().Before(deadline) {
		select {
		case <-p.stop:
			finish("cancelled", "", 0)
			return
		case <-time.After(2 * time.Second):
		}
		resp, err := pairClient.Get(mine)
		if err != nil {
			continue // TimerPi briefly away: keep waiting
		}
		var got struct {
			Assigned string `json:"assigned"`
			Screen   string `json:"screen"`
			Key      string `json:"key"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&got)
		resp.Body.Close()
		if err != nil || got.Assigned == "" {
			continue
		}
		screen := got.Screen
		if screen == "" {
			screen = p.Name
		}
		page := p.Server + "/d/" + url.PathEscape(got.Assigned) + "?" + url.Values{"screen": {screen}}.Encode()
		pos, err := ctp.AddLiveCue(screen, page)
		if err == nil && got.Key != "" {
			// AddLiveCue stored the clean URL; the key goes beside it.
			if cue, cerr := ctp.GetCue(strconv.Itoa(pos)); cerr == nil {
				err = ctp.SetScreenKey(cue.EndpointURL, got.Key)
			}
		}
		if err != nil {
			finish("failed", err.Error(), 0)
			return
		}
		logs.Printf(logs.RTEAddCue, "live pairing: captured into room %s as %q (screen key stored) -> cue %d", got.Assigned, screen, pos)
		awardsSelectionSync()
		ctp.NotifyCuesheetChanged()
		finish("paired", "", pos)
		return
	}
	finish("failed", "nobody captured this screen in TimerPi within 15 minutes", 0)
}

func registerLivePairRoutes(rg *gin.RouterGroup) {
	rg.POST("/live/pair", func(c *gin.Context) {
		p, err := startLivePair(c.PostForm("server"), c.PostForm("name"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": redactPaths(err.Error())})
			return
		}
		c.JSON(http.StatusOK, p)
	})
	rg.GET("/live/pair/:id", func(c *gin.Context) {
		pairMu.Lock()
		p, ok := pairings[c.Param("id")]
		var out livePairing
		if ok {
			out = *p
		}
		pairMu.Unlock()
		if !ok {
			respondError(c, http.StatusNotFound, "no such pairing")
			return
		}
		c.JSON(http.StatusOK, out)
	})
	rg.DELETE("/live/pair/:id", func(c *gin.Context) {
		pairMu.Lock()
		p, ok := pairings[c.Param("id")]
		if ok && p.State == "waiting" {
			close(p.stop)
			p.State = "cancelling"
		}
		pairMu.Unlock()
		c.Status(http.StatusNoContent)
	})
	// Forget a live cue's stored screen key (re-pair to get a new one).
	rg.POST("/cue/:cuePos/screenkey/forget", func(c *gin.Context) {
		cue, err := ctp.GetCue(c.Param("cuePos"))
		if err != nil || cue.SourceKind != "endpoint" {
			respondError(c, http.StatusNotFound, "not a live page cue")
			return
		}
		if err := ctp.SetScreenKey(cue.EndpointURL, ""); err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		logs.Printf(logs.RTEUpdate, "live cue %d: screen key forgotten", cue.CuePos)
		c.Status(http.StatusNoContent)
	})
}

// parseStartAt reads a start instant given as unix milliseconds ("" = none).
// It must lie within the next minute.
func parseStartAt(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("at must be unix milliseconds")
	}
	t := time.UnixMilli(ms)
	if d := time.Until(t); d < -time.Second || d > time.Minute {
		return time.Time{}, fmt.Errorf("at must be within the next minute (is %v away)", d.Round(time.Millisecond))
	}
	return t, nil
}
