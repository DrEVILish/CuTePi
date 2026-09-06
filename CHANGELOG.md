# Changelog

This file is append-only: entries are added as work lands, never removed or
rewritten, so it stays a true history of the project.

## 2026-09-06 — 2026-09-04 feature set delivered (9 commits)

All decisions from the second Q&A round are now built. Commits, newest first:

- `d631b56` **Cue groups + slideshow** — `cue_group` table (name,
  `parent_group_id`, collapse, and slideshow settings `slideshow`/`shuffle`/
  `loop`/`fade_ms`/`duration_ms`); membership rides the existing
  `cuesheet.parent` column, so folder membership is a pure presentation layer
  over the flat `cuePos` order. Group CRUD routes (`/api/group`, incl.
  collapse + inspector), `POST /api/cue/:cuePos/group` membership, and dnd
  drop-on-group-header. The cuesheet renders contiguous group-header clusters
  (folder icon, chevron collapse, play + inspector + delete buttons); empty
  groups render trailing so they stay discoverable. Slideshow play
  (`POST /api/group/:id/play`) runs a goroutine cycling member images in sheet
  order (Fisher–Yates shuffle, loop, per-image hold default 5s, optional fade-
  out between images), aborted the moment anything else plays/stops (cuePos
  identity guard); the playing image's progress row is the now-showing
  indicator.
- `752595c` **.CTP show export/import** — export zips `cutepi.json` (manifest
  v1: version, exportedAt, selected cue, all cues, audit trail) + present
  media files; import validates all referenced media (in-archive or local
  pool) *before* mutating, dedupes titles/cueNums, and appends or overwrites.
- `7ebacc3` TODO.md reflects completed features.
- `9bdfb83` **Web UI log viewer + audit trail** — `logs` ring buffer (1000)
  with debug/info/warn runtime switching and an append-only structured audit
  (`cue_start`/`cue_end`, AUD-E500); `/api/logs` GET/POST-level/DELETE;
  terminal-style modal. First commit of the `logs` package (stray `.gitignore`
  `logs` rule was hiding it).
- `cb55bbc` **Import-time playability probe** — `media.VerifyPlayable`
  (`ffmpeg -v error -t 1`) on single-file upload and yt-dlp import; images are
  exempted (probe+thumbnail suffice).
- `c2aedbf` **Missing-source detection** — startup scan flags lost media; pool
  tiles get a warning triangle; cues offer delete or re-link.
- `f6f8700` **Loop counter + defaults** — `loop_count` (0 = infinite), loop
  wins over auto-continue; new cues default `loop`/`hold`/`loop_count` off/0.
- `520f34f` **Auto-continue + waits** — per-cue `autoContinue`; `preWait`/
  `postWait` only pause for auto-continuing cues; advances in sheet order with
  the cuePos identity guard; `spaceBar`/`ArrowUp`/`ArrowDown` triggers.
- `29dc239` housekeeping.

Still open in DESIGN.md: Q18 (config `loop` default seeding) and Q19
(smoke-test script) — documented, not blocking.

## 2026-09-04 — Product decisions round 2 (docs only, no code)

Refinements from the second Q&A round; recorded in DESIGN.md (Product
Decisions), TODO.md, README.md. No code changed.

- **Platform**: headless Debian Trixie (pure server, no X/Wayland) → HDMI
  video via KMS/DRM; HDMI audio ALSA-exclusive to CuTePi. Hardware-first
  decode (v4l2 h264/hevc) with software fallback.
- **Undecodable**: cue fails to start → immediate error to the user; import
  includes an early playability probe to reject bad files.
- **Trim**: stored as given, no normalization; `0` at either end =
  untrimmed.
- **Loop**: `loop` always wins over auto-continue; new `loop_count` (0 =
  infinite, N times). Auto-continue advances in sheet order down, triggering
  group actions when the next row is a group.
- **Selection**: Media Pool tiles are not selectable; the sheet always has a
  selected cue (Space acts on it).
- **Interruption**: playlist/slideshow interruption is driven by whatever cue
  is triggered and that trigger's own settings.
- **Groups/slideshow**: images are visible cue rows inside the group with a
  now-showing indicator; slideshow settings live on `cue_group`; folder icon
  + distinct slideshow icon.
- **Missing source**: startup-only scan; cues offer delete or re-link to a
  replacement file.
- **.CTP**: export + **import** (import modal chooses append-to-end or
  overwrite); manifest includes the playback audit trail.
- **Logs**: level selector switches the *recording* level (write less during
  a show); structured audit trail (cue started/stopped, wall-clock) included
  in exports.

## 2026-09-04 — Product decisions recorded (docs only, no code)

All open hardware/UX/product questions answered by the operator; recorded in
DESIGN.md (Product Decisions) and scoped in TODO.md / README.md. No code
changed.

- **Platform**: Raspberry Pi 4/5, headless, video + audio out of the HDMI port;
  any video codec must play.
- **Trigger**: Space (focused UI, not editing) plays the selected cue.
- **End behavior**: cues stop after playback; auto-continue is explicit and is
  the only place `preWait`/`postWait` apply. Defaults `loop=off`, `hold=off`.
- **Trim**: `posStart`/`posEnd` are timecodes into the source media.
- **Cue groups**: nestable visual folders that can act as playlists; slideshow
  mode plays a group's images shuffled/looped/faded (group settings).
- **Dropped**: the "first-connected client may edit settings" restriction; all
  operators may edit. System output is always 100% (no master volume).
- **To build**: missing-source warnings (Media Pool + cues), `.CTP` show export
  (ZIP: JSON manifest + referenced media), Web UI log viewer (levels + clear),
  Space trigger, auto-continue/waits, cue groups, slideshow.

## 2026-09-04 — Waveform in the database

### Added
- **Waveform data is now stored in the DB** instead of a `.wave.png` file.
  `mediapool` gains `waveform` (JSON amplitude-peak array, 300 buckets
  normalized 0..1) and `waveform_pending` columns. Peaks are computed on media
  import by the thumbnail worker via `media.GeneratePeaks` (mono 100 Hz sample
  stream decoded with ffmpeg, bucketed and normalized) and stored with
  `ctp.StoreWaveform`.
- **"Analyse" action in the media pool dropdown** re-flags a file's peaks for
  regeneration via `ctp.RequestWaveformAnalysis` (`POST /api/media/:filename/analyse`),
  picked up by the worker.
