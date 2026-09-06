# CuTePi Design Document

This is the single living design/spec and architecture document for CuTePi. It is
updated as part of every request that changes scope, architecture, or requirements
— do not let it drift out of sync with the code. Completed work is marked, not
deleted, so history is visible.

## Overview

CuTePi is a media cue-playback controller intended to run on a Raspberry Pi 4/5,
driving video/audio playback out of the HDMI port via GStreamer. It has a Go
backend (HTMX server-rendered UI) and a vanilla HTML5/CSS3/JS frontend. SQLite3
is the source of truth for app state (media library + cue list + settings).

- Server: Go, HTMX for interactivity, no client-side framework.
- Client: HTML5/CSS3/vanilla JS only.
- Media tooling: ffmpeg (thumbnails/waveforms/metadata), yt-dlp (URL downloads).
- Playback: GStreamer, including test-pattern playback.
- DB: SQLite3, auto-created with schema on first run if missing.
- Config: `~/CTP/config/config.json`, `~/CTP/config/ctp.db`, media in
  `~/CTP/media/`, thumbnails in `~/CTP/thumbnails/`. `~` expands to the real
  home directory. Default port 3000.
- Multi-client sync: a WebSocket hub with server-authoritative state; the
  change-detection polling path remains the reconnect/fallback mechanism.
- Trust model: trusted local network, no auth.

## Product Decisions (2026-09-04 — operator answered the open hardware/UX questions)

- **Platform**: Raspberry Pi 4 or 5, headless, video and audio out of the HDMI
  port, running a **pure server distro (latest Debian Trixie)** — **no X11 /
  Wayland desktop**, so playback drives the HDMI connector directly
  (**KMS/DRM**), not through a windowed `autovideosink`.
- **Audio**: HDMI embedded audio, **ALSA-exclusive to CuTePi** — CuTePi is the
  only audio producer on the box.
- **Codecs**: "any video file could be used." Decode **hardware-first
  (v4l2 h264/hevc where available), software fallback** via GStreamer
  autoplugging.
- **Undecodable files**: if a cue cannot start (undecodable/corrupt source),
  playback fails **immediately with an error surfaced to the user**. At
  **import time**, try to establish playability as early as possible (a
  decode probe) and reject early rather than discovering it at cue time.
- **Cue trigger**: pressing **Space** while the control UI is focused and the
  focus is **not** in a text/editing control plays the currently selected cue.
  **Media Pool items are `aria-disabled`/not selectable — the sheet behaves as
  if a cue is always selected** (single-select row is the one Space acts on).
- **Playback end**: a cue **stops** after playback — cues are a list, not a
  playlist; there is no implicit auto-advance. Auto-continue is an explicit,
  per-cue setting.
- **Waits**: `preWait` / `postWait` apply only when a cue is set to
  **auto-continue** — time before the cue starts and time after it ends.
- **Loop / Hold defaults**: `loop=off`, `hold=off` for newly created cues.
  **`loop` always wins over auto-continue**, and looping cues have a **loop
  counter** (`0` = infinite, `N` = play N times); an auto-continuing cue only
  advances once a finite loop count is exhausted.
- **Auto-continue chain**: continues **in sheet order downward**. If the next
  row is a **cue group**, the group's action is triggered and the chain
  continues accordingly.
- **Interruption**: whether partway through a playlist/slideshow a triggered
  cue interrupts depends on **what was triggered and that trigger's own
  settings** — behaviour is driven per-cue/per-group, not globally.
- **Trim** (`posStart` / `posEnd`): the timecode *into the original source
  media* at which the cue starts and ends; `0` at either end = untrimmed at
  that end. Values are **stored as given — no normalization** of an
  out-at-full-duration back to `0`.
- **Cue groups**: visual folders that hold cues, nestable inside other
  folders; a "group of cues" can be triggered as a playlist. Rendered as
  collapsible rows with indentation; drag-reorder works across folder
  boundaries; global order remains the flat `cuePos`, folder membership is a
  presentation layer.
- **Slideshow**: a cue group containing images can enable slideshow mode from
  the group's inspector — plays its images shuffled, looped, with fades, per
  group settings (`duration-per-image`, `fade ms`, `shuffle`, `loop`, all on
  `cue_group`). **Images are visible cue rows inside the group**, with an
  indicator next to the image currently being displayed. New type-column
  icon (folder icon; distinct icon for slideshow-enabled groups).
- **Settings editing**: the "first-connected client may edit" restriction is
  **dropped** — all operators may edit settings.
- **Output level**: system output is always 100%; volume remains a per-cue
  control only (no global/master volume).
- **Missing source media**: the Media Pool shows a warning-triangle icon on
  entries whose source file is gone; cues referencing a missing file show a
  warning **offering both actions — delete the cue, or choose a replacement
  file to re-link**. Detection runs once at **startup** (no periodic scan).
- **Show export/import**: a `.CTP` file — a ZIP containing a JSON manifest of
  all cue information (including the playback **audit trail**) plus the
  relevant Media Pool content. **Import restores a show from a `.CTP` with a
  choice in the modal: append to the end of the current cuesheet or overwrite
  it.**
- **Logs**: viewable from the Web UI with level filtering and a clear action.
  Changing the level in the UI changes **what is recorded** (runtime level
  switch — write less during a show), not just what is displayed. Structured
  playback events (cue started/stopped at wall-clock time) are recorded as an
  **audit trail** and included in the .CTP export.
- **yt-dlp**: URL import is a core, imperative feature (not a convenience).

## Layout (current)

