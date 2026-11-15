# Shared CuTePi / TimerPi Pi allocation

Status: 2026-11-15. The owner authorised both physical Pis for TimerPi/shared-Pi preparation: “You are allowed to use both pi’s.” The [TimerPi lead](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/69b2fa00-1ced-49b2-aba2-5c87a8238f1e) subsequently verified both by read-only SSH. Fresh identity/network/service observations below come from that preflight; older OS, build and tooling facts are dated separately. This record update involved no Pi commands or device changes.

## Ownership and slots

- Both 192.168.10.73 and 192.168.10.55 are allocated for TimerPi/shared-Pi preparation under the new owner grant. Neither 192.168.10.162 (the amd64 UI/dev server) nor 192.168.10.50 (CT110) is the second Pi.
- CuTePi lead retains playback, deployment and DRM ownership. TimerPi lead remains the sole shared network-configuration writer.
- The CuTePi hardware-test slot is released; .73 last ran the tested c98ea65 application with the original saved show restored. No PI_LOCK remains. CuTePi project work continues in CT110; no additional CuTePi hardware measurement is currently scheduled.
- No competing CuTePi hardware measurement/deploy is scheduled for the reserved qualification session below. Existing workloads remain protected until the timed handover: .73 runs cutepi; .55 runs cutepi-follower and has an active/exited cutepi-mesh unit.
- The original allocation grant did not settle the shared network profile or grant a disruptive slot. The later owner-directed qualification session below reserves a temporary timerpi/ch13/GB test window, subject to captured baselines, rollback and timed service handovers. Permanent profile convergence remains undecided. Preserve Ethernet management throughout; TimerPi alone changes network controllers.
- The separate .55 reboot restriction remains: explicit owner approval is required. Do not run the TimerPi installer on a CuTePi playback Pi.
- Existing direct PVE-root SSH access to root@192.168.10.73 and root@192.168.10.55 was verified by TimerPi's fresh preflight. Use the existing SSH identity and host-key trust; no new credentials are needed or shared here.

## Reserved qualification session: 2026-11-15, from 15:16:39 UTC

The owner directed TimerPi to perform correct hardware tests and finish delivery. CuTePi accepted TimerPi's requested shared session from 15:16:39 UTC until explicit release/rollback handback. This reservation supersedes the earlier no-slot status for temporary qualification only. It is not a permanent network-profile decision or a claim that tests have passed.

- Initial stage: isolated TimerPi build/data on temporary port18080, display off, no CuTePi data/service changes. Existing playback continues.
- Radio stage: temporary TimerPi PRODUCT profile timerpi/ch13/GB, with wired management retained. TimerPi is the sole network writer and captures baselines/rollback before transport changes. The .55 radio-transition handover was requested for 15:31:00 UTC; follower stop was verified at 15:31:11.868 UTC. The later .73 display handover remains separate.
- Display stage, if required: exclusive .73 DRM handover after the real cutepi service is stopped and its processes have released the display. Never run competing DRM tools while CuTePi playback is active.
- No reboot is planned. Existing .55 reboot restrictions and .73 software-install/disabled-service restrictions remain.
- CuTePi performed the service-only .55 handover below. Earlier inventory tables remain dated preflight observations, not a live session-state report.

### .55 follower pause completed: 15:31:11 UTC

TimerPi confirmed private baseline/rollback archives at PVE /var/tmp/timerpi-hw-20261115, an exact .55 runtime unit/env backup, isolated port18080/display-off test services and no DRM tools. It owns controller stop/masking, timed network-only rollback and the radio transition; Ethernet addresses/routes remain untouched for the first transport drill. Its captured .73 rollback state is rfkill-blocked/down/managed/no bat0, power saving on, MTU1500; .55 restores the original controller/profile/bat0 with power saving off.

CuTePi requested only systemctl stop cutepi-follower.service on .55 at 15:31:11.731 UTC, 11.7 seconds after the agreed boundary. Verification at 15:31:11.868 UTC found inactive/dead, MainPID=0, ControlPID=0, original PID1707227 absent and no service-cgroup members. No custom ExecStop/ExecStopPost hooks were present. TimerPi received an explicit STOPPED acknowledgment before its radio transition.

The transient unit unloaded (LoadState=not-found). Recreate the exact saved runtime definition/env on restoration; a plain systemctl start cannot be assumed to work. A second private definition/properties backup and command evidence are held in CT110 /var/lib/cutepi-work/handover-20261115; do not commit raw configuration or environment.

