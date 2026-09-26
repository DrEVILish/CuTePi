# CuTePi — Design Guide

Living design/spec + architecture record. Updated whenever scope, architecture,
or requirements change; do not let it drift out of sync with the code. This is
a guide, not a changelog.

## 1. Overview

CuTePi is a media cue-playback controller intended to run on a Raspberry Pi
4/5, driving video/audio out of the HDMI port via GStreamer. A Go backend
serves an htmx-driven "control centre" web UI (dark, operator-facing);
SQLite3 is the single source of truth for state (media library, cue list,
selection, settings).

- **Server**: Go + gin, htmx v4 (htmax.js) for interactivity, no client-side framework.
- **Client**: HTML5/CSS3/vanilla JS, deps vendored under `public/src/` (no CDN).
- **Media tooling**: ffmpeg/ffprobe (metadata, thumbnails, waveforms), yt-dlp
  (URL downloads).
- **Playback**: GStreamer, including test-pattern playback.
- **DB**: SQLite3 via `sqlx`
- **Config/defaults**: `~/CTP/config/config.json` (env-overridable), port
  3001 default, media in `~/CTP/media/`, thumbnails in `~/CTP/thumbnails/`.
- **Trust model**: trusted LAN appliance; optional operator password (see §7).
- **Multi-client sync**: WebSocket hub with server-authoritative state; change-detection polling only to be used as the reconnect/fallback path.

## 2. Product decisions

- **Platform**: Raspberry Pi 4/5, headless, video+audio out of HDMI, pure
  server Debian Trixie (raspbian) — no X11/Wayland; gstreamer playback drives the HDMI connector directly, not a windowed sink.
- **Audio**: HDMI embedded ALSA, **exclusive to CuTePi** (only audio producer).
- **Codecs**: any file; decode **hardware-first (v4l2 h264/hevc) with software fallback** via GStreamer autoplugging.
- **Undecodable sources**: fail **immediately with an error surfaced to the user** at cue time; an **import-time probe** (`ffmpeg -v error -t 1`) rejects bad files early.
- **Cue trigger**: **Space** (control UI focused, not in an editable field) plays the selected cue; media-pool tiles are never selectable — the sheet always has a selected cue.
- **Playback end**: a cue **stops** after playback — cues are a list, not a playlist; there is no implicit advance. Auto-continue is explicit and per-cue.
- **Waits**: `preWait`/`postWait` apply **only to auto-continuing cues** (before start / after end). Auto-continue advances down the sheet; a stop on a group row triggers the group's action and continues.
- **Loop / Hold defaults**: `loop=off`, `hold=off` for new cues. `loop` always wins over auto-continue; `loop_count` (0 = infinite, N = play N times) is exhausted before an auto-continuing cue advances.
- **Interruption**: behaviour when a new trigger interrupts a running playlist/slideshow is per-cue/per-group, not global (identity guards, §6.8).
- **Trim**: `posStart`/`posEnd` are timecodes *into the source media*; 0 =  untrimmed at that end; values stored as given (no normalization).
- **Cue groups**: visual folders, nestable, collapseable, hold cues, a group can act as a playlist.
- **Slideshow**: a group of images can run slideshow mode from the cue group inspector — shuffle / loop / fade / duration-per-image on `cue_group`;
  images are visible cue rows inside the group with a "now showing" indicator.
- **Settings editing**: all operators may edit
- **Output level**: system output always 100%; volume is a **per-cue** control only (no global/master).
- **Missing sources**: startup scan flags missing files; pool tile shows a  warning triangle; the cue shows a warning offering **delete the cue** or **re-link to another media file**. No periodic scan.
- **Show export/import**: `.CTP` = ZIP of a JSON manifest (all cue info incl. the **audit trail**) + referenced media; import restores with a modal choice: **append to end or overwrite**.
- **Logs**: viewable in the Web UI with level filter + clear; changing the level changes what is **recorded** (runtime switch). Structured playback events form an **audit trail** shipped in `.CTP` exports.
- **yt-dlp**: URL import is a core, imperative feature (not a convenience).

## 3. Architecture & repo layout