```
config/config.go      config load/save, path expansion, defaults
ctp/ctp.go, ctp/db.go  core domain types + sqlite schema/access
gsp/gsp.go             GStreamer pipeline management
routes/*.go            HTTP handlers (index, api, upload, youtube, install, public)
templates/*.html       server-rendered HTMX fragments + pages
public/                static assets: css/, src/ (js), icons/, img/
main.go, network.go    entrypoint / server wiring
pre.sh                 go mod tidy / go get / test bootstrap script
```

## Top Bar

Left: the 80s corporate-style "CuTePi" logo, Upload button, Download from YouTube/URL
button (any yt-dlp-compatible URL).
Right: Play, Pause, Stop, Panic, Show Test, Settings dropdown → Settings modal.
The Settings modal also selects the browser-local visual theme: LCARS, QLab, or
Future SciFi (the default).

"Show Test" opens a modal ("test is loaded"); "Hide test" button closes it.
Playback controls (play / toggle-pause / stop / panic / fade-out) are wired to
their `/api/*` endpoints.

**Restart / Shutdown** (implemented 2026-09-01): the Settings dropdown also
holds Restart and Shutdown items, each behind a native `hx-confirm` dialog.
`POST /api/restart` spawns a detached copy of the binary that waits for the
parent to release the port before starting (env `CUTEPI_RESTART_WAIT`);
`POST /api/shutdown` SIGTERMs the process so `main.go`'s graceful-DB-close
handler runs. Both respond 200 before acting. Self-managed only: under
systemd, restart is `systemctl restart cutepi`.

**QR button** (implemented 2026-08-31): a TopBar button opens a modal showing a QR
code encoding the host-derived `/upload` URL, so an operator can scan it with a
phone to open the mobile upload page. This **replaces** the original "visiting
`/` on mobile shows an error pointing at `/upload`" requirement — no UA
sniffing is done.

## Media Pool (left pane)

- 2-column thumbnail gallery, 16:9 thumbnails for all types.
- Video thumbnail: frame at 5s. Image: copy of the photo. Audio: waveform
  (ffmpeg `showwavespic`), stored like other thumbnails.
- Each item: a 3-dot dropdown menu with **Play** (`POST /api/play/:filename`),
  **Load** (`POST /api/load/:filename`), **Add** (as a cue), **Delete** (opens
  an "are you sure?" confirm modal), and **Refresh thumbnail**. No inline
  play/delete buttons — the dropdown was chosen to avoid accidental playback.
- Placeholder SVGs per media type shown while thumbnail generation is pending;
  the pending queue is persisted in the DB so it survives restart.
- Filters: filename search + type dropdown (video/image/audio/all, default All),
  separate controls.
- Sort: newest first (date added).
- Drag-and-drop upload directly onto the pool; multi-file upload supported.
- Drag from pool into CueList supported.
- Scrollable, scrollbars always visible.
- **Missing source** (implemented 2026-09-05): at **startup** a scan flags
  media rows whose source file is absent from disk; the pool tile shows a
  warning-triangle icon. Cues referencing a missing file show a warning in the
  row/inspector offering **delete the cue** or **choose a replacement file**
  (re-links `media_id`). No periodic scanning.

## Cue List (right pane)

- Table with sticky header, scrollable body, scrollbars always visible.
- Columns: Cue No, Cue Name, Cue Pre-Wait, Cue Duration, Cue Post-Wait,
  Actions (play, delete, move up, move down). Column widths user-adjustable
  (width only, not order).
- All cells editable via double-click; save on blur/enter.
- Time fields (Pre-Wait/Duration/Post-Wait): `hh:mm:ss.ms`; a bare number
  entered is interpreted as seconds.
- **Cue Inspector** (implemented 2026-08-31): a collapsible panel docked at the
  bottom of the screen showing the selected cue's details — name, media, trim
  In/Out, Hold toggle, waits. It hosts the trim and Hold features below.
- **Trim (start/end)** (implemented 2026-08-31): per-cue `In`/`Out` times edited in
  the Cue Inspector and stored in the existing `posStart`/`posEnd` columns. On
  play the pipeline seeks to `In` and auto-stops when it reaches `Out`;
  `In = Out = 0` plays the whole clip (no trim).
- **Hold last frame** (implemented 2026-08-31): a per-cue toggle (`hold`
  column, **default off per 2026-09-04**, previously default 1) in the Cue
  Inspector. When set, a video cue's final frame stays on screen after
  end-of-stream until Stop/Panic; image cues display as-is.
- Reordered via drag-and-drop (mouse only, no keyboard reorder) and via
  move-up/down actions; persisted immediately to DB. Drag-and-drop reorder
  (implemented 2026-08-31): drag rows between rows or to the end with the existing
  purple drop-line indicator; persisted via `PUT /api/cue/reorder` — the client
  sends the full ordered list of cuePos and the server reindexes 1..N in one
  transaction.
- Drop targets: between rows or at end, with a visual line indicator.
- Row selection: single-select only (arrow-key navigable), representing the
  one cue that can play next. DECIDED 2026-08-31: persisted to the DB (a
  `selected` marker on the cue row) so it survives a reload and is shared
  across clients; it is what the Play action / spacebar acts on.
- Cues reference media by unique media ID, not filename.
- Deleting a media item that is referenced by cues hard-deletes those cues
  (after a confirmation warning to the user).
- Cue deletion is a hard delete.

## Uploads

- `/upload`: mobile-friendly page (drag-and-drop + file picker), also usable
  as a modal from the desktop UI. On completion, modal closes and Media Pool
  updates.
- `/` (main control UI) is desktop-oriented. REPLACED 2026-08-31: instead of
  UA-sniffing and gating `/`, the TopBar has a **QR button** opening a modal
  with a QR encoding the host-derived `/upload` URL, so a phone can scan it to
  open the mobile upload page.
