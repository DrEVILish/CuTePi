# Shared CuTePi / TimerPi Pi allocation

Status: 2026-11-15, updated after CuTePi hardware verification. The earlier read-only preflight is complete. Three normal stock-kernel boots were used during decoder recovery/comparison; no network configuration or controller was activated. The pre-boot mesh runtime did not survive those boots.

## Ownership and slots

- CuTePi lead retains playback, deployment and DRM ownership of 192.168.10.73.
- [TimerPi lead](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/69b2fa00-1ced-49b2-aba2-5c87a8238f1e) is the sole shared network-configuration writer.
- The read-only network preflight slot is complete. The Pi remains allocated to CuTePi playback; the CuTePi audit/fix/build work continues in CT110.
- The CuTePi hardware-test slot is released; the real service runs the tested c98ea65 application with the original saved show restored. No PI_LOCK remains.
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
| Radio | After hardware recovery: wlan0 DOWN, type managed. Before the boots it was IBSS cutepi-mesh, channel1 /2412MHz,20MHz on brcmfmac/phy0. No new radio profile was applied. |
| Mesh address / bridge | bat0 is absent after the normal boots; previously 10.77.0.73/24. Wired eth0 remains UP at 192.168.10.73/24 and unbridged. |
| Advertised capability | IBSS, managed, AP; channels13/2472 and36/5180 listed usable, actual joins not tested |
| Regulatory report | global GB; phy country99; observational facts, not a channel qualification |
| Clock | NTP=yes; NTPSynchronized=yes |
| Service/controller | cutepi-mesh LoadState=not-found; runtime mesh exists despite no installed unit |
| Known controller scripts | /opt/cutepi/tools/mesh/batman-wifi.sh and /root/batman-wifi.sh; last-applied script not established |
| batctl | absent from PATH and checked common locations; no private path established |

The repository deploy/cutepi-mesh.service is an optional unit template, not proof of an installed or running controller. Existing script uses ip/iw/modprobe rather than batctl.

## Shared profile decision remains open

CuTePi's last active profile was cutepi-mesh on2412 with a separate10.77 network and unbridged Ethernet. That runtime is currently down after the hardware-test boots; it has not been recreated because the shared network writer and controller-activation agreement still apply. TimerPi's product profile fixes ESSID timerpi and proposes channel13 /2472 (or channel36 /5180 under its radio policy), bat0 and eth0 bridged through br0, plus mDNS/event-key election/cloud synchronization. Both controllers cannot manage the same radio/bridge independently.

Resolve [the shared network-profile decision](../.scratch/design-review/issues/12-shared-network-profile.md) with the owner before convergence. Preserve TimerPi's existing fixed-ESSID decision; do not silently rewrite either product's network contract.

## Recovery observation

The H.264/MMAL decoder remained unusable across application restarts during experimental hardware tests and recovered on normal stock boots. The final c98ea65 service passed a real H.264 output check after the original show was restored. [Hardware evidence](reviews/2026-11-15-hardware.md) records the limits. Mesh restoration is a network-owner coordination item; no automatic convergence with TimerPi is authorised. Capability/regulatory/clock rows above retain their preflight observations.

## Playback constraints

AGENTS.md remains authoritative: no new applications, Node/npm/browser installation, service restarts/reboots, radio/bridge/route changes or DRM access during read-only preflight. Never open /dev/dri/card* before cutepi; do not read debug DRM state while measuring. Keep required avahi, NetworkManager/wpa_supplicant, ssh, timesyncd and polkit. Do not re-enable the disabled services/timers listed in AGENTS.md. Hardware testing uses the real service and independent picture/audio evidence.

No credentials, screen keys, pairing secrets or passwords belong in coordination messages or this file.
