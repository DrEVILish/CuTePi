# AGENTS.md

Environment notes for agents working on this repo.

## Servers

| Role | Host | Port | Public name (reverse proxy) |
|------|------|------|------------------------------|
| Test | `192.168.10.73`  | 80   | `cutepi-test.drevilish.com` |
| Dev  | `192.168.10.162` | 3001 | `cutepi-dev.drevilish.com`  |

- Both names are reverse proxies to the machines above.
- Reverse proxies must always work: the app answers under any Host name (`routes/origin.go` has no Host allow-list, and there is no `allowed_hosts` setting). Do not add a Host check, allow-list or DNS-rebinding guard that refuses requests by name. The only cross-site protection is the Origin/Referer check on state-changing requests.
- The proxy (Nginx Proxy Manager, `192.168.10.120`) serves these names over HTTP only. HTTPS fails at the TLS handshake until a certificate is attached to each proxy host.
- SSH to the other machine with `root@<ip-address>`.
- Both machines have the repo checked out under `/opt`.

## Test server (`192.168.10.73`)

- Runs on the target hardware, with a 1080p60 HDMI monitor plugged in that handles video and audio.
- The app runs as the `cutepi` systemd service on port 80, as the unprivileged `cutepi` user (DESIGN §7). Restart it with `systemctl restart cutepi`.
- Its data is in `/var/lib/cutepi` (DB `/var/lib/cutepi/config/ctp.db`, media, thumbnails, WebKit caches under `.cache/`). `/root/cutepi` is the copy from before the move to the service user (2026-10-07); delete it once the move is confirmed. `deploy/install-service-user.sh` sets the machine up (user, polkit rules, unit); the old unit is kept as `/etc/systemd/system/cutepi.service.pre-user`.
- Use the real service for testing, not a throwaway instance on a temp directory.
- There is no browser or Node on this machine, and none may be installed. Keep this machine free of extra applications: no npm, browsers or other tooling beyond what the app itself needs. Test through the HTTP API and measure the results from outside the process (ALSA loopback capture for audio, the DRM plane state for video).
- Video runs on hardware display planes (`CUTEPI_WALL_SINK=kmssink` in `/etc/systemd/system/cutepi.service.d/playback-env.conf`). Pictures are not in `/dev/fb0`, which only holds the black console layer underneath. To measure video, read the overlay planes' properties through libdrm (`FB_ID` changes per presented frame; also `alpha`, `zpos`, `CRTC_X/Y/W/H` and `rotation`), for example with Python `ctypes`. Poll only the planes in use, at 10 ms or slower: heavy polling takes the modeset lock and costs the video frames.
- For exact per-plane frame rates, trace the kernel rather than polling: kprobes and kretprobes on `drm_mode_setplane` (kmssink's frames; the blocking call returns when the frame is latched) and on `drm_mode_obj_set_property_ioctl` (alpha/zpos writes), plus the `drm:drm_vblank_event` tracepoint for the refresh grid, with `trace_clock=mono`. This needs no `/dev/dri` access. A property or `GetPlane` poll waits on the plane lock that every blocking commit holds, so with two busy planes it samples only every ~60 ms and under-reads. Remove the kprobes afterwards. TEST_REPORT O1 has the method.
- The service must be the display's DRM master. Don't open `/dev/dri/card*` before the service has started (a tool that opens it first becomes master and playback fails with "permission denied").

## Dev server (`192.168.10.162`)

- Installing npm and browsers (for example Playwright or Chromium) is allowed here, and only here. Use it for UI work such as screenshots and browser-driven checks.
- Do not install these on the test server. Run browser-based checks against the dev server instead.