- No file size limit; reject non-media (non video/image/audio) files.
- Metadata (title, duration, resolution, codec) extracted synchronously at
  upload time but without blocking the HTTP response to the client (UI stays
  responsive); title defaults to filename if unextractable. If duration,
  resolution, or codec fail to extract, the import is rejected/fails with an
  error.

## Settings Modal

- Fields (so far): server port, poll interval (ms). More fields later.
- Theme selector: LCARS, QLab, or Future SciFi (default);
  stored locally in the current browser.
- Poll interval: server-side configurable default 100ms, minimum 10ms,
  validated client- and server-side.
- Port change requires restart — modal displays a message, does not restart
  the process itself.
- Only reachable from TopBar (no keyboard shortcut).
- Editing restriction: the original spec's "only the first-connected client may
  edit settings" requirement is **dropped (decided 2026-09-04)** — all operators
  may edit. No session-id mechanism is needed.
- Status (2026-08-31): implemented — `templates/settingsModal.html` wired to
  the TopBar Settings dropdown item, `GET/POST /api/settings` backed, tests in
  `routes/routes_test.go`.

## Data Model (SQLite)

Table names come from the schema in `ctp/db.go` (the source of truth). Note the
design doc's conceptual `media` → **`mediapool`** and `cues` → **`cuesheet`**.

- `mediapool`: `media_id` (unique id), `filename` (unique; path derived from
  config), `mimetype`, `size`, `duration` (float seconds), `resolution`,
  `codec`, `media_title`, `thumbnail_pending`, `waveform` (JSON peaks),
  `waveform_pending`, `date_added`. Sorted
  newest-first by default (`date_added DESC, media_id DESC`).
- `cuesheet`: order number `cuePos` (unique, reindexed on reorder; also a
  reorderable row order, not a separate stable id), `cueNum` (unique label),
  references media by `media_id` (FK → `mediapool`, ON DELETE CASCADE), `title`,
  the editable times `preWait`, `cueDuration`, `postWait` and trim points
  `posStart`/`posEnd` (INTEGER milliseconds, `0` = untrimmed at that end),
  `hold` and `loop` (0/1, **both default 0 per 2026-09-04**), `loop_count`
  (0 = infinite, default 0), `autoContinue` (0/1, with auto-continue), and
  `parent` (`cue_group.group_id`, 0 = top level) holding a cue's group
  membership, and the `state` table's persisted selected cue position.
- `cue_group` (implemented 2026-09-05): nestable folders (`parent_group_id`)
  grouping cues **via `cuesheet.parent` = `group_id`** (folder membership is a
  presentation layer over the flat `cuePos` order — no cue re-impact);
  slideshow settings live here (`duration_ms`, `fade_ms`, `shuffle`, `loop`,
  `slideshow` enabled flag, `collapse`); groups can be triggered as a playlist
  or slideshow. Empty groups render as trailing header rows so they stay
  discoverable and deletable.
- Configuration file: port and poll interval persist in
  `~/CTP/config/config.json`; the theme is browser-local presentation state.
- Thumbnail/waveform generation queue: must be persistent across restarts
  (implemented as the `thumbnail_pending` DB column + the `worker` package).

## Playback / GStreamer

- Pipelines must be uniquely tracked; starting a new pipeline must stop/replace
  the old one without losing the reference to it (known past bug — see
  Known Issues).
- Test-pattern playback supported via GStreamer.
- **Trim** (implemented 2026-08-31): on Load, seek to the cue's `posStart`; reaching
  `posEnd` is treated as end-of-stream (auto-stop). No trim when both are 0.
- **Hold last frame** (implemented 2026-08-31): on end-of-stream of a held video
  cue, seek to the final frame and pause instead of clearing, so the last frame
  remains on screen; Stop/Panic still tear the pipeline down.
- **Loop** (implemented 2026-09-01): `gsp.SetLoop` / `POST /api/loop`. On
  end-of-stream (natural EOS or the trim Out reached), a looping clip seeks
  back to its in-point and plays on rather than stopping or clearing. The
  default for newly loaded clips is `config.Loop()` (a `loop` bool in
  `config.json`), toggled live from the Now Playing widget. **Default is
  `off`** per 2026-09-04.
- **Loop counter** (implemented 2026-09-04): a `loop_count` column
  (0 = infinite, N = play N times). `loop` always wins over auto-continue — a
  looping cue only auto-advances once a finite count is exhausted.
- **Auto-continue & waits** (implemented 2026-09-04): a cue can be
  flagged `autoContinue`. `preWait` and `postWait` only take effect for
  auto-continuing cues — a pause before the cue starts and after it ends.
  Auto-continue advances in sheet order downward; a cue-group row triggers the
  group's action (playlist/slideshow) and the chain continues from there. A
  non-auto-continuing cue always stops on EOS (no implicit advance); the list
  is a cue list, never a playlist.
- **Decoding / undecodable** (implemented 2026-09-05): pipelines
  use GStreamer autoplugging resolved **hardware-first** (v4l2 h264/hevc where
  available) with **software fallback**. If a cue cannot start because its
  source is undecodable/corrupt, fail immediately and surface the error to the
  user. Import includes an early playability probe (decode test, `ffmpeg -v
  error -t 1`) so bad files are rejected at import time.
- **Output** (decision 2026-09-04): video via KMS/DRM (headless Debian Trixie,
  no display server); audio via HDMI embedded ALSA, exclusive to CuTePi.