- **Cue Inspector renders the waveform from the DB** on a `<canvas>` drawn from
  the stored peaks, so the trim timeline no longer depends on a generated PNG.

### Changed
- The thumbnail worker's pending query now covers both `thumbnail_pending` and
  `waveform_pending` rows; `processOne` branches per flag.
- Removed `media.GenerateWaveform` and the `<filename>.wave.png` build; the
  inspector and worker no longer reference the old PNG waveform.

## 2026-09-03 — htmx v2→v4 migration fix: caller-side JS was left on v2 APIs

htmx was upgraded to v4.0.0 (bundled, `public/src/htmx.org@4.0.0`) but the
app's JS/templates still used v2 event names and detail shapes, so several
client behaviours silently stopped firing.

### Fixed
- **Cue inspector didn't follow cue-sheet row clicks.** Inspector auto-refresh
  listened for `htmx:afterSwap` and read `e.detail.target` — v4 renamed the
  event to `htmx:after:swap` and moved swap info to `e.detail.ctx.target`.
  Rewrote the listener to v4 shapes (`ui.js`). A stale GET could also render
  the previously selected cue, so the inspector refresh is debounced and
  cache-busted, and `GET /api/cue/inspector` now emits `Cache-Control: no-store`.
- **Timeline trim-out drag also moved trim-in.** The trailing click after a
  drag reached the timeline click handler. Now `pointermove` sets a `dragged`
  flag and `pointerup` installs a one-shot capture-phase click suppressor so
  the post-drag click is swallowed (`cueinspector.html`).
- **Other `hx-on:htmx:after-request` handlers were dead** (never fire under
  v4): Settings save/status, Upload modal close, YouTube modal close/error
  text, Test modal open/close, Delete modal close. Replaced the inline v2
  attributes with a single `htmx:after:request` listener in `ui.js` using
  `e.detail.ctx.sourceElement` / `request.status` / `ctx.text`, and keyed off
  element ids. Removed the now-unused inline handlers from the templates.
- **Test updated** for the new markup (`id="showTestBtn"` replaces the v2
  `hx-on:htmx:after-request`).

## 2026-09-03 — Deployment test + fix: stale config paths broke DB writes

### Fixed
- **Stale `config.json` paths broke the service's DB writes.** When the data
  tree was migrated to a new location, the copied `config.json` still pinned
  absolute `working_dir`/`db.location`/`media`/`thumbnails` to the *old*
  path. The service opened that old (now-deleted) DB file: reads still worked
  through the open fd, but every `fsync`/journal write failed with **"attempt to
  write a readonly database"**, so selecting a cue (`SetCue`) and volume
  updates returned 500s. Fix: regenerate `config.json` for the new working dir
  rather than copying it verbatim. Deployment test now confirms writes persist
  (selection lands in the `state` table). The service holds the
  DB open as `/opt/cutepi/data/config/ctp.db` (not a deleted inode).
- **Write-path regression coverage** — added `TestSelectCuePersistsAndRenders`,
  which drives `POST /api/cue/:cuePos` and asserts the selection persists to the
  `state` table and renders the selected row. Complements the existing
  `TestVolumeEndpointPersistsClampedDBToActiveCue`.

### Verified (live, :3001)
- service `active`/`enabled`; all GET endpoints 200 (WS `101` upgrade OK); a
  real media cue add → 200 (writes mediapool + cuesheet); select-cue persist →
  `selectedCuePos` in DB; `POST /api/volume` → 200 (gsp idle without a pipeline,
  so DB writes clamp/record as coded); `systemctl restart` (equiv. of the UI
  Restart button) comes back `active`. 4 cues intact, `volume` default 0 dB.

## 2026-09-03 — systemd service + install script

### Added
- **`cutepi.service`** systemd unit — runs CuTePi as a managed service:
  `Restart=on-failure` (crash recovery), `KillSignal=SIGTERM` (graceful shutdown
  closes the DB), `StartLimit`/`TimeoutStopSec`, and a `WorkingDirectory` that
  matches the deployed `/opt/cutepi` asset tree (`templates/`, `public/` are
  read relative to cwd). Data lives under `WORKING_DIR=/opt/cutepi/data`.
- **`install.sh`** — builds the binary, installs it plus the runtime assets to
  a prefix (default `/opt/cutepi`), writes the unit, and `enable --now` +
  restarts it. Re-running after code changes rebuilds and restarts the service.
  Overridable via `CUTEPI_PREFIX`, `CUTEPI_DATA`, `CUTEPI_PORT`,
  `CUTEPI_SERVICE`.
- **Systemd-aware Restart** — `routes.restartProcess` now detects it is running
  under a unit (via `INVOCATION_ID`) and calls `systemctl restart <unit>`
  instead of the detached self re-exec, so the UI's Restart button restarts the
  managed service rather than spawning an unmanaged orphan that would hold the
  port. Unit name from `CUTEPI_SERVICE`, default `cutepi`. Covered by
  `TestSystemdUnitResolution`.

### Changed
- The live server (previously an ad-hoc `/tmp` process on :3001) is now the
  `cutepi` service at `/opt/cutepi`, with its DB/media/thumbnails migrated to
  `/opt/cutepi/data` (4 cues preserved).

## 2026-09-02 — Schema rationalization, volume round-trip coverage

### Added
- **Legacy `cuesheet` schema rationalization** — a guarded migration
  (`ctp.migrateLegacyCuesheetDefault`) now detects a `volume` column whose
  default drifted from the old linear value (`1.0`) to the current dB default
  (`0`) and rebuilds the table to the code's DDL, preserving every row and the
  `mediapool` FK. SQLite has no `ALTER ... DEFAULT`, so a table rebuild is the
  only way to re-brand the default; fresh DBs (default already `0`) are
  untouched. Unit-tested against a throwaway legacy DB
  (`TestMigrateLegacyCuesheetDefaultRebuildsStaleVolume`).
- **Volume round-trip coverage** — `TestVolumeEndpointPersistsClampedDBToActiveCue`
  drives `POST /api/volume` through the real gin engine against a real cue,
  asserting the dB value is applied and the persist clamped value (in-range,
  +12 ceiling, -60 floor) lands on the cue's DB row.

### Verified
- The vertical dB volume slider: markup (attribute order + readout) asserted
  by `TestInspectorRendersForSelectedCue`; CSS is the standard `vertical-lr` +
  `direction: rtl` min-at-bottom technique; live-drag commit flows through the
  inspector form's `hx-put` to `UpdateCue`. (A real pointer-drag still needs a
  headed browser; this sandbox is headless.)