Follower restoration is pending TimerPi's confirmation that its original .55 network/controller baseline is restored. CuTePi issued no mesh-unit, radio/network, DRM or .73 commands during this handover. The session remains reserved; .73 playback has not been paused.

### Controlled pause and restore handover

1. Before a pause, TimerPi supplies the exact UTC stage/start time and confirms protected baselines and rollback exist: unit definitions/drop-ins, active/enabled state, process ownership, binary identity, and show/data state; network baselines belong to TimerPi. Keep raw captures private because unit arguments/configuration may contain secrets. Preserve the original binary, data and service configuration; do not redeploy CuTePi.
2. CuTePi acknowledges the agreed service-only action and records its completion before TimerPi proceeds. On .55, CuTePi pauses cutepi-follower; TimerPi alone stops or otherwise controls cutepi-mesh and its network side effects. An active/exited unit may still have an ExecStop that changes networking.
3. Before .73 display testing, TimerPi confirms its display is off. CuTePi stops the real cutepi service and verifies its processes have exited without opening a DRM card. A failed or incomplete stop does not release ownership. Only an explicit stopped/released acknowledgment permits TimerPi display access.
4. During testing, only TimerPi changes radios/controllers/bridges/routes. No CuTePi deployment, show edit, concurrent hardware benchmark or DRM probe is scheduled.
5. For restoration, TimerPi first stops its test processes and children, releases DRM, restores its network changes to the captured baseline, and confirms wired management and controller ownership. Restore .73's original down/managed/no-bat0 state and .55's original cutepi-mesh state; do not automatically apply the same mesh to both.
6. CuTePi then restores only services it paused and their original runtime state. Verify the original binary/configuration/show, service health and HTTP access; qualify .73 playback with independent picture/audio evidence when playback resumes. Report failures and retain the reservation until recovery or explicit handback. Do not label restoration complete from service status alone.

## Fresh read-only inventory: 2026-11-15

| Property | Playback Pi: 192.168.10.73 | Second Pi: 192.168.10.55 |
|---|---|---|
| Host/model | cutepi; Raspberry Pi 4 Model B Rev 1.5 | pisignage; Raspberry Pi 4 Model B Rev 1.5 |
| Running kernel | 6.18.50+rpt-rpi-v8 | 6.12.109+rpt-rpi-v8 |
| Wired management | eth0 UP, 192.168.10.73/24; default gateway 192.168.10.1 | eth0 UP, 192.168.10.55/24; default gateway 192.168.10.1 |
| Interface drivers | eth0: bcmgenet; wlan0: brcmfmac | eth0: bcmgenet; wlan0: brcmfmac |
| Radio | wlan0 DOWN, type managed; iw reports channel 34/5170MHz while down, which is not an active join | wlan0 UP, IBSS cutepi-mesh, channel 1/2412MHz, 20MHz |
| Mesh / bridge | bat0 absent; Ethernet remains unbridged after hardware recovery | bat0 10.77.0.55/24; eth0 unbridged, no br0 |
| Playback service | cutepi active/running | cutepi-follower active/running; cutepi unit absent |
| Mesh service/controller | cutepi-mesh unit absent; no runtime mesh | cutepi-mesh loaded, active/exited; existing mesh must be preserved |
| Other checked units | timerpi, timerpi-mesh and cutepi-follower absent | TimerPi units absent |

An active/exited mesh unit describes its systemd state, not proof of peer connectivity or application synchronisation. Both Pis are reachable over their wired management addresses; no mesh drill or sync qualification was performed in this preflight.

## Earlier inventory and evidence

### Playback Pi: .73

CuTePi's hardware verification on 2026-11-15 recorded the real cutepi service running as the unprivileged cutepi user on port 80, HDMI 1080p60 video/audio, and data in /var/lib/cutepi. Its tested application was c98ea65 with the original show restored. Debian 13.7/Trixie was recorded in TimerPi's earlier preflight; the fresh checks above did not establish a newer OS or application version.

Three normal stock-kernel boots during decoder recovery/comparison removed the earlier runtime IBSS cutepi-mesh/channel 1/2412MHz and bat0 10.77.0.73/24 configuration. No network configuration or controller was activated during that work. The fresh preflight confirms that mesh is still absent; the previous “runtime mesh exists” statement was stale.