- **Volume** (implemented 2026-09-01): the audio playback chain is now
  `queue → audioconvert → audioresample → volume → autoaudiosink`. The manager
  retains the branch's `volume` element (last audio branch wins) and applies
  the cue's per-cue master gain on link (there is **no global master**).
  Volume is stored and edited **in dB** (0 = 0dB, range −60…+12), converted to
  linear gain (`10^(dB/20)`) for the GStreamer `volume` element via
  `gsp.dbToGain`. The Cue Inspector exposes it as a vertical slider with a dB
  readout; `gsp.SetVolume(dB)` drives it live and callers persist the clamped
  value onto the cue's `volume` column (default 0 = 0dB). This also gives
  FadeOut a real gain stage to target if it's ever extended to a gradual fade.
- **Seek** (implemented 2026-09-01): `gsp.Seek(seconds)` / `POST /api/seek`.
  Absolute seek clamped to the clip's duration and its trim Out. The Now
  Playing scrubber live-seeks; the change poller skips re-renders while the
  thumb is `:active` so an active drag isn't interrupted.
- **Trim timeline** (implemented 2026-09-01): each audio-bearing media item
  gets its amplitude envelope stored in the DB as a JSON peak array — computed
  by the thumbnail worker (`media.GeneratePeaks`, a fixed 300-bucket envelope
  normalized to 0..1) and stored via `ctp.StoreWaveform`. The Cue Inspector
  draws those peaks onto a `<canvas>` and overlays draggable In/Out markers on
  it; dragging/clicking writes back into the existing trim fields, so Save
  still uses `PUT /api/cue/inspector/:cuePos`. The Media Pool dropdown
  "Analyse" action re-flags a file (`ctp.RequestWaveformAnalysis`) to rebuild
  the peaks. Timeline falls back to an empty track when no peaks exist.
- **Inspector follows selection** (implemented 2026-09-01): because the
  cuesheet is re-rendered by four different paths (htmx swap, the change
  poller, WebSocket sync, and drag-and-drop) and only one of them fires
  `htmx:afterSwap`, the inspector refresh is now a `MutationObserver` on
  `#cuesheet` that re-fetches `GET /api/cue/inspector` whenever a fresh
  tbody appears — covering all four paths with one hook.

## Synchronization (implemented 2026-08-31)

- **Server-authoritative**: the server holds the authoritative playback,
  cuesheet, and selection state; browsers are pure mirrors that render whatever
  the server sends. The DB stays the source of truth and hub messages are
  derived from it.
- Go WebSocket hub (`/api/ws`) broadcasts sync events for transport and state
  (load / play / pause / stop / panic / test / hold / position clock), the
  cuesheet, and the selection to every connected client, keeping all open
  browsers in sync.
- The existing change-detection path (`GET /api/nowplaying/status` + JS polling)
  is kept as the reconnect/fallback while no WS connection is established.
- Client-side split: playback/cuesheet/selection state is server-side; purely
  cosmetic UI chrome (column widths, panel collapse/width) stays in
  `localStorage`.

## Show Export / Import (.CTP) (implemented 2026-09-05)

- A `.CTP` file is a **ZIP** containing a **JSON manifest** (`cutepi.json`:
  `app`/`version`/`exportedAt`, cuesheet incl. order/selection, per-cue
  settings, and the playback **audit trail**) plus the **referenced Media Pool
  content** (files present on disk; `media/<filename>`).
- **Export** (`GET /api/show/export`): produces that ZIP for archiving or
  moving a show to another Pi.
- **Import** (`POST /api/show/import` via the show modal): restores a show
  from a `.CTP` with **append to the end of the current cuesheet** or
  **overwrite** (replaces the current cuesheet). All referenced media is
  validated as available (inside the .CTP or already in the local pool)
  **before** any mutation; cue titles and `cueNum` labels are deduped on
  import.

## Logs & Audit Trail (implemented 2026-09-05)

- **Log viewer in the Web UI** (`GET /api/logs`, terminal-style modal):
  filter/record by level (debug/info/warn) and a clear action. The level
  selector switches the **recording** level (runtime toggle), so a show can
  run with fewer logs written.
- **Audit trail** (`logs.Emit("cue_start"|"cue_end")`): structured playback
  events (pos/title at wall-clock time) are recorded in an append-only ring
  and **included in .CTP exports**. Clearing the log never clears the audit.

## Testing

- Go unit tests required for DB and API endpoints, with verifiable output.
- `pre.sh` should fully bootstrap: `go mod tidy`, `go get`, run tests.

## Error Handling

- Use distinct, loggable error codes for troubleshooting across backend
  operations (DB, ffmpeg/yt-dlp, GStreamer, uploads).

## Known Issues / Audit Findings (2026-08-28 full audit)

Zero `*_test.go` files exist anywhere in the repo — confirmed no test coverage.
`routes/install.go`, `routes/upload.go`, `routes/youtube.go` are 13-line
stubs; `gsp.go` has multiple stub/no-op functions. Ranked by priority:

1. **`gsp/gsp.go` global `pipeline`/`bus` vars have no mutex.** `Load()`/
   `ShowTest()` do check-nil/stop/nil/rebuild non-atomically — concurrent
   calls race, old pipeline can be orphaned/leaked with an active sink still
   running. This is the root cause of the "multiple pipelines"/"losing
   reference" bugs from prior commits; not actually fixed despite the
   "Fixed multiple pipelines happening at once" commit message.
2. **`ctp.CuesheetLength` is declared but never assigned anywhere** →
   `NextCue()`'s bound check (`CurrentCue+1 < CuesheetLength`, i.e. `< 0`) is
   always false, so Next-cue can never advance. `PrevCue()` has an off-by-one
   (`CurrentCue-1 > 1`) blocking navigation to cue 1.