## 2026-09-02 — dB volume slider, content-fixed cue columns, selection fix

### Changed
- **Volume is now in dB** — the `volume` column stores dB (default `0` = 0dB,
  range −60…+12) instead of linear gain. The GStreamer `volume` element gets
  `10^(dB/20)` via `gsp.dbToGain`. The Cue Inspector's volume field became a
  **vertical slider** (lower dB at the bottom) with a read-only dB readout kept
  in sync by the client. Old linear-gain rows are migrated to dB on startup.
- **Type column is icon-only** — the "Type" header label is gone; the column is
  content-fixed to the widest icon (no resize knob). Includes a screen-reader
  `sr-only` "Type" label for accessibility.
- **Cue No column sized to its header** — content-fixed to the "Cue No" title
  width; both this and the Type column are no longer user-resizable.
- **Cue row selection target fixed** — the row used the invalid Alpine-style
  `hx-target:inherited="#cuesheet"` (htmx v4 dropped it), so the swap target
  fell back to the row and the afterSwap/MutationObserver inspector refresh
  never fired. Now `hx-target="#cuesheet"`, so selecting a cue re-renders the
  sheet and the inspector follows the selection.

### Verified
- WebSockets sync (`ws.Handle` at `/api/ws`, client in `ui.js`) already handled
  selection/media sync; re-confirmed intact this pass.

## 2026-09-01 — Per-cue volume, inspector refinements

### Added
- **Per-cue master volume** — new `volume` `REAL` column on `cuesheet`
  (default 1.0 = 0dB). The active cue's gain is applied to the pipeline's
  audio `volume` element on play and driven live by `gsp.SetVolume`, which
  now returns the clamped value so callers persist it onto the cue. Edited in
  the Cue Inspector (a number input). There is **no global volume** anymore.
- **Cue Inspector colour dropdown** — the HTML5 colour picker is replaced by
  a fixed 12-named-swatch `<select>` (same palette in the row's right-click
  context menu). Empty = no accent.
- **Cue Inspector trim-out fix** — a stored `posEnd` of 0 means "no trim out"
  (play to file end), but the timeline was rendering it at 0% (hidden and
  undraggable). The timeline now treats 0 as the full duration so the Out
  marker appears and drags correctly.
- **Cue Inspector Source line** — the cue's file name is shown next to its
  editable title as `Source: <filename>`.
- **Context-menu colour & fade persistence fix** — the onchange handlers
  referenced an undefined `m` variable, so colour/fade changes never
  persisted; they now use `menuEl`.

### Changed
- **Master volume removed** — the global default volume (`config.json`
  `volume`), the Settings volume field, and the Now Playing volume slider are
  gone. Playback level is per-cue only, defaulting to 0dB.
- **Loop is inspector-only** — the cue row's loop button, the Now Playing loop
  button, and the context-menu Loop item are removed. Loop is set from the
  Cue Inspector. The orphaned `POST /api/loop` route is dropped.
- **Cue-row action buttons removed** — the row's Play/Loop/Move/Delete button
  group (and its `Actions` column, now 6 columns) is gone. Cue properties are
  changed via the right-click context menu and the Cue Inspector.

## 2026-09-01 — Per-cue model, fade & stop others, context menu, theme plugin

### Added
- **Per-cue storage columns** (`cuesheet`): `loop`, `color`, `parent`,
  `fadeOut`, `fadeAction`, `autoFollow`. All editable through the generic
  `PUT /api/cue/<pos>/edit/<col>` endpoint (colour validated as `#rrggbb`,
  `fadeAction` restricted to peers/list/all, booleans normalized).
- **Media-type icon column** on each cue row (`bi-film` / `bi-music-note-beamed`
  / `bi-image` / file fallback), driven by the cached mimetype.
- **Per-cue colour** — a left-border accent + name swatch, set inline via
  `--cue-accent` from the cue's `color` column.
- **Per-cue loop toggle** button in the cue row's action group.
- **Per-cue progress row** under the currently-playing cue: a live progress
  bar (position/duration from the active pipeline) that is click/drag
  scrubbable, seeking via `POST /api/seek`.
- **Right-click context menu** on cue rows: Play, Toggle Loop, Colour picker,
  Fade & Stop Others (scope + duration), AutoFollow toggle, Delete.
- **AutoFollow** — when a cue with its `autoFollow` flag set finishes, the
  server advances the selection to the next cue (`ctp.AutoFollowSelect`,
  driven by a new `gsp.SetCueEndHook`).
- **Fade & Stop Others Over Time** — a cue with a `fadeOut` duration fades the
  currently-playing clip to black (audio `volume -> 0` + video
  `videobalance` brightness `-> -1`) over that duration, then stops it, then
  starts the cue. `POST /api/fade` exposes the same ramp for a transport
  button. (`gsp.FadeAndStop`.)
- **Theme plugin** — paste a JSON token map (e.g. `{"--ctp-bg": "#0a0a12"}`)
  in Settings to define a Custom theme; applied via an injected `:root[data-theme=custom]`
  stylesheet, persisted in localStorage.
- **Cue Inspector play options** — the inspector now also edits Loop,
  AutoFollow, colour, and Fade & Stop Others alongside trim/hold.

### Changed
- Cuesheet renders flow through `renderCuesheet` (routes), which enriches each
  cue with playback state (`Playing`/`PlayPos`/`PlayDur`) from the active
  pipeline; Now Playing data formatting in the header was already shared.
- The video playback chain now includes a `videobalance` element (retained for
  the brightness ramp) alongside the existing `volume` element.
- Cue rows carry the context-menu and loop data attributes
  (`data-cue-loop`, `data-cue-auto-follow`, `data-cue-color`,
  `data-cue-fade-action`, `data-cue-fade-out`).

## 2026-09-01 — Transport controls, trim timeline & theme pass

### Added
- **Scrub / seek** — `POST /api/seek` backed by a new `gsp.Seek` (absolute
  position, clamped to the clip and its trim Out). The Now Playing widget
  gains a draggable scrubber that live-seeks (debounced) while the change
  poller skips re-renders during an active drag.
