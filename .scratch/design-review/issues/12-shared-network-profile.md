# Which shared network profile and Pi allocation should both projects use?

Labels: wayfinder:grilling
Type: grilling
Status: open
Assignee: unassigned
Parent: [CuTePi design and specification review](../map.md)
Blocked by: none
Area: deploy and tools / routes and ws / TimerPi networking owner

## Question

With TimerPi's fixed ESSID timerpi decision preserved, should CuTePi share TimerPi's IBSS/bridge profile or keep an explicitly separate mesh? Decide SSID/frequency/BSSID/subnets, Ethernet management versus br0 bridging, one network controller and configuration lifecycle, clock-sync ownership, discovery/election isolation and whether both applications ever coexist on one Pi. Select an allocated second Pi and a reversible shared drill slot before any conversion.

## Starting recommendation

Use one named network writer across projects, retain CuTePi playback/deploy/DRM ownership, and keep the current wired management path and configuration unchanged until topology, rollback and hardware allocation are agreed. Do not run the TimerPi installer or both network controllers on the playback Pi. Treat advertised radio capability as unqualified until a controlled join/drill.

## Cross-project decision

[Choose the shared CuTePi and TimerPi network profile](https://github.com/DrEVILish/timerpi/issues/7) is the matching TimerPi Wayfinder ticket. Both tickets share the allocation and one-network-writer constraints; neither authorises device conversion or a separate profile assumption. TimerPi HW-DRILLS prerequisites include two named Pis, an agreed slot/profile, one network writer, retained wired management and rollback, followed by separate transport and application-sync proof.

## Evidence to inspect

[Verified shared Pi inventory and allocation](../../../docs/SHARED_PI_ALLOCATION.md). CuTePi tools/mesh/batman-wifi.sh currently uses cutepi-mesh/2412/bat0 10.77.0.x and leaves eth0 unbridged. TimerPi lead reports its product profile uses timerpi, ch13/5GHz36 radio policy and br0 with eth0. Both physical Pis (.73 and .55) are now allocated. The owner-directed temporary qualification session is complete in the canonical allocation record: timerpi/ch13/GB, retained Ethernet, captured rollback and timed playback handovers. Permanent shared-profile convergence remains open.

## Comments

2026-11-15 — Both project leads acknowledged the owner-authorised coordination, single network writer and Ethernet/rollback constraints. This is a decision ticket, not permission to convert either device or rewrite either product spec.

2026-11-15, 15:16:39 UTC — CuTePi accepted TimerPi's owner-directed qualification session until explicit release/rollback handback. TimerPi alone owns network changes; CuTePi owns playback/deploy/DRM pause and restoration. Exact disruptive-stage times remain to be exchanged. Temporary qualification does not resolve this permanent-profile decision.

2026-11-15, 16:14:03 UTC — Final network handback received; original CuTePi playback and follower restored. Temporary qualification did not convert either permanent profile. TimerPi reports wireless1000-client delivery complete but operator p95=1.38s exceeded250ms, so that load remains unqualified. TimerPi subsequently confirmed temporary-file/unit cleanup and explicitly released the slot at16:15:02UTC.