3. **DB path init-order bug**: `ctp/db.go`'s `init()` opens the sqlite DB
   using `config`'s package-init env-var defaults, before `main()` calls
   `config.LoadConfig()`. Any DB path set in `config.json` is silently
   ignored.
4. **First-run crash risk**: no `os.MkdirAll` before creating the config file
   or DB file — fresh installs where `~/CTP/config/` doesn't exist yet will
   panic (db.go) or silently fail (config.go `SaveConfig`).
5. **SQL injection**: `ctp.UpdateCue` string-concatenates the `col` parameter
   (sourced from an unvalidated URL path segment) directly into an `UPDATE
   ... SET <col> = ?` statement with no allow-list.
6. **`DELETE /api/cue/:cuePos` is broken**: handler passes the numeric
   `cuePos` to `ctp.RemoveCue(filename string)`, which looks up by
   `mediapool.filename` — deleting a cue via the UI silently no-ops.
7. **`/upload` and `/youtube` have no POST handlers** — only stub `GET /`
   pong routes exist. Client-side forms already POST to these paths and will
   404. Matches the "need to reimplement upload and db processing" commit.
8. **No ffmpeg/yt-dlp integration exists at all in Go code.**
   `gsp.GetMimeType/GetFileSize/GetDuration` are non-functional stubs
   (mimetype = raw filename, size/duration always 0) — every uploaded file
   gets garbage metadata today.
9. **`gsp.CurrentPlaying()` always returns `""`** — the "don't delete the
   file that's currently playing" guard in the media-delete route is a no-op.
10. **Template context/casing bugs**: `/api/clear` populates `gin.H{"cuesheet":
    ...}` (lowercase) but `cuesheet.html` ranges over `.Cuesheet`
    (capitalized) — post-clear re-render silently shows an empty table.
    `index.html` doesn't pass context into `{{ template "cuesheet.html" }}`
    at all, so the cuesheet is always empty on first page load.
11. `routes/index.go` renders hardcoded fake mediapool sample data instead of
    querying the DB — home page never reflects real state on load.
12. Dead/orphaned code: `gsp.bus` var, `monitorProgress/queryPosition/
    queryDuration/formatTime` (unreachable), `templates/dropzone.html`
    (not referenced by any other template — `uploadModal.html` is the one
    actually used, and duplicates its DOM ids/logic), `gsp.Next()/Prev()`
    (empty, wired to dead buttons; real cue-advance lives in
    `ctp.NextCue/PrevCue` instead — two disconnected "next/prev" concepts).
    `buildFilePipeline`/`buildTestPipeline` in gsp.go are ~90% duplicated.
13. Keyboard shortcut for spacebar (`cuesheet.html`) posts to
    `/api/cue/play`, which does not exist (only `/api/play` does) — 404s
    silently.
14. `mediapool.html` listens for a custom `mediapool-updated` DOM event that
    nothing in the codebase ever dispatches — dead wiring intended for
    post-upload refresh.
15. Inconsistent logging (`fmt.Println` in routes/api.go vs `log.Printf`
    elsewhere); mixed indentation in ctp.go/gsp.go.

## Overhaul Plan (update as items complete)

- [x] Fix startup/config ordering: DB init is now called explicitly from
      `main()` after `config.LoadConfig()`; `os.MkdirAll` creates config/db/
      media/thumbnail directories on first run.
- [x] Rewrite `gsp` pipeline manager: single mutex-guarded pipeline handle
      (`manager` struct), atomic stop-then-replace via `swap()`, EOS/error
      now clears the manager's reference (`clearIfCurrent`), pipeline-file
      vs pipeline-test-pattern building de-duplicated into one
      `buildPipeline()`. Dead code (`bus` var, `monitorProgress`,
      `queryPosition/queryDuration/formatTime`, empty `Clear()`) removed.
- [x] Fix `ctp` cue navigation (`CuesheetLength` replaced with a live
      `SELECT COUNT(*)`, `PrevCue` off-by-one) and the
      `DELETE /api/cue/:cuePos` filename/cuePos mismatch (`RemoveCue` now
      takes a cue position). Regression tests added in `ctp/ctp_test.go`.
- [x] Allow-list `UpdateCue`'s column parameter (`editableCueColumns`) to
      close the SQL injection; regression test added.
- [x] Implement real `POST /upload` (multipart, extension + ffprobe
      validated, rejects on unextractable metadata) and `POST /youtube`
      (yt-dlp) handlers, replacing the stub `GET /` "pong" routes.
- [x] Implement real ffmpeg-backed metadata (`media.Probe`) and
      thumbnail/waveform generation (`media.GenerateThumbnail`), replacing
      the `gsp.Get*` stubs. Thumbnail generation runs in a background
      worker (`worker.RunThumbnailWorker`) driven by a `thumbnail_pending`
      DB column, so pending work survives a restart per spec.
- [x] Fix template context/casing bugs (`Cuesheet` key on `/clear`,
      `index.html` context passing) and wire real DB-backed data into
      `routes/index.go` (mediapool + cuesheet, no more hardcoded samples).
- [x] Add Go unit tests for `config` and `ctp`; `go build`/`go vet`/
      `go test ./...` all pass cleanly.
- [x] Update `pre.sh` to run `go mod tidy`, `go build`, `go vet`,
      `go test ./...`, and install ffmpeg/yt-dlp alongside GStreamer deps.
- [x] Added `GET/POST /api/settings` (port + poll interval, validated,
      port change requires restart per spec) and a per-item
      `POST /api/media/:filename/refreshThumbnail` route.