```
config/config.go       config load/save, path expansion, defaults, auth password
ctp/ctp.go, db.go      domain types + SQLite schema/access/migrations
gsp/gsp.go             GStreamer pipeline manager (single mutex-guarded pipeline, atomic swap), state version counter
media/                 ffprobe metadata (Probe), thumbnails, waveform peaks
worker/                DB-backed thumbnail/pending queue (survives restart)
routes/*.go            HTTP handlers: index, api, groups, show (export/import), upload, youtube, auth, install, public
logs/logs.go           stable [SUBSYS-E###] codes + ring; audit trail
ws/                    WebSocket hub (server-authoritative sync)
templates/*.html       server-rendered htmx fragments + pages
public/                static assets: css/, src/ (js), icons/, img/
main.go network.go     entrypoint / server wiring / restart
```

**Split of responsibilities**: playback/cues/selection/media-library state are server-authoritative; the browser is a mirror (WebSocket + poll fallback). Purely cosmetic UI state — column widths, panel sizes, theme choice — stays in `localStorage`.

**htmx conventions**: htmx v4 vendored, explicit attribute inheritance. Successful state mutations return the complete target partial with `outerHTML`;
status-only actions use `hx-swap="none"`;
4xx/5xx responses never replace application panels (suppressed in `ui.js`).

## 4. Data model (SQLite)

Tables come from the schema in `ctp/db.go` (source of truth). Conceptual
`media` → **`mediapool`**; `cues` → **`cuesheet`**.

- `mediapool`: `media_id`, `filename` (unique), `mimetype`, `size`, `duration`
  (float sec), `resolution`, `codec`, `media_title`, `thumbnail_pending`,
  `waveform` (JSON peaks), `waveform_pending`, `media_meta` (ffprobe JSON for
  the Media tab), `date_added`. Sort newest-first
  (`date_added DESC, media_id DESC`).
- `cuesheet`: order `cuePos` (unique, reindexed 1..N on reorder); `cueNum`
  (unique label); `media_id` FK → mediapool (ON DELETE CASCADE); `title`;
  `preWait`, `cueDuration`, `postWait`, trim `posStart`/`posEnd` (ms, 0 =
  untrimmed); `hold`, `loop` (both default 0), `loop_count` (0 = infinite),
  `autoContinue`, `color`, `fadeOut`, `fadeAction`, `volume` (dB, default 0 =
  0dB);   `parent` (`cue_group.group_id`, 0 = top level). `parent` is the single
  source of membership truth: it is written explicitly by the op that moves
  or creates the cue, and never re-derived. `sheet_index` is visual order
  only. The renderer reads both literally and never writes (see §6.4).
- `cue_group`: `group_id`, `name`, `parent_group_id` (nesting; validated
  acyclic, depth ≤ 8), `collapse`, `slideshow`, `shuffle`, `loop`, `fade_ms`,
  `duration_ms`, `cue_num`, `color`.
- `state`: small server-authoritative key/value store — persisted selection
  (cue `cuePos`, or group id negated), sync version counter.
- `config.json`: port, `poll_interval_ms`, `loop` (direct-load default),
  `auth_password`, working-dir paths.

## 5. UX / UI design (per panel)

### 5.1 Overall shell and themes

Exactly 100vh: topbar (natural height) + content row (media pool | cuesheet panes) with no page-level scroll; only the panes scroll internally (`scrollbar-gutter: stable`).
Themes are browser-local presentation state via CSS variables (`--ctp-*`); **LCARS**, **QLab**, **Future SciFi** (default),
and **Custom** (paste a JSON token map such as `{"--ctp-bg": "#0a0a12", "--ctp-accent": "#00ff88"}`).
A theme change restyles the shared pane chrome (pool, inspector) together.

### 5.2 Top bar (merged command header)

One row, left to right:

- WebSocket status dot (hover: a styled card with connection facts — live/offline, connected client count, server uptime, poll fallback — from `GET /api/serverinfo`),
- Wall clock (locale `HH:MM:SS`, tabular numerals).
- **GO cluster** (in the version-guarded status partial):
- The GO button (bordered, glowing — the loudest control on screen)
- *selected* cue it fires
- **NOW cluster** (same partial):
- playing cue number - cue name, progress indicator, time, remaining time.
- menu dropdown, Tests / Settings / Export / Import / Logs / Restart / Shutdown.
- The cuesheet has no GO bar of its own — the header is the single command strip.

**Firing visuals on the sheet**: the playing row has an indicator on the left of the row, and a cue in a chain wait shows a live seconds pill.

### 5.3 Media Pool (left pane)

- multi-column, 16:9 thumbnails (video = frame at `duration/2`; image = copy; audio = `showwavespic`); per-type placeholder SVGs while pending;
  hover reveals details (name, mime, size, date added); column count tunable via a header stepper, persisted.