- **Loop** — `POST /api/loop` + `gsp.Loop`/`SetLoop`. On end-of-stream (or trim
  Out reached) a looping clip restarts from its in-point instead of stopping.
  A loop toggle button sits in the Now Playing widget. Configurable default.
- **Volume** — an actual `volume` element was added to the audio playback
  chain in `gsp.buildPipeline`, so a real gain stage now exists (FadeOut
  could eventually use it). `POST /api/volume` + `gsp.SetVolume` drive it;
  the Now Playing widget and Settings both expose a volume control.
- **Cue Inspector trim timeline** — the inspector now renders a clickable /
  draggable waveform timeline (`media.GenerateWaveform`, generated by the
  thumbnail worker as `<filename>.wave.png`). Clicking sets the trim In;
  the In/Out markers drag to set the trim window, which writes back into the
  existing trim fields before Save.
- **Cue Inspector now follows the selection** for all four cuesheet
  re-render paths (htmx swap, change poller, WebSocket sync, drag-and-drop)
  via a `MutationObserver` on `#cuesheet`, where the old `htmx:afterSwap`
  hook missed the three non-htmx paths.
- **Empty cuesheet always fills the pane** — the cuesheet table (and its
  striped filler) now renders even with zero cues, with the empty-state text
  as a guidance row in the table body.
- **New settings** under Settings: default loop-on-end and default volume.
  `config.json` gains `loop` and `volume` (volume defaults to `1.0`,
  loop to `false`).
- **Theme overhaul** — three distinct looks in `themes.css` still driven by
  `--ctp-*` tokens: Future SciFi Blue (deep navy + cyan glow + scanline-ish
  inset shadow), LCARS (black/orange, rounded pills, uppercase, thick accent
  rail), QLab (flat charcoal + orange, square corners, chunky borders).
  Shared transport control styling for the scrubber/volume sliders and trim
  timeline added so all themes stay consistent.

### Removed
- The "Drag onto the cuesheet to add as a cue" tooltip on mediapool tiles
  (redundant; the empty cuesheet now tells the user the same thing).

### Fixed
- MediaPool 3-dot menu: the whole box now sits bottom-right clear of the
  caption overlay, with the toggle icon centred and the menu right-aligned.

### Tests
- `TestSettingsLoopAndVolume`, `TestTransportControlsEndpoints`,
  `TestNowPlayingWidgetControlsMarkup`, `TestSetLoopPersists`,
  `TestSetVolumeClampsAndValidates`.

## 2026-09-01 — Restart / Shutdown controls

### Added
- New `POST /api/restart` and `POST /api/shutdown` endpoints plus **Restart**
  and **Shutdown** items in the TopBar Settings dropdown (each guarded by a
  native htmx `hx-confirm` dialog). Both respond 200 first, then act after a
  short grace so the browser sees a clean response.
- Shutdown signals the current process with SIGTERM, reusing the existing
  graceful-DB-close handler in `main.go`.
- Restart spawns a detached copy of the running binary (new session, same
  env/args/log output) marked with `CUTEPI_RESTART_WAIT`; the child waits for
  the parent to exit (freeing the port, up to 20s) before starting, avoiding
  a bind race. Deliberately self-managed: a systemd unit would use
  `systemctl restart cutepi` instead (see the `ponytail:` note in
  `routes/api.go`).
- New log codes `RTERestart` / `RTEShutdown` (`RTE-E225` / `RTE-E226`).

### Tests
- `TestRestartAndShutdownEndpoints` — swaps the process-killing hooks for
  stand-ins, asserts both endpoints return 200 and invoke their hook, and
  that the rendered topbar advertises both actions with an `hx-confirm`.

## 2026-08-31 — Operator UI pass

### Added
- Added the CuTePi 80s corporate-style logo and browser-local LCARS, QLab, and
  Future SciFi themes in Settings; Future SciFi is the default.
- Added responsive top-bar layout, mobile upload picker wiring, sticky CueSheet
  headers, Enter-to-save inline editing, striped blank CueSheet rows, and
  viewport-safe dropdowns.

### Fixed
- Cue selection changes now advance the server sync version, so arrow-key
  selection changes propagate to every connected browser.
- Media-library updates now send a separate WebSocket refresh event.
- Repeated media can be added as separate cues, and cue order/selection stays
  consistent after moves and deletes.
- Upload/YouTube partial swaps now replace `#mediapool` instead of nesting it.

## 2026-08-31 — Cue workflow and multi-client sync

### Added
- Added the collapsible Cue Inspector with persisted trim In/Out and per-cue
  hold-last-frame settings.
- Added persisted cue selection, drag-and-drop reorder, QR upload access, and
  `/api/ws` server-authoritative sync with polling fallback.
- Added focused WebSocket integration coverage.

## 2026-08-31 — HTMX v4: drop compat flags, go explicit

### Changed
- Removed the `htmx-config` compat meta (`implicitInheritance:true` +
  `noSwap:[204,304,"4xx","5xx"]`) from `templates/header.html`. htmx-4
  behaviour is now expressed explicitly per-element instead of via the flags.
- **Explicit inheritance**: the cuesheet's non-selected cue `<tr>` now uses
  `hx-target:inherited="#cuesheet"` so the dblclick-edit cells (which carry
  their own `hx-swap="outerHTML"` but no target) keep inheriting the
  `#cuesheet` target, exactly as they did under implicit inheritance.
- **noSwap for no-content/status-only actions**: explicitly marked these
  fire-and-forget elements with `hx-swap="none"` so their responses never touch
  the DOM — the media controls (play/togglePause/stop/prev/next/panic/fadeOut),
  the `#esc` stop trigger, the cue `#spaceBar` play trigger, and the Show Test
  modal's "Hide Test" button. (Mediapool Play/Load/Refresh-thumbnail, cue Play,
  and Settings Save were already `hx-swap="none"`.)
- **noSwap for 4xx/5xx**: added a `htmx:before:swap` handler in
  `public/src/ui.js` that prevents swapping HTTP error responses (status >=
  400) into the `#cuesheet`/`#mediapool` targets, preserving the old noSwap
  behaviour for partial-rendering endpoints — most importantly
  `DELETE /api/media/:filename` (409 when a file is currently playing) and the
  various error.html renders. 204/304 remain no-swap via htmx 4's default
  `noSwap:[204,304]`.
- Not annotated with `hx-swap="none"` (on purpose): the endpoints that return a
  re-rendered partial on success (cuesheet next/prev/move/delete, mediapool
  add, delete confirm, inline-edit PUT, upload, youtube) keep their real swap;
  their 4xx/5xx errors are covered by the `htmx:before:swap` handler above.