Earlier capability observations: phy0 advertised IBSS, managed and AP, with channels 13/2472 and 36/5180 listed usable; actual joins were not tested. Regulatory reports were global GB and phy country 99, not channel qualification. NTP=yes and NTPSynchronized=yes were observed then, not reverified here. Known scripts were /opt/cutepi/tools/mesh/batman-wifi.sh and /root/batman-wifi.sh; their last application was not established. batctl was absent from PATH and checked common locations; no private path was established. The repository deploy/cutepi-mesh.service is an optional template, not proof of an installed unit on .73; its script uses ip/iw/modprobe rather than batctl.

### Second Pi: .55

The following are historical 2026-11-12 facts, retained separately from the fresh observations:

- Physical Pi 4B Rev1.5, approximately 4 GiB RAM. Upgraded from Debian 12 to Debian 13/Trixie with a clean dpkg audit. Kernel 6.18.60 was installed but not booted; the fresh preflight still reports running kernel 6.12.109+rpt-rpi-v8.
- Go 1.27.1 and GStreamer 1.26.2 were installed with a then-current CuTePi follower build. Its current binary commit remains unverified. The follower was configured with headless/dummy video and no attached monitor then; current display attachment was not checked.
- Wi-Fi was identified as BCM43455/CYW43455 SDIO. The stock brcmfmac driver was restored after temporary driver experiments; power saving was off then. These firmware/configuration details were not rechecked in the fresh preflight.
- PiSignage player/Node/mpv/Chromium autostart was stopped; application files were retained under /root/pisignage-removed. Node/Chromium packages remained installed then. Their current state was not rechecked.
- Wired two-Pi sync tests used .73 and .55. The .55 show was restored and the units unlinked afterwards; the follower service was left running. The new preflight confirms a running follower, without establishing its current show or linkage.

Historical sources: [Debian Trixie upgrade](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/393cb3cf-d8b5-4cb4-b12c-bb67a3b50b4d), [Wi-Fi driver research](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:delegated-task:command%3Amcp%3A2f467470-d027-4796-9e33-e7f21ed684a1%3Adelegate-task%3A31767eda-ee53-4119-8e07-0915c08fd551), and [multi-unit sync tests](t3-thread://v2/9083cb1a-e6ff-40c7-869a-a43f5d9bcad6/thread:delegated-task:command%3Amcp%3A2f467470-d027-4796-9e33-e7f21ed684a1%3Adelegate-task%3A18db06a5-509f-4efd-ad2b-ce347afea43f). [DESIGN.md](../DESIGN.md) also records the two-Pi measurements.

## Shared profile decision remains open

CuTePi's existing profile uses cutepi-mesh on 2412 MHz with a separate 10.77 network and unbridged Ethernet. It is active on .55 and absent on .73 after the hardware-test boots. TimerPi's product profile fixes ESSID timerpi and proposes channel 13/2472MHz (or channel 36/5180MHz under its radio policy), bat0 and eth0 bridged through br0, plus mDNS/event-key election/cloud synchronisation. Both controllers cannot manage the same radio/bridge independently.

Resolve [the shared network-profile decision](../.scratch/design-review/issues/12-shared-network-profile.md), cross-linked to [TimerPi issue #7](https://github.com/DrEVILish/timerpi/issues/7), with the owner before convergence. Preserve TimerPi's existing fixed-ESSID decision; do not silently rewrite either product's network contract. Allocation and wired reachability do not prove BATMAN-adv transport or application sync.

## Recovery observation and playback constraints

The H.264/MMAL decoder remained unusable across application restarts during experimental hardware tests and recovered on normal stock boots. The final c98ea65 service passed a real H.264 output check after the original show was restored. [Hardware evidence](reviews/2026-11-15-hardware.md) records the limits. The temporary qualification may change .73 networking under TimerPi ownership, but rollback must restore its captured pre-session state. No permanent or automatic convergence with TimerPi is authorised.

[AGENTS.md](../AGENTS.md) remains authoritative for .73: no new applications, Node/npm/browser installation, or re-enabling disabled services/timers. Keep required avahi, NetworkManager/wpa_supplicant, ssh, timesyncd and polkit. Read-only preflight permits no service restarts/reboots, radio/bridge/route changes or DRM access. Never open /dev/dri/card* before cutepi; do not read debug DRM state while measuring. Hardware testing uses the real service and independent picture/audio evidence.

No credentials, screen keys, pairing secrets or passwords belong in coordination messages or this file.