- [x] Graceful shutdown (SIGINT/SIGTERM closes the DB) added to `main.go`.
- [x] Remove remaining dead code/templates. **Implemented 2026-08-31.** The
      items: `templates/dropzone.html`
      is an orphaned duplicate of `uploadModal.html`'s dropzone markup/JS and
      is never rendered by any template — safe to delete. `gsp.Next()/Prev()`
      are empty no-ops wired to toolbar buttons that are functionally distinct
      from `ctp.NextCue/PrevCue` (cuesheet advance) — decided: remove the
      methods and buttons (cue navigation keeps using the ctp-backed routes),
      rather than unifying them.
   - [x] `mediapool.html`'s `hx-trigger="mediapool-updated from:body"` listener
      has no dispatcher anywhere; upload/youtube handlers currently
      re-render the mediapool by returning the partial directly via
      `hx-target="#mediapool"` instead, so this dead listener can likely be
      removed, but drag-and-drop-triggered refreshes (spec: DnD from pool
      into cuelist, and DnD upload) still need frontend wiring/testing in a
      browser.
   - [x] Column width persistence for the CueList, drag-and-drop reordering
      persistence, inline double-click editing polish, and the mobile
      `/upload` page vs desktop-only `/` gate are still per the original
      spec but not yet re-verified against the current backend changes in
      this pass — frontend wasn't rewritten, only the routes/templates it
      talks to were fixed where broken.
  - [ ] No live GStreamer/ffmpeg/yt-dlp hardware smoke test was run in this
      session (sandboxed environment); `go build`/`go vet`/`go test` all
      pass, but real playback and downloads should be verified on the
      actual Raspberry Pi target.

### Planned features (decided 2026-08-31 — implemented)
- [x] Cue Inspector panel (collapsible, docked at the bottom of the screen).
- [x] Cue trim (`In`/`Out`) via the Inspector; gsp seek-to-`In` +
      auto-stop-at-`Out`.
- [x] Per-cue Hold (last frame) — `hold` column + freeze-on-EOS.
- [x] Drag-and-drop cue reorder + `PUT /api/cue/reorder`.
- [x] QR-code button for `/upload` (replaces the mobile gate).
- [x] WebSocket hub — server-authoritative multi-browser sync.
- [x] Persisted single-select cue rows (arrow-key navigable, "plays next").
- [x] Dead-code cleanup — delete `templates/dropzone.html`, the
      `mediapool-updated` listener/trigger, and the empty
      `gsp.Next()`/`Prev()` plus the Prev/Next toolbar buttons and
      `/api/next`|`/api/prev` routes.

## Functional Test Pass (2026-08-28) — bugs found & fixed while testing live

Ran the compiled server end-to-end (isolated `WORKING_DIR`, real ffmpeg/
ffprobe/yt-dlp/GStreamer installed) and exercised upload, mediapool,
cuesheet CRUD, settings, and playback endpoints with `curl`/`sqlite3`. This
surfaced several bugs that unit tests didn't catch (they need a live
template render or a live ffmpeg run to trigger) — all fixed and re-verified
live:

- **Cuesheet always rendered empty.** `cuesheet.html` did
  `{{ range .Cuesheet }}` but every handler passes `Cuesheet` as the
  `ctp.Cuesheet` struct (`{Cues []Cue}`), not a slice — Go's `html/template`
  raised "range can't iterate over ..." on every single render, including
  the very first page load, and gin had already flushed a 200 with the
  partial output before the error aborted the rest of the body. Fixed to
  `{{ range .Cuesheet.Cues }}`; verified a cue row now renders correctly
  with the right selected-highlight behavior.
- **Inline cell editor pre-filled with garbage.** The GET-equivalent
  handler for `/api/cue/:cuePos/edit/:col` passed the *entire* `Cue` struct
  as the form's value instead of the one column being edited, so
  double-clicking a cell showed something like
  `{{0 file.mp4 video/mp4 ...} 1 1 1 1 title 0 0 false}` in the input
  instead of the current title. Added `ctp.CueColumnValue()` (using the
  same column allow-list as `UpdateCue`) and wired the handler to it;
  verified the edit form now pre-fills the actual current value.
- **`ConfigFilePath`'s default ignored `WORKING_DIR`.** It was computed
  independently from the home directory before `WorkingDir` was resolved,
  instead of being derived from it like `Db`/`Media`/`Thumbnails` already
  were. Setting `WORKING_DIR` (or relocating everything else via env vars)
  silently left `config.json` writes going to the real `~/CTP/config/`
  instead of the intended location — caught when a `POST /api/settings`
  call wrote to the sandbox's real home directory instead of the isolated
  test one. Fixed by resolving `WorkingDir` first and deriving
  `ConfigFilePath`'s default from it; verified `config.json` now lands in
  the configured working directory.
- **Video thumbnail generation always seeks to a hardcoded 5s.** Any
  uploaded clip shorter than 5s (a very normal case) made every ffmpeg
  thumbnail attempt fail (seek past EOF), and since the worker retried
  every poll tick (2s) forever, this became an infinite failure loop
  burning CPU. Fixed `media.GenerateThumbnail` to take the clip's probed
  duration and use `duration/2` as the seek point for anything under 10s;
  also added a 60s per-item backoff in the worker so a persistently-failing
  file doesn't get retried on every single tick regardless of cause.
  Verified live: a 3s test clip now gets a correct 640×360 thumbnail.
- Hardened the thumbnail ffmpeg filter chain with `format=yuvj420p` at the
  end, since the mjpeg/JPEG encoder can reject unusual source chroma
  formats (e.g. 4:4:4) otherwise — found via a synthetic test clip, but a
  real robustness gap for arbitrary real-world uploads too.