### Tests
- `routes/routes_test.go`: added `TestHtmxCompatFlagsRemoved` asserting the
  `htmx-config` meta is gone and the cue row carries the explicit
  `hx-target:inherited="#cuesheet"` annotation.

## 2026-08-31 — Error codes for logging

### Added
- New lightweight `logs` package (`logs/logs.go`) with stable, documented error
  codes in a `<SUBSYS>-E<NNN>` scheme (`GSP`, `RTE`, `NET`) plus a single
  `logs.Printf(code, format, ...)` helper that prefixes log lines with the code,
  e.g. `[GSP-E100] gsp: error pausing: <err>`.
- `logs` unit test asserting all codes are unique and well-formed.
- Full code-meaning table documented in `DESIGN.md` -> Build Decisions.

### Changed
- Replaced the bare `fmt.Println`/`log.Println` in `gsp/gsp.go`, `routes/api.go`,
  and `network.go` with coded `logs.Printf(...)` calls, preserving the original
  message text (just prefixed/normalized with a code). No runtime/control-flow
  changes. Purely informational stdout (startup network banner, mediapool
  add-cue `fmt.Printf` lines) left untouched.

## 2026-08-31 — README refresh

### Changed
- Rewrote `README.md` as a concise overview + quick start + tests + current
  Features doc. The old trailing checklist of long-term goals (several of which
  are now implemented) is replaced by a "Features" section listing what is
  actually built (media pool UI, 7-column cuesheet with inline editing and
  adjustable width, collapsible/resizable media-pool panel, change-detection
  Now Playing, settings + Show Test modals, GStreamer playback, media tooling),
  and a short "Not yet implemented" section holding the genuinely-unimplemented
  ideas so they're clearly separated from shipped work. Install/run/environment
  guidance and tests are preserved; detailed docs are pointed to via `DESIGN.md`
  / `CHANGELOG.md` / `TODO.md`.

### Docs
- `TODO.md`: marked the "README" item under Tooling / Quality as `[x]`.

## 2026-08-31 — Media pool toggle: right edge + more obvious

### Changed
- The media-pool collapse button ("open/close" toggle) now sits on the RIGHT
  edge of the panel (at the splitter/border), vertically centred, instead of
  the far-left edge. It now uses a bold double-chevron-left icon in a tight
  square that turns solid red on hover.
- The expand button (shown when the pool is collapsed) now uses a double
  chevron-right in green, matching the clear open/close affordance.
- Files: `public/css/comp/layout.css`, `templates/mediapool.html`,
  `templates/index.html`.

## 2026-08-31 — Separate polling endpoints with change detection

### Added
- New lightweight `GET /api/nowplaying/status?version=N` endpoint polled by the
  Now Playing widget instead of the old "always re-render" `GET /api/nowplaying`
  every 500ms. It returns a small JSON `{"changed": bool, "version": N}`: the
  version is a monotonic, server-side change counter in `gsp`; `changed` is
  true only when the counter differs from the version the client last saw.
  Serving the same counter to every client keeps concurrent browsers in sync
  without any per-client server state.
- `gsp` now maintains that counter, bumped on real client-visible changes: a
  pipeline swap (`Load`/`ShowTest`), teardown (`EOS` via `clearIfCurrent`,
  `Panic`), Play/Pause/TogglePause/Stop transitions that act on a live
  pipeline, and position "ticks that matter" — `CurrentPosition()` bumps only
  when the displayed clock (nearest second, matching `formatClock`) actually
  changes. So an idle or paused widget is no longer re-rendered on every poll;
  while playing it re-renders about once per second when the clock text moves.
- `mediainfo.html` no longer self-polls via `hx-trigger="every 500ms"` with a
  full `outerHTML` swap. `public/src/ui.js` polls `/api/nowplaying/status`
  every 500ms and fetches/replaces `#mediainfo` via `GET /api/nowplaying` only
  when `changed` is true.
- The index page's initial server render now passes the current
  `Filename`/`Position`/`Duration` into `mediainfo.html` (shared
  `nowplayingData()` helper in `routes/index.go`), so the widget is correct
  from the first paint instead of showing "Nothing playing" until the first
  poll.

### Tests
- `TestNowPlayingStatus` — asserts the endpoint returns 200 with the
  `changed`/`version` JSON shape and that `changed` correctly reflects the
  client's last-seen version (`?version=0` → false, `?version=1` → true on a
  fresh server).
- `TestNowPlayingHTMLRenders` — asserts `GET /api/nowplaying` still renders the
  `mediainfo.html` partial (idle "Nothing playing" state).
- `gsp/state_test.go` (`TestStateVersionBumpsOnPlaybackOperations`) — asserts
  the counter bumps on ShowTest/Play/Pause/TogglePause/Stop/Panic and stays
  stable during idle position queries. Gated on `gst-launch-1.0` being present,
  like the runtime smoke test.

## 2026-08-31 — Cuesheet header clarity (Position vs Cue Number)

### Changed
- The cuesheet "Position" and "Cue Number" columns are now visually
  distinguished so they aren't mistaken for duplicates. Both keep their
  visible labels but gained a muted sub-label (Position → "row order", Cue
  Number → "editable label") styled via a new `.cue-col-sub` rule in
  `public/css/comp/cuesheet.css`, plus a `title` tooltip on each `<th>`:
  Position = "Playback order; reindexed when cues are moved", Cue Number =
  "User-defined label (e.g. 1A, 2B, 'Opening'); unique, editable by
  double-click".
- Pure template/markup + CSS change: `data-column-resize` attributes and the
  `.col-resize-handle` spans are untouched, so column resizing still works;
  no backend/data-model changes.

### Tests
- Extended `TestCuesheetRendersColumnResizeMarkers` to assert the clarified
  Position/Cue Number headers (tooltips and sub-labels) render alongside the
  resize markers.

## 2026-08-31 — Show Test modal

### Added
- New `templates/testModal.html` — a Bootstrap modal reporting "Test is loaded"
  with a **Hide Test** button that posts to `/api/stop` and closes the modal
  (via `hx-on:htmx:after-request`), plus a delegated `[data-test-open]` handler
  that posts to `/api/test` then opens the modal.
