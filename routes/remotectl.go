package routes

// Remote listener lifecycle (§12.8): HyperDeck, OSC UDP and OSC TCP/SLIP
// each run only while their Settings Network toggle is on — all off by
// default, since neither protocol authenticates. ApplyRemote reconciles the
// running listeners with config.Remote() at startup and after every settings
// save; RemoteStatus feeds the Network tab's live on/off list.

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"CuTePi/config"
	"CuTePi/logs"
)

// remoteListener is one bound listener (closer != nil) or a failed bind
// (err != ""), keyed by protocol id.
type remoteListener struct {
	addr   string
	closer io.Closer
	err    string
}

var (
	remoteMu  sync.Mutex
	remoteRun = map[string]*remoteListener{}
)

// remoteProtocols is the fixed display order of the Network tab list.
var remoteProtocols = []struct{ ID, Label string }{
	{"hyperdeck", "HyperDeck (TCP)"},
	{"osc_udp", "OSC (UDP)"},
	{"osc_tcp", "OSC (TCP/SLIP)"},
}

// remoteWanted maps each enabled protocol to its bind address.
func remoteWanted(r config.RemoteSettings) map[string]string {
	want := map[string]string{}
	if r.HyperDeck {
		// Same bind address as OSC: the one setting pins every
		// unauthenticated listener to a chosen interface.
		want["hyperdeck"] = net.JoinHostPort(r.OSCBind, strconv.Itoa(r.HyperDeckPort))
	}
	if r.OSCUDP {
		want["osc_udp"] = net.JoinHostPort(r.OSCBind, strconv.Itoa(r.OSCUDPPort))
	}
	if r.OSCTCP {
		want["osc_tcp"] = net.JoinHostPort(r.OSCBind, strconv.Itoa(r.OSCTCPPort))
	}
	return want
}

// ApplyRemote starts, stops or rebinds listeners so the running set matches
// the saved settings. A failed bind is logged and reported in RemoteStatus;
// the next apply retries it. Losing a control listener never stops the show
// controller.
func ApplyRemote() {
	want := remoteWanted(config.Remote())
	remoteMu.Lock()
	defer remoteMu.Unlock()
	for id, l := range remoteRun {
		if addr, ok := want[id]; ok && addr == l.addr && l.closer != nil {
			continue
		}
		stopRemoteLocked(id, l)
	}
	for id, addr := range want {
		if _, running := remoteRun[id]; running {
			continue
		}
		closer, err := startRemote(id, addr)
		l := &remoteListener{addr: addr, closer: closer}
		if err != nil {
			l.err = err.Error()
			logs.Printf(logs.RTEDeckErr, "%s listener %s: %v", id, addr, err)
		} else {
			logs.Printf(logs.RTEDeck, "%s listener on %s", id, addr)
		}
		remoteRun[id] = l
	}
}

func stopRemoteLocked(id string, l *remoteListener) {
	if l.closer != nil {
		_ = l.closer.Close()
		logs.Printf(logs.RTEDeck, "%s listener on %s stopped", id, l.addr)
	}
	if id == "hyperdeck" {
		deckDropAll()
	}
	delete(remoteRun, id)
}

func startRemote(id, addr string) (io.Closer, error) {
	switch id {
	case "hyperdeck":
		return startHyperdeck(addr)
	case "osc_udp":
		return startOSC(addr)
	case "osc_tcp":
		return startOSCTCP(addr)
	}
	return nil, fmt.Errorf("unknown remote protocol %q", id)
}

// RemoteStatusEntry is one row of the Network tab's live server list.
type RemoteStatusEntry struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Addr    string `json:"addr"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

// RemoteStatus reports every protocol's configured and live state.
func RemoteStatus() []RemoteStatusEntry {
	want := remoteWanted(config.Remote())
	remoteMu.Lock()
	defer remoteMu.Unlock()
	out := make([]RemoteStatusEntry, 0, len(remoteProtocols))
	for _, p := range remoteProtocols {
		e := RemoteStatusEntry{ID: p.ID, Label: p.Label, Addr: want[p.ID]}
		_, e.Enabled = want[p.ID]
		if l, ok := remoteRun[p.ID]; ok {
			e.Addr = l.addr
			e.Running = l.closer != nil
			e.Error = l.err
		}
		out = append(out, e)
	}
	return out
}