- **Hover details**: the info overlay is clipped to the tile; when the tile is too small for it (rows truncated), the details pop out in a styled
  floating card near the tile instead of spilling.
- Filters: filename text + type dropdown (video/image/audio/all). Sort: newest first (`date_added DESC, media_id DESC`).
- 3-dot menu per tile (bottom-right, opens downward next to the button):
  **Add** (as cue), **Delete** (confirm modal), **Refresh thumbnail**, **Analyse** (waveform rebuild).
- Drag-and-drop upload onto the pool (multi-file); items are draggable into the cuesheet (drop on a group header assigns membership).
- Panel collapsible to a sliver / drag-resized; width + collapsed state persisted. Scrollbars always visible. Empty pool shows info drag to upload placeholder.
- Missing source: warning-triangle icon on tiles whose file is absent from disk (startup scan only).

### 5.4 Cuesheet (right pane)

- Sticky header, scrollable body. Columns: **icon** (media type / missing warning), **Number**, **Name**, **PreWait**, **Duration**, **PostWait**.
  per-cue actions live in the Inspector and the row context menu. Column widths for drag-adjustable (width only, **Icon** and **Number** are fixed width), persisted.
- All cells editable by double-click; save on blur/enter. Time parser: `hh:mm:ss.ms` or a bare number = seconds.
- **Groups** are first-class rows: numbered (editable `cue_num`, maxlength 24), coloured, selectable and nestable — subgroup headers render indented inside their parent's
  block** (each level indents 1.1rem; the row carries `--depth`).
  **An open group's block is enclosed by a fine hairline border** — the header draws the top edge, the last member the bottom, every row the sides 
  so membership reads at a glance; collapsed groups draw only their header. Members' names indent same as subgroup headers; a collapsed group skips its whole subtree
  in keyboard nav. Right-click group rows: Play, Inspector, Collapse/Expand, New subgroup, Delete group; group headers are draggable to move the whole subtree block.