- The TopBar Test button in `templates/mediacontrols.html` now posts to
  `/api/test` with `hx-swap="none"` and, on success, opens the test modal
  through Bootstrap JS (`bootstrap.Modal.getOrCreateInstance`). The previously
  non-functional "Tests" dropdown item is now `data-test-open`, so it triggers
  the same load-and-show flow.
- The modal is included on the index page (`templates/index.html`).

### Tests
- `TestIndexRendersTestModal` — asserts the index page renders the test modal,
  its "Hide Test" `/api/stop` button, the `data-test-open` entry, and the
  `hx-on:htmx:after-request` wiring.

## 2026-08-31 — Playback runtime verification

### Verified
- `gsp` now carries a self-contained runtime smoke test
  (`gsp/runtime_scratch_test.go`, `TestPlaybackRuntime`) that exercises the
  real GStreamer playback path: it generates a short video and an audio ogg
  via `gst-launch-1.0`, then verifies Load, position/duration tracking,
  pause/resume (`TogglePause`), Stop, and Panic.
- Confirmed end-to-end in this environment (headless): video and audio
  pipelines build and play (`filesrc -> decodebin -> auto{audio,video}sink`);
  position advanced; while paused position held static then resumed advancing;
  Stop/Panic freed the pipeline. Test skips cleanly when `gst-launch-1.0` is
  unavailable.

## 2026-08-31 — CueList: user-adjustable column widths

### Added
- Column widths in the cuesheet table are now adjustable by dragging a
  header's right edge (each `<th>` gained a `data-column-resize` key and a
  `.col-resize-handle` span). Widths are clamped (40–600px) and persist in
  `localStorage` (`cutepi.cuesheet.colWidths`), keyed by column.
- The table now uses `table-layout: fixed` + `width: 100%`, so resizing one
  column reapportions the others within the pane instead of scrolling
  horizontally; the leading spacer column is pinned narrow.
- Implementation follows the media-pool pattern in `public/src/ui.js`:
  delegated `mousedown` (survives htmx innerHTML swaps of `#cuesheet`) plus a
  MutationObserver that re-applies widths when the drag-and-drop path replaces
  `#cuesheet` outright. Restored on load and after every re-render.

### Tests
- `TestCuesheetRendersColumnResizeMarkers` — asserts each of the 7 columns
  renders its `data-column-resize` key + handle and that cue edit/action
  markup is untouched. (Resize itself is client-side, so it is covered by the
  marker render + manual verification.)

## 2026-08-31 — Per-type thumbnail placeholder SVGs

### Added
- New per-type placeholder SVGs under `public/img/` (`placeholder-video.svg`,
  `placeholder-audio.svg`, `placeholder-image.svg`, `placeholder-other.svg`):
  dark 16:9 tiles with a muted Bootstrap-Icons glyph (film, music note, image,
  file-with-play), served through the existing `/img` static mount.
- `mediapoolView` now serves an empty `.Thumbnail` while an item's thumbnail
  is pending, and `mediapool.html` falls back to the matching placeholder
  (`/img/placeholder-{type}.svg`, keyed off the existing `$type` derived from
  `.Mimetype`) instead of drawing a broken background image. Items with a real
  thumbnail URL keep using it. This covers every mediapool render path
  (`GET /`, `GET /mediapool`, and the post-upload / youtube / delete
  re-renders, all of which share `mediapoolView` + the partial).
- `.media_thumbnail` now uses `background-size: cover` / `background-position:
  center` / `background-repeat: no-repeat` so placeholders and thumbnails fill
  the 16:9 tile instead of tiling at their intrinsic size.

### Tests
- Extended `TestMediapoolRendersFilterControlsAndType`: a pending item renders
  its placeholder SVG; an item whose thumbnail is complete keeps its thumbnail
  URL and does not render a placeholder.

## 2026-08-31 — Settings modal (port + poll interval)

### Added
- New `templates/settingsModal.html` Bootstrap modal: port and poll
  interval (ms) fields with a Save button. The TopBar Settings dropdown item
  (in `mediacontrols.html`, wired via `data-settings-open`) fetches
  `GET /api/settings` on open and populates the fields; Save posts to
  `POST /api/settings` (form fields `port`/`pollInterval`) and displays the
  server's message (e.g. "Port changes require a server restart to take
  effect.") using the htmx v4 `hx-on:htmx:after-request` syntax. Errors
  (e.g. sub-10ms poll interval) surface as an inline `text-danger` note.
- Modal is wired into `templates/index.html` alongside the other modals.

### Notes
- The spec's "only the first-connected client may edit settings" has no
  mechanism anywhere in the codebase (no client/session id exists), so
  editing is wired to all clients and the limitation is documented in
  `DESIGN.md` / `TODO.md` rather than half-implemented.

### Tests
- `TestSettingsGet` (`GET /api/settings` returns 200 with the `port` and
  `pollInterval` JSON keys) and `TestSettingsPost` (form POST returns 200
  with the restart-required message) added to `routes/routes_test.go`.

## 2026-08-31 — Media pool panel: border, collapse/resize, empty state

### Added
- The media pool pane now has a clear defining right border (and a subtle
  background tint) so it is visibly separated from the cuesheet.
- Collapse/expand: the Mediapool header has a chevron button that collapses
  the panel to zero width (cuesheet fills the row); a floating expand button
  at the left edge restores it. State persists in `localStorage`.
- Drag-to-resize: a splitter handle between the two panes lets the user
  resize the media pool (clamped 160px–70% of the window); the chosen width
  persists in `localStorage`. Implemented in `public/src/ui.js` (delegated,
  so it survives htmx re-renders of `#mediapool`).
- Empty state: an empty media pool now shows a concise "No media yet" placeholder
  with an Upload button (opens the upload modal) and "or drop files here" hint,
  vertically centred in the pane, instead of a blank grid. The filter bar is only
  shown when there are items.
- Layout moved from Bootstrap `col-md-3`/`col-md-9` to an explicit flex row
  (`.app-main-row`) driven by `--mediapool-width` (default 320px).
- The media pool is now full-height of its pane: an internal flex column keeps
  the header/filter fixed while only the thumbnail grid scrolls (instead of
  scrolling the whole pane).
- The collapse button was moved out of the header to the far left edge of the
  pane, vertically centred (halfway up), doubling as the splitter handle's
  "grab".

