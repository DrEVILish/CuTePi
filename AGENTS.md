# AGENTS.md

Environment notes for agents working on this repo.

## Servers

| Role | Host | Port | Public name (reverse proxy) |
|------|------|------|------------------------------|
| Test | `192.168.10.73`  | 80   | `cutepi-test.drevilish.com` |
| Dev  | `192.168.10.162` | 3001 | `cutepi-dev.drevilish.com`  |

- Both names are reverse proxies to the machines above.
- The app refuses any public Host name it doesn't know with HTTP 421 (DNS-rebinding guard, `routes/origin.go`). Each proxy name must be listed in `allowed_hosts` in that machine's `/root/cutepi/config/config.json`, and the service restarted.
- The proxy (Nginx Proxy Manager, `192.168.10.120`) serves these names over HTTP only. HTTPS fails at the TLS handshake until a certificate is attached to each proxy host.
- SSH to the other machine with `root@<ip-address>`.
- Both machines have the repo checked out under `/opt`.

## Test server (`192.168.10.73`)

- Runs on the target hardware, with a 1080p60 HDMI monitor plugged in that handles video and audio.
- The app runs as the `cutepi` systemd service on port 80. Restart it with `systemctl restart cutepi`.
- Use the real service for testing, not a throwaway instance on a temp directory.
- There is no browser or Node on this machine, and none may be installed. Keep this machine free of extra applications: no npm, browsers or other tooling beyond what the app itself needs. Test through the HTTP API and measure the results from outside the process (ALSA loopback capture for audio, the DRM plane state for video).
- Video runs on hardware display planes (`CUTEPI_WALL_SINK=kmssink` in `/etc/systemd/system/cutepi.service.d/playback-env.conf`). Pictures are not in `/dev/fb0`, which only holds the black console layer underneath. To measure video, read the overlay planes' properties through libdrm (`FB_ID` changes per presented frame; also `alpha`, `zpos`, `CRTC_X/Y/W/H` and `rotation`), for example with Python `ctypes`. Poll only the planes in use, at 10 ms or slower: heavy polling takes the modeset lock and costs the video frames.
- The service must be the display's DRM master. Don't open `/dev/dri/card*` before the service has started (a tool that opens it first becomes master and playback fails with "permission denied").

## Dev server (`192.168.10.162`)

- Installing npm and browsers (for example Playwright or Chromium) is allowed here, and only here. Use it for UI work such as screenshots and browser-driven checks.
- Do not install these on the test server. Run browser-based checks against the dev server instead.