- **One drop model** (single cue, multi-selection block, or group block) — the hovered row band computes exactly one intent and draws exactly one indicator for it; the **indicator's indent shows where the drop lands**: indented to the member name (in-group, `cue-drop-in`) or plain (top-level, `cue-drop-top`):
  - pointer on a group header's **body** (below its near-first ~30% strip) → the drop **JOINS** the group this way: collapsed folder = highlight (lands **last**); expanded header = line right below the header (lands **first**); a dragged group nests as a subgroup;
  - pointer on the header's **top strip** → the plain top-level line above the header (between blocks — this is the transition zone around a header);
  - member cue bands → an **indented** line before (upper half) or after (lower half) that member; membership = the member's group. Dropping below an expanded group's last member therefore lands INSIDE (member slot) while hovering the following top-level row keeps top level — the line's indent flips at the row boundary, making the membership jump unmistakable;
  - top-level cue bands → plain line before/after, stays top level; empty space below the sheet is always top level;
  - dragging a cue inside the multi-selection moves the whole block, relative order preserved; dragging an unselected cue moves only it (QLab/Finder);
  - any join into a collapsed group opens the group, so the dropped cue lands visibly;
  Every drop sends one payload (`POST /api/sheet/drop`: what was dragged + the hovered band's `parent` (0 = top level) + the slot anchor `after`, else the gap mode) and the server executes it literally — position and membership are stored together, in one transaction. No membership is inferred server-side for client drops; the legacy derived path (`join/forceTop`) remains only for internal callers.
  - `Cmd/Ctrl+Backspace` deletes the selection; a selected group block removes the folder, subgroups and member cues together (`DeleteGroupWithCues`).
  - Positioned cue inserts (`POST /api/cue/add/:file/:cuePos`, i.e. media drops) resolve the cuePos to a visual gap *before* renumbering, then take that gap's owner. Naming a cue that opens its group's run lands above the header (top-level), not inside it; appends always land top-level.
  - Move buttons swap two cues and give each the owner of its new slot, so crossing a header changes membership. Full-order replaces keep stored parents and then release only cues left with no adjacent belonging row (a lone member never leaves). The renderer treats a cue parked directly above its own header as outside the folder.
- **Groups are draggable**: their position is **stored** (`cue_group.sheet_index`) like cue position, set by the drag and drop; a group block (header + whole span, nested included) moves as one.
- Row-state visuals (QLab style): colour full-row tint per cue, selected-row highlight.
- Selection: single-select, arrow-key navigable (Up/Down walk cues + group  headers; Right/Left open/close a selected group). Persisted in the DB; `POST /api/cue/:pos` selects. Space (or transport Play) acts on it.
- **Context menu** (cue rows): colour, delete
- **Missing source** cues: warning badge + the Inspector shows a Re-link / Delete cue banner.
- **Scheduled** cues (schedule enabled, any time including midnight) show a clock icon + trigger time after the title.

### 5.5 Cue Inspector (bottom-docked panel)

- Built and behaved like the Media Pool pane: shared resizer/collapse chrome
  (collapse = fully hidden, one form spanning all tabs so any change saves instantly (htmx `change delay:200ms`, **silent flash** on commit — no dialogs / "Saved" text).
- Tabs (static strip in `index.html`; audio/video panes are omitted for image cues:
  - **Time** — waveform trim timeline (canvas of JSON peaks, draggable In/Out markers; **only dragging a handle changes trim**; clicks elsewhere are inert),
    Trim In/Out fields, Pre-Wait, Post-Wait, Loop + loop-count, Hold-last-frame, Auto-continue, fade-stop scope/time, playback-rate slider with 1× reset.
    Renders even where duration is unknown (timeline duration-gated). The timeline shades the shared audio+video
    fade-in/out envelope over the trim window (same curve the engine ramps).
  - **Video** — video Fade In / Fade Out (times).
  - **Audio** — volume (dB slider −60..+12, double-click resets to 0 dB), Fade In / Fade Out, Balance/Pan
    (double-click centres), Mute toggle button (danger-red while muted), EBU R128 loudness-gain readout.
  - **Media** — detailed codec info (VLC label/value style): container, overall bitrate, video codec/profile/resolution/fps/pixel format/colour
    space, audio codec/channels/sample rate/bitrate. From `media_meta`, refreshed by the thumbnail worker; missing fields just omit rows.
  - **Colour** — 12-swatch named palette as a radio chip grid + "None"; instant-save on change).
- **Inspector edits apply to the NEXT firing, not the running instance.** Trim, fades, waits, schedule,
  colour and structural settings are read when a cue fires — editing a playing cue never disturbs it.
  Exception: volume, mute, balance and rate are pushed live to the running pipeline when you edit the
  cue that is currently playing (and persisted for next time).
- **Trim timeline extras**: Zoom mode (arm, then drag a box over the waveform), +/- zoom steps, Zoom reset; mouse-wheel pans a zoomed window left/right.
  Deep zoom fetches a pixel-matched envelope (`/api/media/:name/wave`) so bars stay ~1 per CSS pixel at every depth.
  The zoom window survives inspector re-renders (saves don't reset view).
- Top of the panel: cue badge, title, `Source: <file>`. When the source is missing, a warning banner replaces the timeline area with a **Re-link** dropdown (media pool) and **Delete cue**.
- Group selection shows the **Group Inspector**: name, `cue_num`, colour, slideshow on/off, shuffle/loop (enabled when slideshow on), **duration and fade in seconds** (persisted as ms).

### 5.7 Upload

- Desktop: modal from the topbar (or Drag and drop onto the pool). Mobile: standalone `/upload`. Both: drag-and-drop + file picker, multi-file, no size limit,
  reject non-media types (422). Metadata extracted synchronously but without blocking the HTTP response; duration/resolution/codec failures reject the import.
  YouTube/URL via yt-dlp with stage logging.
  Uploads, deletions, and thumbnail changes broadcast a targeted WebSocket refresh.

### 5.8 Show export / import (`.CTP`)

- **Export** (`GET /api/show/export`): ZIP of `cutepi.json` (cuesheet +
  selection, per-cue settings, **groups incl. nesting + slideshow settings —
  manifest v2**, audit trail) + referenced media files (`media/<filename>`).
- **Import** (Show modal): restore with **append to end** or **overwrite**;
  all referenced media validated as available (in the `.CTP` or local pool)

### 5.9 Log viewer & audit trail

- Web-UI modal (`GET /api/logs`): filter/record by level (debug/info/warn),
  clear action. Runtime level switch affects what is **recorded**.
- Audit trail (`cue_start`/`cue_end` events with pos/title/wall-clock) is an
  append-only ring; clearing the log never clears the audit.

## 6. Playback & features

### 6.1 Pipeline manager (`gsp`)

Single mutex-guarded pipeline handle in a `manager` struct; atomic
stop-then-replace via `swap()`; EOS/error clear the current reference
(`clearIfCurrent`); one `buildPipeline()` for files and test patterns. A cue
position (`CurrentCuePos`) and playing-file (`CurrentPlaying`) are tracked for
guards. GStreamer runtime is smoke-tested against real `gst-launch-1.0`
assets (`gsp` test; skips when absent); the audio chain is
`queue → audioconvert → audioresample → volume → autoaudiosink`.

### 6.2 Trim, Hold, Loop, Volume, Seek

- **Trim**: on load, seek to `posStart`; reaching `posEnd` = EOS. No trim if
  both 0.
- **Hold**: on EOS of a held video cue, seek to final frame and pause (frame
  stays until Stop/Panic); image cues display as-is.
- **Loop**: on EOS (natural or trim-Out), seek back to the in-point and
  continue. `loop_count` 0 = infinite, N = N plays; loop wins over
  auto-continue.
- **Volume**: per-cue dB (−60..+12, default 0 = 0dB), converted
  `10^(dB/20)` for the GStreamer volume element; live-set via `SetVolume`.
- **Seek**: absolute, clamped to clip duration and trim Out.

### 6.3 Auto-continue & waits

Per-cue `autoContinue`; `preWait` pause before start and `postWait` after end apply only to it; advances down the sheet. A generation counter guards the
chain — any operator playback during the wait disarms it (even replaying the *same* cue; the old cuePos-equality guard could not detect that).

### 6.4 Groups & slideshow

- Groups trigger as playlists (`playFirstGroupMember` or `slideshowRunner`).
- **Nestable groups**: a group may live inside another.
  Rendering, keyboard selection and collapse all walk the tree via `ctp.FlattenSheet` (the single source both the cuesheet renderer and the
  arrow-key walk share, so they can never disagree): a group's block contains its subgroups' blocks, collapsing hides the subtree's member cues only —
  unrelated rows parked inside the span (top-level gap rows, other groups' members) keep rendering. Subgroup creation: Drag a group into another group.
- **Membership is stored, never derived** (rebuilt 2026-09-17 after the old
  derive-on-every-op model kept DB and UI out of tune): `cuesheet.parent` is
  written explicitly by the op that places the cue (drop, insert, move,
  bulk-assign) and read literally by `FlattenSheet` — a cue renders inside a
  group's span only when its stored parent matches that group, otherwise as a
  top-level gap row at its position. There is no re-derivation pass and no
  exception list; the render path performs zero writes. A startup heal
  (`ctp.HealSheet`, alongside the missing-file scan) repairs only dangling
  state from older builds — parents pointing at deleted groups go to 0 and
  missing indices are backfilled — never membership judgement calls.
- Slideshow: a per-group goroutine cycles member images with shuffle/loop, `duration_ms` hold (default fallback if unset), and `fade_ms` fade;
  `POST /api/group/:id/play`. A **cuePos identity guard** aborts the run when the operator plays something else, and after the fade completes, so a stale
  loop can't clobber a newer choice.
- **Slideshow soundtrack**: audio cues inside a slideshow group do not slide — they become the background-music playlist (`gsp.BackgroundPlaylist`, its
  own audio pipeline, shuffled when the group shuffles) played underneath the slides for the whole run. Any main-pipeline decision — Stop, Panic, a new
  load — kills the soundtrack with it.

### 6.5 Queued fade-then-play

`POST /api/play`/cue-play with a running pipeline and a `fadeOut > 0` runs `FadeAndStop(fadeOut)` in a background goroutine, then loads the target cue —
guarded by the same identity check: if `CurrentCuePos()` moved away from the target during the fade, the queued load bails (an operator's newer cue/stop
wins). Errors are logged, not swallowed.

### 6.6 Cue trigger

Space (not in an editable field) → plays the selected cue; on a group selection this triggers the group action.

### 6.7 Synchronization & inspector auto-follow

WebSocket hub (`/api/ws`) is the **primary** channel: while playing, the pipeline ticker pushes one sync per displayed second, waking the
version-guarded pollers immediately; change-detection polling (`/api/nowplaying/status`, interval from `config.json`) runs only as the
fallback while the socket is disconnected. The topbar shows the socket state as a status dot.

### 6.8b Scheduled fire (wall-clock)

Per-cue recurring trigger: `schedule_enabled` + `schedule_days` bitmask (bit0=Mon) + `schedule_time_ms` (ms since midnight, set to whole seconds).
Second precision end to end (inspector `step=1` time input, `HH:MM[:SS]` API): minute-rounded times can never hit an exact-second sync-fire.
The scheduler ticks every 200ms and fires cues due within the last 1s (one missed tick + jitter); anything older is stale and never fires, so
enabling Show mode late never replays the day's past cues. Each cue fires once per day; schedules arm only in Show mode.
Multi-node sync-fire (several Pis firing the same second) assumes NTP-synced clocks and identical shows — each node fires on its own clock
crossing. Decision-accurate, not output-accurate: pipeline build takes ~100s of ms, so frame-exact joint output needs timed pre-roll (v2).

### 6.8 Identity guards (three places, one invariant)

The invariant *"start only if the operator hasn't moved on"* is implemented in `slideshowRunner`, the auto-continue chain, and the queued fade-then-play
goroutine — all comparing `gsp.CurrentCuePos()`. Keep them consistent.

## 7. Error handling & logging

- Distinct, loggable codes `[SUBSYS-E###]` (`GSP`, `RTE`, `NET`, `YDL`) via `logs.Printf`; greppable, never reused for a different meaning.
- Failures at trust boundaries reject cleanly (undecodable imports 4xx at import rather than cue time; missing media 409; settings validation).
- Optional auth: `AuthMiddleware` (config `auth_password`, editable in Settings) applies to all routes including static assets; browser basic-auth
  prompt; 401 wrong password; 200 once accepted. `GET /api/settings` reports `authEnabled` but never the password.

## 8. Testing & verification

- `go test ./...` — `config`, `ctp`, `media`, `worker`, `routes` (HTTP-level template renders, auth, groups, slideshow), `gsp` (real GStreamer smoke),
  migration and export/import round-trip tests. The suite speaks HTTP only — it never executes the browser JS (`ui.js`/`dnd.js` hover, intent and
  modifier-click logic), so sheet-interaction behaviour is verified manually; no browser harness exists (deliberate: the interaction bugs to date all
  lived in the server model, which the suite does cover).
- `./smoke-test.sh` — end-to-end over HTTP with generated media: upload → pool → cue → play → trim → stop → delete; converges on re-run.
- `pre.sh` bootstraps: `go mod tidy`, build, vet, test.
- Check before closing a session: `go build ./... && go vet ./... && go test ./...`.

## 11. Change history

History lives in git (`git log --oneline`; docs were consolidated 2026-09-17,
dropping the separate CHANGELOG.md/README.md). DESIGN-level deltas that alter
the guide above are folded in as they land — mark completed work, don't
delete it.

## 12. Planned features (spec'd, awaiting build)

This section specs the 2026-09-14 feature batch. **Status as of 2026-09-17:
§§12.1–12.7, 12.9–12.10 are built** (GO bar §5.2, wait pills §5.4, health/F8
§5.4, multi-select + bulk §5.4, auto-number/renumber §5.4, topbar clock §5.2,
fade curves §5.5, panic hold §6.5/Settings, test patterns §5.2) — only
**§12.8 (remote protocols), §12.11 (under evaluation) and §12.12 (v2)** remain
unbuilt.

### 12.1 GO bar with next-cue preview (QLab-style)

- A persistent strip at the top of the cuesheet pane: **GO → <num> <title>**
  for the *selected* unit (Space/GO plays the selection), plus a dimmed
  "next" line showing the unit that follows it in `SelectUnits` order.
- **GO** (button + Space/Enter) triggers `POST /api/cue/selected/play`. With
  "GO advances selection" on (default), the server re-selects the next unit
  after firing, so repeated GOs walk the show. Rendered inside the cuesheet
  partial, so it rides the existing sync paths — no new endpoint or poller.

### 12.2 Running wait countdowns

- PreWait/PostWait are invisible while they run — a hung cue and a deliberate
  wait look identical. A shared wait-state (`cuePos`, kind pre/post,
  `endsAtMs`, owned by the auto-continue chain, which switches its sleeps to
  250 ms ticks) rides the existing per-second WS sync.
- UI: the waiting row shows a live pill ("waiting 3.2s"); with
  "GO advances selection", the pending cue's PreWait also counts down in the
  GO bar's next line.

### 12.3 Cue health states

- Persisted per cue: `last_result` (0 = never played, 1 = ok, 2 = error) +
  `last_played_at`. gsp's end hook marks `ok` for a completed cue; any
  load/play error marks `error` (missing-file is the existing startup scan,
  shown separately). Show import/clear resets results; an operator
  "clear results" action exists too.
- UI: glyph + edge tint in the icon column, so a scan mid-show finds broken
  cues. **Jump to next broken cue** hotkey (F8): selection walks to the next
  `error`/missing row and scrolls it into view.

### 12.4 Multi-select + bulk edit

- Selection extends from a single id to an **anchor + set** (persisted):
  Shift+arrows/click extend the range, Ctrl/Cmd-click toggles, Ctrl+A selects
  all visible units, Esc collapses to the anchor. Arrow navigation keeps the
  anchor; Space still plays the anchor cue only. Shift+arrows step the range
  head one visible unit (stepping back shrinks; reaching the anchor clears).
  A range covers **visible rows in sheet order** (`SelectUnits`: collapsed
  members excluded, group headers included); headers persist in the set as
  `-groupID` and highlight, but cue-only consumers (bulk ops, block drag, F8)
  ignore them. Every single-selection write (cue or group) clears the set.
- Row visuals: every selected row shares the anchor's highlight — one
  selection look; anchor identity lives in the GO bar, not a second tint.
- **Bulk actions** (context menu when >1 selected): colour, fade scope/time,
  auto-continue, assign-to-group, delete — one transactional server endpoint
  (`POST /api/cue/bulk` with `{op, value, positions[]}`), not a client-side
  loop of single-cue calls, so half a bulk edit can never persist.
- Dragging any selected cue moves the whole selection as one block
  (relative order preserved).

### 12.5 Cue-number arithmetic on insert

- Auto-numbering (setting, default on): new cues numbered 5, 10, 15…;
  an insert *between* numbered cues takes a fractional number (12.5), kept as
  text — no schema change (`cueNum` is TEXT; existing CAST-INTEGER MAX for
  next-number computation still works).
- **Renumber ×5** action recomputes the visible sequence to clean integers;
  manual inline edits remain the override (auto-numbering never rewrites a
  hand-set number).

### 12.6 Topbar clock

- The current wall clock (locale `HH:MM:SS`) lives in the topbar next to the
  live-status dot, rendered client-side (1 s tick, no server round trip, no
  sync path). A clock must not announce itself to screen readers: `role="img"`
  with an `aria-label` of the time, updated only on focus.
- Later sibling (also spec'd, §12.2): the **show clock** — performance
  elapsed time started by the first GO.

### 12.7 Fade curves (volume envelope automation)

- Today every fade is a flat linear ramp of the shared audio/video envelope.
  Per-cue **fade curve** setting selects the envelope shape `f(t)`, applied
  identically to fade-in, fade-out and the slideshow fade (one ramp function,
  three callers):
  - `linear` — constant slope (current behaviour, default)
  - `smooth` — S-curve (smoothstep: slow start, fast middle, slow end) —
    natural-sounding audio fades, gentler light changes
  - `log` — logarithmic (perceptual: fast initial change, long tail) —
    matches how loudness is heard
  - `exp` — exponential (slow start, sharp finish)
- Data: `cuesheet.fade_curve` TEXT default `'linear'`; validated against the
  list above. UI: a curve picker next to the fade time in the inspector (and
  in the bulk-edit op set, §12.4). Slideshow groups may set a group-level
  curve used for their inter-slide fades.

### 12.8 Remote control protocols (OSC + HyperDeck)

- **OSC** (UDP, port configurable, default 8000): address map
  `/cue/{pos}/play`, `/cue/{pos}/select`, `/cue/next`, `/cue/prev`,
  `/transport/play|pause|stop`, `/panic`, `/showtest/{name}`,
  `/volume/{dB}`. replies minimal; unknown addresses ignored (logged at
  debug). Server binds `0.0.0.0` or a configured address (toggles in
  Settings).
- **HyperDeck Remote Control Protocol** (TCP, default port 9993): Blackmagic
  text-command subset — `play`, `stop`, `record` (ignored/unsupported
  response), `load: <clip>` (select + load the cue whose title/number
  matches), `goto: <tc>`, `transport info`, `notify` — enough for HyperDeck
  controllers, Stream Deck plugins and Companion to drive CuTePi as if it
  were a deck. One client at a time; transport state is reported from the
  gsp state machine.
- **Trust boundary**: neither protocol authenticates (protocol limitation) —
  they are off by default and documented as control-room-LAN features.
- **Configuration lives in the Settings modal**: enable/disable toggle +
  port per protocol (and OSC bind address), persisted in `config.json`;
  the modal lists both servers with their live on/off state.

### 12.9 Panic holding image

- Setting: **"Panic cuts to a holding image"** + holding image picker (from
  the media pool). When on, Panic does not go to black — it immediately
  loads and holds the configured image (full-frame, looping), so screens
  never show dead black mid-show.
- Fallback: holding image missing/unplayable → plain panic to black, logged
  as an error. The setting lives with the other panic/transport settings.

### 12.10 Test patterns (built-in + custom)

- **GStreamer built-ins**: the Tests menu (topbar) lists a curated set of
  `videotestsrc` patterns — SMPTE, SMPTE100, Snow, Black, White, Red, Green,
  Blue, Checkers-1..4, Circle, Blink, Solid, Barcode — played fullscreen via
  the existing `ShowTest` path (no cue created; ESC/Stop ends).
- **Custom patterns**: the operator can flag any media-pool item as a test
  pattern (media context menu → *Add to test patterns*), which pins it into
  the same Tests menu; selecting one Loads it directly (images hold their
  frame). Patterns persist in `state`; clearing unlists them without
  deleting media.

### 12.11 To be considered

Spec'd enough to evaluate, not committed:

- **Output preview thumbnail** — mirror the Pi's HDMI output in the topbar
  (click to expand): pipeline `tee → appsink` → MJPEG endpoint / WS frames.
  The one real pipeline change on the list; costs decode headroom on the Pi.
- **Minimap for long shows** — VS Code-style scrollbar minimap coloured by
  cue colour/type, click-to-jump. Pure client-side render from cuesheet
  data; value grows with show size (200+ cues).

### 12.12 Slated for v2

- **Master/standby machine sync** — a second Pi mirrors the show over the
  network: every playback decision is replayed to the standby over WS so it
  sits one click behind the same state; mid-show failover is a single
  operator action. Requires a command-replay protocol, media mirroring
  strategy and conflict rules — deliberately out of the v1 scope.

### 12.13 Awards Mode (group playback toggle)

A group playback mode for ceremonies: the operator parks the selection on
one group of cues and each GO press alternates **play → fade-stop** on a
single member, without the selection ever leaving the group header.

- **Model**: new `cue_group.awards_mode` flag, persisted like the slideshow
  flags (DB column + `.CTP` manifest + group inspector). Awards and
  Slideshow are mutually exclusive — enabling one clears the other, both in
  the inspector UI and server-side on save.
- **Settings reuse** (no new knobs except the mode flag itself): the group's
  existing `shuffle` picks the play order (off = sheet order, on = shuffled
  no-repeat bag: every member plays once, then the bag reshuffles), `loop`
  decides what happens at the end of the list (on = wrap/reshuffle and keep
  going; off = the stop of the last member moves the selection to the next
  cue after the group, so the following GO continues the show normally),
  and `fade_ms` is the fade-out applied by every stop-GO (0 = hard cut).
  The inspector hides the slideshow-only Hold field when Awards is on and
  labels the shared Shuffle/Loop/Fade controls for their awards meaning.
- **GO cycle** (Space/GO, remote `/go`, HyperDeck `play` — all funnel
  through `FireSelected`, so all transports behave identically):
  - selection on the awards header, session idle → play the cursor member
    (sequence: sheet order from the first; shuffle: pop the bag).
  - session playing → fade the member out over `fade_ms`, then stop;
    advance the cursor (sequence index / bag position).
  - a GO arriving mid-fade is ignored (phase guard); a GO arriving when the
    engine is no longer on the session's member (operator played something
    else meanwhile) restarts the session from the cursor.
  - empty group: GO is a no-op.
- **Selection never advances** on awards GO presses (the `goAdvance` step
  and the deck-style prewarm arm are both skipped) — the single exception
  is the end-of-list stop with Loop off, which steps the selection out of
  the group as above. Arrow keys/clicks move the selection normally; the
  playing cue keeps ringing (leaving never stops audio) and the awards
  session state resets, so the next GO on the group starts fresh.
- **No automation inside a session**: the awards fire path bypasses
  `loadAndPlayCueKeep`'s image-duration timer and the cue-end
  auto-continue chain never fires for a session member (an awards cue with
  AutoContinue set still waits for the operator). Member fires still mark
  `last_result` and emit `cue_start` audit events like any other GO.
- **Scope**: the member list is the group's subtree in sheet order (same
  scope as `playFirstGroupMember`/slideshow), snapshotted when the session
  starts.