### Tests
- `TestIndexRendersPanelChrome` (panel chrome present: collapse/expand buttons,
  resizer, panes), `TestMediapoolEmptyState` (placeholder + no filters when
  empty), `TestMediapoolEmptyStateWithMedia` (placeholder gone when populated).

## 2026-08-31 — Standalone /upload page

### Added
- Full mobile-friendly standalone upload page (`templates/upload.html`):
  header/inline topbar with a "Control centre" link back to `/`, a
  drag-and-drop zone + file picker (reusing `dropzone.js`), a file list, and
  a big Upload button. Rendered by `GET /upload/`.
- The standalone form is a plain multipart POST (no htmx), so a successful
  upload does a full-page navigation and is redirected to `/` (303) — the
  desktop modal keeps getting the `mediapool.html` partial via its `HX-Request`
  header.
- `dropzone.js` now also lists files chosen via the OS picker (previously it
  only handled drag-and-drop), which is the primary input on the standalone
  page.
- Desktop topbar gained a visible link (`/upload`) next to the Upload modal
  button, per the spec's "desktop has a link to /upload".
- `error.html` now renders the actual `error` message instead of a bare
  "ERROR".

### Tests
- `TestUploadPageRenders` (GET /upload/ renders the dropzone + control-centre
  link, no "pong" stub) and `TestUploadRejectsNoFiles` (400 for both plain and
  htmx requests). `setupTestServer` now registers the Upload routes.

## 2026-08-31 — MediaPool: delete-confirm, filtering, fixed 2-col grid

### Added
- **Delete confirmation modal** — the MediaPool 3-dot Delete action now opens
  an "Are you sure you want to delete [filename]?" Bootstrap modal with
  **DELETE / KEEP**. Confirmed deletion re-renders the `#mediapool` and closes
  the modal via an `hx-on:htmx:after-request` handler. New partial
  `templates/deletemodal.html`, wired into `index.html`.
- **Filtering** — a filename text filter plus a type dropdown
  (All/Video/Image/Audio), applied client-side over `data-media-type` and
  `data-media-name` attributes on each tile via a new `filterMedia()` in
  `public/src/ui.js`, re-applied on `mediapool-updated`.
- **Refresh thumbnail entry** — added to the MediaPool dropdown, posting to
  the existing `POST /api/media/:filename/refreshThumbnail` route.
- **Fixed 2-column gallery** — replaced the responsive Bootstrap `row`/col
  classes with a dedicated `.media-grid` (always exactly two columns) and
  made the pool pane keep its scrollbar visible (`scrollbar-gutter: stable`).
- **Date added** — each media tile now shows its `date_added` (formatted
  `YYYY-MM-DD HH:MM`); the pool already sorts newest-first.

