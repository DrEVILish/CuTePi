# Shared CuTePi / TimerPi Pi allocation

Status: 2026-11-15, read-only preflight completed by both project leads. No hardware/network/controller changes were made.

## Ownership and slots

- CuTePi lead retains playback, deployment and DRM ownership of 192.168.10.73.
- [TimerPi lead](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/69b2fa00-1ced-49b2-aba2-5c87a8238f1e) is the sole shared network-configuration writer.
- The read-only network preflight slot is complete. The Pi remains allocated to CuTePi playback; the CuTePi audit/fix/build work continues in CT110.
- No second Pi is allocated. Historical .55 is unverified, .162 is the CuTePi UI/dev server, and .50 is CT110, not playback hardware.
- Disruptive mesh/sync drills need a named second Pi, a preserved Ethernet management route, recorded rollback and an explicitly agreed shared time slot. Do not run either network controller until that slot is agreed.
- Do not run the TimerPi installer on this CuTePi playback Pi.

## Verified playback Pi inventory

| Property | Observed value |
|---|---|
| Host/model | cutepi; Raspberry Pi 4 Model B Rev 1.5 |
| OS/kernel | Debian 13.7/Trixie (TimerPi preflight); stock 6.18.50+rpt-rpi-v8 |
| SSH / wired management | root@192.168.10.73; eth0 192.168.10.73/24; default gateway 192.168.10.1 |
| Playback | real cutepi service active, unprivileged cutepi user, port80, HDMI1080p60; data /var/lib/cutepi |
| Radio | phy0, brcmfmac, wlan0 IBSS cutepi-mesh, channel1 /2412MHz,20MHz; radio is a bat0 member |
| Mesh address / bridge | bat0 10.77.0.73/24; eth0 unbridged; no br0 observed |
| Advertised capability | IBSS, managed, AP; channels13/2472 and36/5180 listed usable, actual joins not tested |
| Regulatory report | global GB; phy country99; observational facts, not a channel qualification |
| Clock | NTP=yes; NTPSynchronized=yes |
| Service/controller | cutepi-mesh LoadState=not-found; runtime mesh exists despite no installed unit |
| Known controller scripts | /opt/cutepi/tools/mesh/batman-wifi.sh and /root/batman-wifi.sh; last-applied script not established |
| batctl | absent from PATH and checked common locations; no private path established |

The repository deploy/cutepi-mesh.service is an optional unit template, not proof of an installed or running controller. Existing script uses ip/iw/modprobe rather than batctl.

## Shared profile decision remains open

CuTePi's current profile is cutepi-mesh on2412 with a separate10.77 network and unbridged Ethernet. TimerPi's product profile fixes ESSID timerpi and proposes channel13 /2472 (or channel36 /5180 under its radio policy), bat0 and eth0 bridged through br0, plus mDNS/event-key election/cloud synchronization. Both controllers cannot manage the same radio/bridge independently.

Resolve [the shared network-profile decision](../.scratch/design-review/issues/12-shared-network-profile.md) with the owner before convergence. Preserve TimerPi's existing fixed-ESSID decision; do not silently rewrite either product's network contract.

## Playback constraints

AGENTS.md remains authoritative: no new applications, Node/npm/browser installation, service restarts/reboots, radio/bridge/route changes or DRM access during read-only preflight. Never open /dev/dri/card* before cutepi; do not read debug DRM state while measuring. Keep required avahi, NetworkManager/wpa_supplicant, ssh, timesyncd and polkit. Do not re-enable the disabled services/timers listed in AGENTS.md. Hardware testing uses the real service and independent picture/audio evidence.

No credentials, screen keys, pairing secrets or passwords belong in coordination messages or this file.
