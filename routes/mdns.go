package routes

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"

	"CuTePi/logs"
)

// Discovery (DESIGN §6.8c): each unit advertises itself as _cutepi._tcp
// (TXT id=<stable id>) through avahi-daemon's D-Bus API, since the service
// user cannot write /etc/avahi/services, and browses for the others. Without
// avahi, discovery is off and units are linked by a typed address.

const mdnsType = "_cutepi._tcp"

// foundUnit is a unit seen over mDNS (one per address it answers on).
type foundUnit struct {
	ID, Name, URL string
}

var (
	mdnsMu    sync.Mutex
	mdnsFound = map[string]foundUnit{} // by service name and interface
)

// discoveredUnits lists the units seen over mDNS, by name then address.
func discoveredUnits() []foundUnit {
	mdnsMu.Lock()
	defer mdnsMu.Unlock()
	out := make([]foundUnit, 0, len(mdnsFound))
	for _, u := range mdnsFound {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].URL < out[j].Name+out[j].URL })
	return out
}

// startMDNS publishes this unit and browses for the others (IPv4).
// shortcut: an avahi-daemon restart drops the advertisement until CuTePi restarts.
func startMDNS(port int, id, name string) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		logs.PrintfWarn(logs.LNKWarn, "mDNS discovery off (no system D-Bus): %v", err)
		return
	}
	const avahi = "org.freedesktop.Avahi"
	srv := conn.Object(avahi, "/")
	var group dbus.ObjectPath
	if err = srv.Call(avahi+".Server.EntryGroupNew", 0).Store(&group); err == nil {
		g := conn.Object(avahi, group)
		txt := [][]byte{[]byte("id=" + id)}
		err = g.Call(avahi+".EntryGroup.AddService", 0, int32(-1), int32(-1), uint32(0),
			name, mdnsType, "", "", uint16(port), txt).Err
		if err == nil {
			err = g.Call(avahi+".EntryGroup.Commit", 0).Err
		}
	}
	if err != nil {
		logs.PrintfWarn(logs.LNKWarn, "mDNS: advertising %s failed: %v", mdnsType, err)
	}
	// Subscribe before creating the browser: its first items follow at once.
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(avahi + ".ServiceBrowser")); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "mDNS: browsing failed: %v", err)
		return
	}
	ch := make(chan *dbus.Signal, 32)
	conn.Signal(ch)
	var browser dbus.ObjectPath
	if err := srv.Call(avahi+".Server.ServiceBrowserNew", 0, int32(-1), int32(0), mdnsType, "", uint32(0)).Store(&browser); err != nil {
		logs.PrintfWarn(logs.LNKWarn, "mDNS: browsing failed: %v", err)
		return
	}
	go func() {
		for s := range ch {
			if s.Path != browser || len(s.Body) < 5 {
				continue
			}
			iface, _ := s.Body[0].(int32)
			svc, _ := s.Body[2].(string)
			domain, _ := s.Body[4].(string)
			switch s.Name {
			case avahi + ".ServiceBrowser.ItemNew":
				goSafe(func() {
					var (
						rIface, rProto, aProto int32
						rName, rType, rDomain  string
						host, addr             string
						rPort                  uint16
						rTxt                   [][]byte
						flags                  uint32
					)
					err := srv.Call(avahi+".Server.ResolveService", 0, iface, int32(0), svc, mdnsType, domain, int32(0), uint32(0)).
						Store(&rIface, &rProto, &rName, &rType, &rDomain, &host, &aProto, &addr, &rPort, &rTxt, &flags)
					if err != nil {
						return
					}
					u := foundUnit{Name: svc, URL: "http://" + addr}
					if rPort != 80 {
						u.URL += ":" + strconv.Itoa(int(rPort))
					}
					for _, t := range rTxt {
						if v, ok := strings.CutPrefix(string(t), "id="); ok {
							u.ID = v
						}
					}
					if u.ID == "" || u.ID == id {
						return // not a CuTePi we can pair with, or this unit
					}
					mdnsMu.Lock()
					mdnsFound[svc+"|"+strconv.Itoa(int(iface))] = u
					mdnsMu.Unlock()
				})
			case avahi + ".ServiceBrowser.ItemRemove":
				mdnsMu.Lock()
				delete(mdnsFound, svc+"|"+strconv.Itoa(int(iface)))
				mdnsMu.Unlock()
			}
		}
	}()
}