### Fixed
- htmx v4 `hx-on` event syntax: the delete modal used `hx-on::after-request`,
  which under htmx v4 resolves to a nonexistent `after-request` event. Correct
  syntax is `hx-on:htmx:after-request` (documented by reading the vendored
  source's meta-character parser).
- The delete-confirm button no longer relies on `data-bs-dismiss="modal"`
  racing the DELETE request; it hides the modal on `htmx:after-request`.

## 2026-08-31 — Cuesheet timing, reordering, and MediaPool render fixes

### Added
- `preWait`, `cueDuration`, `postWait` columns on the `cuesheet` table with
  automatic migration for existing DBs (named `cueDuration` to avoid a
  `SELECT *` collision with `mediapool.duration`, a float in seconds).
- `ctp.ParseTime` / `ctp.FormatTime` (exported via `formatTime` template
  func): accept `hh:mm:ss.ms`, `mm:ss.ms`, `ss.ms`, or a bare number
  (seconds).
- CueList now uses the 6 spec columns (Cue Number, Cue Name, Cue Pre-Wait,
  Cue Duration, Cue Post-Wait, Actions) ahead of a leading Position column.
  Actions gains play, move up, move down, delete.
- `MoveCueUp` / `MoveCueDown` (transactional position swaps via a temporary
  negative position to avoid UNIQUE-constraint collisions) and routes
  `POST /api/cue/:cuePos/move/up|down`.
- `POST /api/cue/:cuePos/play`, and a `POST /api/load/:filename` route to
  back the MediaPool dropdown's "Load" action (previously a 404).
- `GET /mediapool` standalone partial endpoint backing the mediapool's
  auto-refresh `hx-get` and re-rendering the panel after deletion.

### Fixed
- `DELETE /api/cue/:cuePos` returned an empty 200 while the delete button
  targets `#cuesheet` with `hx-swap="innerHTML"`, which would wipe the whole
  cue table. It now returns the re-rendered cuesheet.
- `DELETE /api/media/:filename` returned an empty 200 while the delete
  action targeted `closest li`, which removed the wrong element (the dropdown
  item) and left a stale tile. It now re-renders the mediapool, and the
  delete button targets `#mediapool`.
- `MediaPool` "Load" dropdown item hit a nonexistent `/api/load/:filename`
  route (404). Added the route.

## 2026-08-28 — Full overhaul pass

### Fixed
- GStreamer pipeline manager (`gsp/gsp.go`) rewritten around a single
  mutex-guarded handle so concurrent Play/Load/ShowTest/Stop/Panic calls
  can no longer race, orphan a pipeline, or lose the reference needed to
  stop playback.
- `ctp.NextCue` could never advance (compared against a `CuesheetLength`
  field that was never assigned); `PrevCue` had an off-by-one that blocked
  navigating to cue 1. Both now derive cue count from the DB and correct
  the boundary check.
- SQL injection in `ctp.UpdateCue`: the edited column name is now checked
  against an allow-list instead of being concatenated unchecked into the
  `UPDATE` statement.
- `DELETE /api/cue/:cuePos` deleted nothing (it passed a cue position to a
  function expecting a filename); `RemoveCue` now deletes by cue position.
- The sqlite DB was opened at package-init time using only env-var
  defaults, before `main()` read `config.json` — a configured DB path was
  silently ignored. DB init is now explicit and runs after config load.
- First-run installs on a clean system would panic or fail silently
  because config/db/media/thumbnail directories were never created; they
  are now created up front.
- `posStart`/`posEnd` were nullable with no default, so scanning a
  freshly-added cue crashed with a NULL-to-int conversion error; they now
  default to 0.
- `/api/clear`'s HTMX response used the wrong template-data key
  (`cuesheet` vs `Cuesheet`), silently rendering an empty cue list;
  `index.html` didn't pass template context into the cuesheet partial at
  all, so the cue list was always empty on first page load.
- `routes/index.go` rendered hardcoded fake mediapool data instead of the
  real database contents.
- `gsp.CurrentPlaying()` was a stub always returning `""`, so the
  "can't delete the currently playing file" guard never actually fired.

### Added
- Real `POST /upload` and `POST /youtube` handlers (previously only stub
  `GET /` "pong" routes existed, despite the UI already posting to them).
  Uploads are validated by extension, probed with ffprobe, and rejected
  if duration/resolution/codec can't be determined; `/youtube` shells out
  to yt-dlp.
- `media` package wrapping ffmpeg/ffprobe for metadata extraction and
  16:9 thumbnail/waveform generation (video frame at 5s, image resize,
  audio `showwavespic` waveform).
- `worker` package: a background thumbnail worker that polls a
  `thumbnail_pending` DB column (not an in-memory queue), so pending
  thumbnail work survives a restart.
- `GET/POST /api/settings` for reading/updating the poll interval (min
  10ms, default 100ms) and port (takes effect after restart).
- `POST /api/media/:filename/refreshThumbnail` for the per-item thumbnail
  refresh button.
- Graceful shutdown on SIGINT/SIGTERM that closes the DB connection.
- Go unit tests for `config` (home-dir expansion, port/poll-interval
  validation) and `ctp` (mediapool registration, cue navigation
  boundaries, the column allow-list, cue removal).
- `pre.sh` now runs `go mod tidy`, `go build`, `go vet`, and `go test`,
  and also installs ffmpeg/yt-dlp alongside the existing GStreamer
  dependencies.

### Known follow-ups
See `DESIGN.md`'s "Overhaul Plan" section for remaining checklist items
(dead-code cleanup, drag-and-drop wiring, mobile `/upload` page, keyboard
column-width persistence, etc.).

## 2026-08-28 — Live functional test pass

Ran the built server end-to-end against real ffmpeg/ffprobe/yt-dlp/
GStreamer (not just `go test`) and found/fixed several bugs that only
surface with a live template render or a live ffmpeg invocation:

### Fixed
- `cuesheet.html` ranged over the wrong value (`.Cuesheet` instead of
  `.Cuesheet.Cues`), so the cue list rendered empty on every request,
  including the very first page load.
- The inline cell-edit form pre-filled with the entire stringified `Cue`
  struct instead of the specific column's value; added
  `ctp.CueColumnValue()` to fix it.
- `config.ConfigFilePath`'s default was computed independently of
  `WorkingDir` rather than derived from it, so setting `WORKING_DIR` alone
  left `config.json` writes going to the real home directory instead of
  the configured one.
- Video thumbnail generation hardcoded a 5-second seek point, so any clip
  shorter than 5s failed forever (retried every 2s indefinitely); now
  clamped to the clip's own duration, with a 60s per-item retry backoff in
  the worker as a general safety net.
- Hardened the thumbnail ffmpeg command against unusual source pixel
  formats (`format=yuvj420p`).

### Verified working
First-run directory/DB/config auto-creation; upload → metadata extraction
→ registration → background thumbnail generation pipeline; upload
rejection of non-media files; cue add/select/edit/delete; the
currently-playing-file delete guard; settings validation and persistence;
GStreamer test-pattern/playback control endpoints under a headless (no
video sink) test environment.

### Identified but not fixed (need a design decision, tracked in DESIGN.md)
No backend support for cue reordering; no schema field for a per-cue
"duration" despite the UI wiring an edit control for it; Pre-Wait/Duration/
Post-Wait are displayed as hardcoded placeholder text rather than formatted
from real values.

## 2026-08-28 — Extended unit test coverage

Added tests for `config`, `ctp`, `media`, `worker`, and `routes` (HTTP-level
template rendering tests), refactoring a few functions (`config`'s path
defaults, `media`'s thumbnail seek-time calculation, `worker`'s retry
backoff) into pure, dependency-injectable functions specifically so they
could be unit tested without a live server, real ffmpeg, or real time
passing. Writing this coverage surfaced three more real bugs, all fixed:

### Fixed
- SQLite foreign keys (and therefore `ON DELETE CASCADE`) were never
  enabled on the DB connection, so deleting a media item referenced by a
  cue left an orphaned cuesheet row that crashed the entire cuesheet view
  on the next read. Fixed by opening the DB with `_foreign_keys=on`.
- `AddCue`'s insert-at-position logic used `cuePos > ?` instead of `>=`,
  so it never displaced the cue already at the target position, causing a
  UNIQUE constraint failure. Fixing the comparison then exposed a second,
  deeper issue: SQLite enforces UNIQUE constraints per-row during a
  set-based `UPDATE`, so bumping several cue positions in one statement
  could itself transiently collide. Fixed by bumping affected rows
  individually, highest position first, inside a transaction.
- The mediapool's "newest first" sort relied on `date_added`, which only
  has 1-second resolution, so multi-file uploads landing in the same
  second had an effectively random relative order. Added `media_id DESC`
  as a tiebreaker.

### Test coverage added
`config`: path-resolution defaults (including the WORKING_DIR-derivation
regression), poll interval/port validation, first-run config file
creation, poll interval clamping on load. `ctp`: mediapool registration,
duplicate-filename rejection, sort order, cue navigation boundaries, the
cue-edit column allow-list and value lookup, cue insertion at a specific
position, media deletion and its cascade to cues, thumbnail-pending
workflow. `media`: file-extension-to-kind classification, thumbnail
seek-time clamping, image-format detection. `worker`: failure-backoff
tracking. `routes`: full HTTP-level rendering of the index page and
cuesheet partials against real templates, locking in the two template bugs
found during live testing so they can't silently regress.

## 2026-08-28 — UI pass: layout, drag-and-drop, icons, Now Playing

### Added
- Main page now fills exactly 100vh with only the mediapool/cuesheet panes
  scrolling internally, replacing the previous fixed-`90svh` layout.
- Drag-and-drop: OS files dropped onto the media pool upload directly;
  media pool items can be dragged onto the cuesheet to add a cue at the
  hovered position, with a visual drop-line indicator.
- Toolbar buttons (topbar and playback controls) now use icons instead of
  text labels, with `title`/`aria-label` for accessibility.
- The "Now Playing" widget is now live: it polls the server every 500ms
  and shows the actual current filename and elapsed/total time instead of
  static placeholder text.
