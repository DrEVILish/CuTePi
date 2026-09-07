# CuTePi

Cue-based media playback controller for a Raspberry Pi + GStreamer. A Go (Gin +
SQLite) server serves a dark, htmx-driven control-centre UI that drives HDMI
playback out of the Pi via `gst-launch-1.0`.

- **DESIGN.md** — the design spec, architecture decisions, and feature checklist
  (completed work is marked, not deleted).
- **CHANGELOG.md** — append-only, running history of changes.
- **TODO.md** — shared tracker of open work.

## Quick start

Requires Go, GStreamer dev libraries, ffmpeg, and yt-dlp.

```sh
./pre.sh          # installs system deps, go mod tidy, build, vet, test
go run .          # starts the server (default port 3000, see ~/CTP/config/config.json)
```

Open `http://<host>:3000/` for the control centre. `/upload` is the
mobile-friendly standalone upload page.

### Run as a service (systemd)

For a persistent appliance setup (auto-start on boot, restart on crash, and a
UI Restart button that talks to systemd), install it as a service:

```sh
./install.sh      # builds, installs assets, enables + starts the service
```

This installs the binary and its runtime assets (`templates/`, `public/`) to
`/opt/cutepi`, the data tree to `/opt/cutepi/data`, and a `cutepi.service`
unit (listening on port 3001). After any code change, re-run `./install.sh`
to rebuild and restart the service (or `systemctl restart cutepi`).

Overrides via env: `CUTEPI_PREFIX`, `CUTEPI_DATA`, `CUTEPI_PORT`,
`CUTEPI_SERVICE` (see the script header in `install.sh`).

Configuration, the SQLite database, media, and thumbnails are stored under
`~/CTP/` by default (`config/`, `media/`, `thumbnails/`); override via the
`CONFIG_PATH`, `DB_PATH`, `MEDIA_DIR`, `THUMBNAILS_DIR`, `WORKING_DIR`,
`PORT`, and `POLL_INTERVAL_MS` environment variables, or by editing
`~/CTP/config/config.json` (port and poll interval changes require a restart).

## Features

**Stack** — Go + Gin server, SQLite (`sqlx`) as the single source of truth,
htmx v4 for server-rendered interactivity, Bootstrap 5, vanilla JS for
client-side state. Third-party client deps are vendored under
`public/src/` so the app runs without a CDN.

**Media pool (left pane)** — two-column 16:9 thumbnail grid with filename +
type filtering; each tile shows its date added and a per-type placeholder SVG
(video/audio/image) while its thumbnail is generating. Per-item actions:
Play, Load, Add-to-cue, Delete (with an "are you sure?" confirm modal), and
Refresh thumbnail. `date_added` sort, newest first. Upload via drag-and-drop
directly onto the pool, the desktop upload modal, or the standalone `/upload`
page; YouTube/URL downloads via yt-dlp. The panel can be collapsed or
drag-resized (persisted in `localStorage`) and shows an empty-state placeholder.

**Cuesheet (right pane)** — six columns: Cue No, Cue Name,
Cue Pre-Wait, Cue Duration, Cue Post-Wait, Actions. Cue Name
and the time fields are edited inline by double-click; the time parser accepts
`hh:mm:ss.ms` (or a fraction) and a bare number (seconds). Actions: Play,
Move Up, Move Down, Delete. Column widths are user-adjustable by dragging a
header's right edge (persisted in `localStorage`). Cues can be added by
dragging media-pool items onto the sheet.

The Settings menu includes LCARS, QLab, and Future SciFi themes. Future SciFi
is the default. The selected theme is browser-local UI state; the logo is a
separate 80s corporate-style asset.

**Now Playing widget** — shows the current filename and elapsed/total time.
It polls a lightweight `GET /api/nowplaying/status` change-detection endpoint
every 500ms and only re-renders the full widget (via `GET /api/nowplaying`)
when the server's monotonic state counter signals a real change, so idle and
paused widgets are not re-rendered on every poll. Once a clip is loaded it
also shows a **scrubber**, a **volume** slider, and a **loop** toggle that
drive `POST /api/seek`, `/api/volume`, and `/api/loop`.

**Playback controls** — play / pause / toggle-pause / stop / panic / fade-out,
plus a **Show Test** button that loads a GStreamer test pattern through a
modal with a "Hide Test" control. Playback is supplied by the `gsp` GStreamer
pipeline manager (single mutex-guarded pipeline, atomic swap), runtime-smoke-
tested against real `gst-launch-1.0` assets. Clips can **loop** at their end
and honor a **volume** gain stage (both configurable by default in
`config.json`).

**Cue Inspector** — for the selected cue: trim In/Out, Hold-last-frame, and
Play, plus a draggable **waveform trim timeline** (generated per media item)
that sets the trim window graphically. Waveform peaks are computed on import
and stored in the database; the Media Pool dropdown's **Analyse** action
rebuilds them.

**Cue trigger** — pressing **Space** (with the UI focused, not in an editable
field) plays the currently selected cue. Waits (`preWait`/`postWait`) apply
only to cues set to **auto-continue**; otherwise each cue stops on playback
end (cues are a list, not a playlist). New cues default to `loop=off`,
`hold=off`.

**Settings modal** — edit the server port, poll interval, default **loop**,
and default **volume**; a restart-required message is shown when the port
changes. The TopBar dropdown also has **Restart** and **Shutdown** controls
(each with a confirm dialog) that re-exec or gracefully terminate the server
process.

**Media tooling** — ffprobe/ffmpeg metadata extraction (upload validation),
background thumbnail generation (video frame / image resize / audio waveform)
via a DB-backed worker whose pending queue survives server restarts.

## Tests

```sh
go test ./...
```

Covers `config` (path resolution, port/poll-interval validation), `ctp`
(mediapool registration, cue navigation boundaries, cue-column edit
allow-list, cue removal, time parse/format, move up/down), `media`, `worker`,
`routes` (HTTP-level template rendering), and a GStreamer playback runtime
smoke test (`gsp`).

## Roadmap / not yet built

Implemented: Cue Inspector, trim, Hold, cue reorder, QR upload, WebSocket sync,
persisted selection, restart/shutdown controls, dead-code cleanup, waveform-in-
DB, service-managed operation (a systemd unit installed by `install.sh`), and
the full 2026-09-04 feature set: **Space** cue trigger, **auto-continue** with
`preWait`/`postWait` timing plus the **loop counter**, **defaults** (loop/hold
off), **Cue Groups** (nestable collapsible folders that act as playlists;
membership rides the flat `cuePos` order), **slideshow** cue groups (visible
image rows, shuffled/looped/faded on per-group settings, with a now-showing
indicator), **missing-source** warnings (startup scan; cues offer delete or
re-link), **.CTP export + import** (ZIP of a JSON manifest incl. audit trail +
referenced media; import appends or overwrites), an **import-time playability
probe**, and the **Web UI log viewer** (level-selectable recording, clear,
audit trail).

Outstanding (see TODO.md): none from the 2026-09-04 scope — all built.

Hardware target: Raspberry Pi 4/5, headless Debian Trixie (no display server —
HDMI video via KMS/DRM, HDMI embedded audio exclusive to CuTePi), hardware-
first decode with software fallback. Remaining work is hardware validation on
the Pi — one command: `./smoke-test.sh` (see DESIGN.md; Q18/Q19 both resolved).

**Not planned** (explicitly out of scope): HyperDeck / Companion
feature-compatibility and DeckLink SDI output.
