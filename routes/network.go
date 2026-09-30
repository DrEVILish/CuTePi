package routes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"CuTePi/config"
	"CuTePi/logs"
)

// sysCmdTimeout bounds the system tools called from request handlers
// (nmcli, hostnamectl, xrandr, aplay): a wedged NetworkManager or D-Bus must
// fail the request, not hang it forever.
const sysCmdTimeout = 20 * time.Second

// sysOutput runs a system tool under sysCmdTimeout and returns its stdout
// (combined=false) or stdout+stderr (combined=true).
func sysOutput(combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sysCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	if combined {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

// InstanceName is the machine's hostname — what the mDNS record
// (<name>.local) and the UI brand derive from.
func InstanceName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "cutepi"
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// SetInstanceName renames the machine (hostname + mDNS). Best-effort per
// step with hard errors: validation and hostnamectl must succeed; the
// Avahi refresh degrades to a logged warning (mDNS catches up on its own
// within a couple of minutes at worst).
func SetInstanceName(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if !hostnameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: lowercase letters, digits and hyphens, start/end alphanumeric", name)
	}
	hostnamectl, err := exec.LookPath("hostnamectl")
	if err != nil {
		return fmt.Errorf("hostnamectl not available: cannot rename")
	}
	if out, err := sysOutput(true, hostnamectl, "set-hostname", name); err != nil {
		return fmt.Errorf("hostnamectl: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if avahi, err := exec.LookPath("avahi-set-host-name"); err == nil {
		if out, err := sysOutput(true, avahi, name); err != nil {
			logs.Printf(logs.NETHotspotWarn, "avahi refresh: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	} else if systemctl, err := exec.LookPath("systemctl"); err == nil {
		if out, err := sysOutput(true, systemctl, "restart", "avahi-daemon"); err != nil {
			logs.Printf(logs.NETHotspotWarn, "avahi restart: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	logs.Printf(logs.NETHotspot, "instance renamed to %q", name)
	return nil
}

// Wi-Fi access-point control for the Network settings tab. The Pi's
// image ships NetworkManager, so the hotspot is an nmcli wifi hotspot
// (WPA2-PSK) call, which creates the connection profile + dnsmasq bridge in
// one step. Everything is guarded: if nmcli or a Wi-Fi NIC is missing the
// call degrades to a logged warning, never a broken settings save.
// ponytail: single-shot nmcli hotspot, no channel/ip config; add managed
// profiles when an operator needs a fixed 5GHz band plan.

// applyAP is the hotspot apply hook used by the settings save (a var so
// tests can observe it without running nmcli).
var applyAP = networkAPApply

// networkAPApply turns the configured hotspot on or off to match the saved
// settings. SSID changed while enabled → restart the hotspot.
func networkAPApply() error {
	ap := config.AP()
	nmcli, err := exec.LookPath("nmcli")
	if err != nil {
		return fmt.Errorf("NetworkManager (nmcli) not available: hotspot cannot be managed from these settings")
	}
	if !ap.Enabled {
		// Failure is expected when the hotspot is not up: harmless.
		_, _ = sysOutput(false, nmcli, "connection", "down", "Hotspot")
		return nil
	}
	if ap.SSID == "" {
		return fmt.Errorf("hotspot enabled with no SSID")
	}
	args := []string{"connection", "hotspot"}
	// No Wi-Fi NIC found: omit ifname entirely and let nmcli pick — an
	// empty `ifname ""` argument is rejected by nmcli.
	if nic := wifiNIC(nmcli); nic != "" {
		args = append(args, "ifname", nic)
	}
	args = append(args, "ssid", ap.SSID)
	if ap.Pass != "" {
		args = append(args, "password", ap.Pass)
	}
	if out, err := sysOutput(true, nmcli, args...); err != nil {
		return fmt.Errorf("nmcli: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	logs.Printf(logs.NETHotspot, "Wi-Fi hotspot up: SSID %q", ap.SSID)
	return nil
}

// wifiNIC finds the first Wi-Fi-capable interface name via nmcli ("" lets
// nmcli pick).
func wifiNIC(nmcli string) string {
	out, err := sysOutput(false, nmcli, "--terse", "--fields", "DEVICE,TYPE", "device", "status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Blank/trailing lines split to zero fields — index before comparing.
		if len(f) == 2 && strings.EqualFold(f[1], "wifi") {
			return f[0]
		}
	}
	return ""
}

// networkAPWarn logs hotspot failures without failing the settings save.
func networkAPWarn(err error) {
	logs.Printf(logs.NETHotspotWarn, "Wi-Fi hotspot: %v", err)
}