### Confirmed working end-to-end in this pass
Directory/DB/config auto-creation on first run; multipart upload → ffprobe
metadata extraction → mediapool registration → background thumbnail
generation (16:9, correct dimensions) with the pending flag correctly
clearing; upload rejection of non-media file types (422); cue add/select/
edit/delete against a real DB; `DELETE /api/media/:filename` refusing to
delete the currently-loaded file (409) and actually deleting + cleaning up
the media file and thumbnail otherwise; `/api/settings` validation
(rejects sub-10ms poll interval) and persistence; GStreamer test-pattern
and play/pause/stop/panic endpoints returning promptly without blocking or
crashing the server even when no video sink is available (headless test
environment).

### Newly identified gaps (not fixed — need a product decision, not a bug fix)
- **No backend endpoint exists for cue reordering.** The spec requires
  drag-and-drop reorder and move-up/move-down actions in the CueList,
  persisted immediately, but there is no route or `ctp` function that
  changes a cue's `cuePos` relative to others (`AddCue` shuffles positions
  on insert, but nothing supports a plain reorder). This needs to be
  designed and implemented as a feature, not patched as a bug.
- **The cuesheet has no per-cue "duration" column**, only `posStart`/
  `posEnd` (trim points). The UI template has an edit control wired to
  `/api/cue/:cuePos/edit/duration`, which the column allow-list now
  correctly rejects (500) rather than silently corrupting a query — but
  the spec calls for an editable "Cue Duration" column, and it's unclear
  whether that should be a stored field or computed as `posEnd - posStart`.
  Needs a decision before it can be implemented.
## Test Extension Pass (2026-08-28) — bugs found while writing tests

Extending test coverage (not just re-testing existing behavior) surfaced
three more real, previously-unknown bugs, all fixed:

- **SQLite foreign keys were never enabled on the connection.** The
  `cuesheet.media_id` column declares `ON DELETE CASCADE`, but SQLite does
  not enforce foreign keys per-connection unless explicitly turned on.
  Deleting a media item referenced by a cue left an orphaned cuesheet row;
  `GetCuesheet`'s `LEFT JOIN` then produced NULL media columns, which
  crashed the scan for the *entire* cuesheet (not just the orphaned row).
  Fixed by opening the DB with `?_foreign_keys=on` in `ctp/db.go`; a new
  test (`TestDeleteMediaCascadesToCues`) locks this in.
- **`AddCue`'s "insert at position N" logic used `cuePos > ?` instead of
  `>=`.** Inserting a new cue at a position already occupied by an existing
  cue never bumped that existing cue out of the way, so the subsequent
  `INSERT` collided with it on the `cuePos` UNIQUE constraint. Worse, once
  changed to `>=`, a *single* set-based `UPDATE` bumping several rows still
  hit a transient UNIQUE violation, because SQLite enforces UNIQUE
  per-row during statement execution, not at commit - bumping position 1 to
  2 while position 2 is still occupied fails, regardless of the final state
  being valid. Fixed by bumping affected rows individually, highest
  position first, inside a transaction. Covered by
  `TestAddCueAtPositionBumpsExisting`.
- **Mediapool "newest first" sort had unresolvable ties.** `date_added
  DEFAULT CURRENT_TIMESTAMP` only has 1-second resolution, so a multi-file
  upload (explicitly a required feature) can register several rows with an
  identical timestamp, leaving their relative order effectively random.
  Fixed by adding `media_id DESC` as a tiebreaker in `GetMediapool`'s
  `ORDER BY`. Covered by `TestGetMediapoolSortedNewestFirst`.

To make the WORKING_DIR-derivation bug (found in the earlier live test
pass) and the ffmpeg seek-clamp/backoff logic properly regression-testable
without needing a live server or real ffmpeg, `config.init()`'s
path-resolution logic was extracted into a pure `resolveDefaults(getenv,
homePath)` function, the thumbnail seek-clamp into a pure
`media.thumbnailSeek(duration)`, and the worker's retry suppression into an
injectable-time `failureTracker` type. All three are now directly unit
tested. HTTP-level regression tests were also added
(`routes/routes_test.go`) that render the real templates against a real
in-memory DB, reproducing the two template bugs found in the live pass
(empty cuesheet render, garbage inline-edit value) so they can't silently
regress again - unit tests on `ctp` alone couldn't have caught either, since
both were template-layer bugs.

Full suite (`go test -race ./...`) passes: `config`, `ctp`, `media`,
`worker`, and `routes` all have coverage now; `gsp` still has none (it
needs a real GStreamer pipeline/sink to test meaningfully and wasn't
covered in this pass - see Suggested Improvements below).

- **Pre-Wait/Duration/Post-Wait display is hardcoded placeholder text**
  (`00:00`, `00:30`) in `cuesheet.html` rather than formatted from the
  actual `posStart`/`posEnd` values in `hh:mm:ss.ms` per spec — the raw
  values are shown alongside as debug output (`// {{ .PosStart }}`). Needs
  a real time-formatting helper and the "bare number = seconds" parsing
  rule from the spec, on both render and edit-submit.

## Open Questions

- 2026-09-04: the hardware/UX/product questions below were **answered** and are
  recorded in "Product Decisions"; this section is now for anything still open.

Remaining open:
- Pi validation ownership: whether a `./smoke-test.sh` (generate assets with
  ffmpeg, exercise upload → pool → cue → play → trim → stop via curl against
  the HDMI sink) should ship so hardware validation is one command.

Resolved:
- **Config `loop` default (2026-09-06):** cue loads already always honour the
  cue's own flag — `loadAndPlayCue` passes `Loop: cue.Loop` straight into
  `LoadWithOpts`. `config.Loop()` is only consulted for *direct* pool-loads
  (`gsp.Load`) and as the reported/inspected default when no pipeline is
  loaded (the Settings Loop toggle persists it). New cues default loop off
  (registration does not seed). Keep it as the direct-load default; not
  removed, no registration seeding.

## UI Pass (2026-08-28)

- **Layout**: the app now fills exactly 100vh (`public/css/comp/layout.css`
  + `templates/index.html`'s `.app-body`/`.app-content`/`.app-pane`
  classes) — the topbar takes its natural height and the content row below
  it takes the rest, with no page-level scroll; only the mediapool and
  cuesheet panes scroll internally (`overflow-y: scroll`, so scrollbars
  stay always-visible per spec). Replaces the previous `90svh` fixed-height
  hack.
- **Drag-and-drop** (`public/src/dnd.js`, new): OS files dropped onto the
  media pool pane are uploaded via `fetch` to `/upload` and the pool is
  swapped in-place; media pool items are now `draggable` and can be
  dropped onto the cuesheet to add a cue at the hovered position, shown
  with a purple line indicator (`.cue-drop-indicator`), using the existing
  `/api/cue/add/:filename/*cuePos` endpoint. Both paths call
  `htmx.process()` on the replaced DOM so existing `hx-*` bindings keep
  working after a plain-JS swap.
- **Icons over text**: the always-visible toolbar buttons (topbar
  Upload/YouTube, and Play/Pause/Stop/Prev/Next/Panic/Fade Out/Show Test in
  `mediacontrols.html`) are now icon-only Bootstrap Icons with `title`/
  `aria-label` for accessibility, instead of text labels. Dropdown menu
  items (Tests/Settings/Restart/Shutdown) were left as icon+text, since
  removing the label from an already-labeled list item doesn't help
  usability the way it does on a toolbar.
- **Now Playing**: `mediainfo.html` now self-polls
  `GET /api/nowplaying` (`hx-trigger="load, every 500ms"`, matching the
  spec's polling cadence) and shows the actual currently-loaded filename
  and `position / duration` (via `gsp.CurrentPlaying/CurrentPosition/
  CurrentDuration`, formatted mm:ss or h:mm:ss), or "Nothing playing" when
  idle. Verified live: shows the correct filename after `/api/play/:file`;
  position/duration stay at 00:00 in this sandboxed/headless environment
  since there's no real audio/video sink for GStreamer to report a clock
  from - expected to update correctly on the real Raspberry Pi with HDMI
  output.
- Clarified (not a bug, documented behavior): `Stop()` deliberately leaves
  the pipeline loaded so `Play()` can restart the same file, which means
  `CurrentPlaying()` - and therefore the delete-guard - still reports that
  file as in-use until `Panic()` (or loading something else) fully
  unloads it. This is intentional: deleting a stopped-but-loaded file out
  from under the pipeline could break a subsequent Play().

## Build Decisions

This section keeps the architecture decisions alongside the product spec; this
file is the single design and implementation record.

### Stack and repository shape

- Go/Gin serves server-rendered HTMX fragments; SQLite via `sqlx` is the source
  of truth; GStreamer drives HDMI playback; ffmpeg/ffprobe handles media data;
  yt-dlp handles URL downloads.
- Client code is HTML5/CSS3 and vanilla JavaScript. Bootstrap, Bootstrap Icons,
  and HTMX are vendored under `public/src/`, so the app has no CDN dependency.
- `config/` owns configuration and schema setup, `ctp/` owns domain queries,
  `gsp/` owns playback, `media/` owns probing/thumbnails, `worker/` owns pending
  thumbnail work, `routes/` owns HTTP handlers, and `templates/` owns pages and
  partials.

### State and browser responsibilities

- Playback, cues, selection, and media-library state are server-authoritative;
  the browser mirrors them through WebSocket sync with change-detection polling
  as fallback. Cosmetic panel sizes, column widths, and theme choice remain
  browser-local in `localStorage`.
- All displayed time fields use `hh:mm:ss.ms`; bare input numbers mean seconds.
- Existing databases are migrated in place. SQLite foreign keys are enabled so
  deleting media cascades to its cues.

### HTMX and logging

- HTMX v4 is vendored and uses explicit attribute inheritance. Successful state
  mutations return the complete target partial with `outerHTML`; status-only
  actions use `none`, and error responses never replace application panels.
- Stable log codes use `[SUBSYS-E###]`: `GSP` for playback, `RTE` for routes,
  `NET` for startup networking, with `YDL` for URL-import stages. Codes are
  greppable and must not be reused for a different meaning.

### Product boundaries

- The desktop `/` page is the control room; `/upload` is the mobile-friendly
  uploader. The QR button links operators to `/upload` without user-agent
  sniffing.
- The visual themes are presentation-only and do not alter playback behavior.
- HyperDeck/Companion and DeckLink/SDI support are out of scope.

## Change Log (design-doc level; see CHANGELOG.md for user-facing changes)

- 2026-08-31: Next-build feature set decided: Cue Inspector panel; cue trim
  (seek-to-`In` / auto-stop-at-`Out`); per-cue Hold; drag-and-drop reorder via
  `PUT /api/cue/reorder`; QR-code button for `/upload` (replaces the mobile
  gate); server-authoritative WebSocket synchronization; DB-persisted
  single-select cue rows; and the approved dead-code cleanup. Tracked in
  "Overhaul Plan".
- 2026-08-31: Polling change-detection implemented: `gsp` gained a monotonic
  state-change counter (`StateVersion()`); new `GET /api/nowplaying/status`
  returns `{changed, version}` and is polled by client JS instead of htmx
  re-rendering the Now Playing widget every 500ms.
- 2026-08-28: Initial design document created, consolidating the full spec
  Q&A into a single trackable reference. Codebase audit against this spec
  in progress.
