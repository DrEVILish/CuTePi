# CuTePi — Design Guide

Living design/spec + architecture record. Updated whenever scope, architecture,
or requirements change; This is a guide, not a changelog.

## 1. Overview

CuTePi is a media cue-playback controller intended to run on a Raspberry Pi
4/5, driving video/audio out of an HDMI port via GStreamer. A Go backend
serves an htmx-driven "control centre" web UI (themes provided by ftl-themes);
SQLite3 is the single source of truth for state (media library, cue list,
selection, settings).

- **Server**: Go + gin, htmx v4 (htmax.js) for interactivity, no client-side framework.
- **Client**: HTML5/CSS3/vanilla JS, deps vendored under `public/src/` (no CDN).
- **Media tooling**: ffmpeg/ffprobe (metadata, thumbnails, waveforms), yt-dlp
  (URL downloads).
- **Playback**: GStreamer, including test-pattern playback.
- **DB**: SQLite3 via `sqlx`
- **Config/defaults**: `~/cutepi/config/config.json` (env-overridable), port
  3001 default in dev, port 80 in production and test, media in
  `~/cutepi/media/`, thumbnails in `~/cutepi/thumbnails/`.
- **Trust model**: trusted LAN appliance; optional operator password (see §7).
- **Multi-client sync**: WebSocket hub with server-authoritative state. No
  polling — all status flows over the socket.

## 2. Product decisions

- **Platform**: Raspberry Pi 4/5, headless, video+audio out of HDMI, pure
  server Debian Trixie (raspbian) — no X11/Wayland; gstreamer playback drives the HDMI connector directly, not a windowed sink.
- **Audio**: HDMI embedded ALSA by default, **exclusive to CuTePi** (only
  audio producer). The operator may pick a different output device in the
  Settings Audio tab (enumerated from `aplay -L`); the engine routes through
  `alsasink` when a device is set.
- **Frame rate**: video output **always matches the frame rate the display
  is set to** (a 1080p60 wall shows 60 frames a second, whatever the clip's
  own rate): no dropped or stalled frames during playback or during fades.
  Clips at a lower rate repeat frames evenly to fill the display rate.
- **Output quality first**: what reaches the screen is the top priority. Performance work may not coarsen fades,
  freeze pictures mid-crossfade, or lower resolution or frame rate to save work.
- **Codecs**: CuTePi plays **any codec the user provides** — every file GStreamer can decode, video, image or audio.
  Decode is **hardware-first (V4L2 H.264 and HEVC on the Pi 4; HEVC only on the Pi 5) with software fallback** via
  GStreamer autoplugging; the hardware formats are fast paths, never the supported set. When a file is expected to play
  below full rate on the current hardware, **import warns** (media pool and inspector) but never refuses it (§5.7).
- **Undecodable sources**: rejected **at import** with the reason, by the playback engine itself: the file must
  preroll through GStreamer (`playbin` into fake sinks, same autoplugging and decoder ranks as playback,
  `gsp.CheckDecodable`), then pass `ffmpeg -v error -t 1`. ffprobe reads formats this system's GStreamer may not play,
  so a probe alone would accept files that only fail at cue time. Accepted files that still fail at cue time surface
  the error to the user then. The file extension never decides (any extension imports if it decodes).
- **Transparency and animation**: CuTePi plays **video with an alpha channel** (ProRes 4444, QuickTime Animation,
  PNG-in-MOV, CineForm RGBA, HAP Alpha, FFV1 and VP9 with alpha, …), **stills with transparency** (PNG, TIFF, WebP,
  GIF) and **animated images** (animated GIF, APNG, animated WebP) — transparent areas show the layers beneath (black
  when nothing is beneath), and animations play at their own frame timing. Design: §6.1.3; tests: §8.
- **Decoder ranks**: decoders that autoplugging would pick but that fail on the Pi are demoted
  (`gsp.decoderRankOverrides`): `v4l2jpegdec` (unreliable firmware path; software `jpegdec` instead) and
  `openjpegdec` (fails to negotiate JPEG 2000; `avdec_jpeg2000` instead).
- **Codec test corpus**: `tools/codec-corpus/` generates 52 short files in many codecs and containers (video, audio,
  image) with the Pi's own ffmpeg/GStreamer, probes how GStreamer decodes each (decoder, hardware or software, speed),
  and runs each end to end through the live service (upload, play, output checked from outside). Results:
  TEST_REPORT "Codec corpus".
- **Cue trigger**: **SPACE = GO** and **ESC = fade out** (twice = panic) work from anywhere in the control UI —
  focused buttons, checkboxes, selects, sliders and open dialogs included — except while typing into a text field.
  Space never presses the focused control instead, and holding a key does not repeat GO or panic. (An open
  right-click menu takes the ESC to close itself, §5.4.) With nothing selected, GO resumes transport.
  Media-pool tiles are never selectable — they join the sheet via
  double-click, Enter, or the tile menu.
- **Playback end**: a cue **stops** after playback — cues are a list, not a playlist; there is no implicit advance. Auto-continue is explicit and per-cue.
- **Waits**: `preWait` applies before every start, `postWait` after every end.
  Auto-continue advances down the sheet; a stop on a group row triggers the group's action and continues.
- **Loop / Hold defaults**: `loop=off`, `hold=off` for new cues. `loop` always wins over auto-continue; `loop_count` (0 = infinite, N = play N times) is exhausted before an auto-continuing cue advances.
- **Trim**: `posStart`/`posEnd` are timecodes *into the source media*; 0 =  untrimmed at that end; values stored as given (no normalization).
- **Cue groups**: visual folders, nestable, collapseable, hold cues, a group can act as a playlist.
- **Slideshow**: a group of images can run slideshow mode from the cue group inspector — shuffle / loop / fade / duration-per-image on `cue_group`;
  images are visible cue rows inside the group with a "now showing" indicator.
- **Awards Mode**: a group of audio tracks can run awards mode from the cue group inspector - shuffle / loop / fade, audio tracks are visible cue rows inside the group with a    "now showing" indicator.
- **Settings editing**: all operators may edit
- **Output level**: system output always 100%; volume is a **per-cue** control only (no global/master).
- **Missing sources**: startup scan flags missing files; pool tile shows a  warning triangle; the cue shows a warning offering **delete the cue** or **re-link to another media file**. No periodic scan.
- **Show export/import**: `.CTP` = ZIP of a JSON manifest (cuesheet +
  selection, per-cue settings, groups incl. nesting + slideshow settings —
  manifest v2) + referenced media; import restores with a modal choice:
  **append to end or overwrite**.
- **Logs**: viewable in the Web UI with level filter + clear; changing the level changes what is **recorded** (runtime switch). Structured playback events form an **audit trail** (append-only ring; clearing the log never clears it).
- **yt-dlp**: URL import is a core, imperative feature (not a convenience).

## 3. Architecture & repo layout

```
config/config.go       config load/save, path expansion, defaults, auth password
ctp/ctp.go, db.go      domain types + SQLite schema/access/migrations
gsp/gsp.go, background.go  GStreamer pipeline manager (single mutex-guarded pipeline, atomic swap; warm preroll slot; separate playbin soundtrack pipeline), state version counter
gsp/wall*.go, kms.go   video output: the GPU compositor wall (§6.1, being built) and the KMS display-plane wall (current default, kept as fallback)
media/                 ffprobe metadata (Probe), thumbnails, waveform peaks
worker/                DB-backed thumbnail/pending queue (survives restart)
routes/*.go            HTTP handlers: public, index, api (embeds themes/display/audio), groups, show (export/import), logs, upload, youtube; background listeners for scheduler, HyperDeck, OSC UDP + TCP/SLIP
logs/logs.go           logging + ring; audit trail
ws/                    WebSocket hub (server-authoritative sync)
templates/*.html       server-rendered htmx fragments + pages (index, cuesheet, cueinspector, groupinspector, cueeditcol, mediainfo, mediacontrols, settingsModal, testModal, qrModal, ytdlModal, logsModal, showModal, uploadModal, …)
public/                static assets: css/, src/ (bootstrap, htmx, dnd, dropzone, ui.js), img/ (logo, placeholders), fonts/; icons and theme packs served from the ftl-themes submodule via the `/ftl/` route
main.go                entrypoint / server wiring / restart
```

**Split of responsibilities**: playback/cues/selection/media-library state are server-authoritative; the browser is a mirror (WebSocket). Purely cosmetic UI state — panel sizes, theme choice — stays in `localStorage`.

**htmx conventions**: htmx v4 vendored, explicit attribute inheritance. Successful state mutations return the complete target partial with `outerHTML`;
status-only actions use `hx-swap="none"`;
4xx/5xx responses never replace application panels (suppressed in `ui.js`).

## 4. Data model (SQLite)

Tables come from the schema in `ctp/db.go` (source of truth). Conceptual
`media` → **`mediapool`**; `cues` → **`cuesheet`**.

- `mediapool`: `media_id`, `filename` (unique), `mimetype`, `size`,
  `duration` (float sec), `resolution`, `thumbnail_pending`, `waveform`
  (JSON peaks), `waveform_pending`, `missing` (source absent from disk),
  `loudness_gain` (EBU R128), `media_meta` (ffprobe JSON for the Media tab),
  `date_added`. `codec` and `media_title` were removed by migration
  (write-only, read by nothing) — codec detail lives in `media_meta`.
  Sort newest-first (`date_added DESC, media_id DESC`).
- `cuesheet`: `cue_id`, order `cuePos` (unique, stable identity — never
  reindexed); `cueNum` (TEXT label); `media_id` FK → mediapool
  (ON DELETE CASCADE); `title`; `preWait`, `cueDuration`, `postWait`, trim
  `posStart`/`posEnd` (ms, 0 = untrimmed); `hold`, `loop` (both default 0),
  `loop_count` (0 = infinite); `color`; `parent` (`cue_group.group_id`,
  0 = top level). `parent` is the single source of membership truth: it is
  written explicitly by the op that moves or creates the cue, and never
  re-derived. Playback controls: `fadeOut`, `fadeAction`, `fadeIn`,
  `fade_curve`, `volume` (dB, default 0 = 0dB), `mute`, `balance`, `rate`;
  geometry: `fit_mode`, `rotation`, `flip`; health: `last_result`,
  `last_played_at`; scheduling: `schedule_enabled`, `schedule_days`,
  `schedule_time_ms`; `autoContinue`. `sheet_index` is the visual **and**
  playback order (what-you-see-is-what-plays); `cuePos` is the stable
  identity. The renderer reads both literally and never writes (see §6.4).
- `cue_group`: `group_id`, `name`, `parent_group_id` (nesting; validated
  acyclic, depth ≤ 8), `collapse`, `slideshow`, `awards_mode`, `shuffle`,
  `loop`, `fade_ms`, `duration_ms`, `cue_num`, `color`, `anchor_pos`
  (legacy: pre-`sheet_index` empty-group anchor), `sheet_index` (header
  position in the visual sequence).
- `state`: small server-authoritative key/value store for operator
  preferences — persisted selection (cue `cuePos`, 0 = none; multi-select
  anchor + set), `escFadeMs`, `goAdvance`, `showMode`, `autoNumberCues`,
  `panicHoldImage`, `testPatterns` (pinned custom patterns).
- `config.json`: port, `loop` (direct-load default), `auth_password`,
  `allowed_hosts`, working-dir paths, audio device/channels/rate, hotspot
  SSID/password, remote enable flags + ports (see §12.8). Written atomically
  (temp file + rename) with file mode `0600`.
- `<working dir>/tmp`: scratch space for uploads, `.CTP` imports and yt-dlp
  downloads (see §7). Emptied at startup; `TMPDIR` points here.

## 5. UX / UI design (per panel)

### 5.1 Overall shell and themes

Exactly 100vh: topbar (natural height) + content row (media pool | cuesheet panes) with no page-level scroll; only the panes scroll internally (`scrollbar-gutter: stable`).
Themes come **only** from **ftl-themes** (git submodule `third_party/ftl-themes`, contract v4, v4.1, tracking
`main`); CuTePi ships no themes of its own. The picker lists the submodule's `dist/themes.json` (34 themes, default
`ftl:xbmc`); each theme links its bundle `/ftl/themes/<slug>.css`, sets `html[data-theme=<slug>]`, and its icon sprite
`dist/icons/<slug>.svg`. The choice is browser-local (`localStorage` `cutepi.theme`); older saved values (`lcars`,
`app:blue-future`) map to the ftl theme of the same name.
- **Sub-themes and tint** (contract "Palette variants" and "Theme tint"). Beneath the theme, Settings › Appearance
  offers **Style** (the theme's `variants`, plus Standard; sets `html[data-variant]`) and, for a theme that declares a
  `tint`, a colour control labelled with the theme's own name for it (Win7 Aero: "Window Color", applied as the inline
  custom property `tint.token` on `<html>`, live while dragging, with Default to clear it). Each field is hidden for
  themes without it. Both are saved per theme slug in this browser (`cutepi.theme.variant.<slug>`,
  `cutepi.theme.tint.<slug>`) and applied by the pre-paint boot script, so there is no flash. Choosing a Style clears
  the custom colour, because a variant may be a tint preset. `?variant=` and `?tint=` preview for one page view, like
  `?theme=`. A tint reaches the page only if its token is a custom-property name and its default a `#rrggbb` colour
  (`routes.validTint`).
- **Cascade**: Bootstrap (kept for its JavaScript and a few utilities) < ftl-themes (`@layer ui`) < CuTePi's own
  unlayered CSS. Bootstrap is imported into `@layer bootstrap` because v4 dropped the `ftl-` class prefix and 35 class
  names now match Bootstrap's.
- **Library markup**: components use the ftl-themes v4 class names (`.btn`, `.input`, `.field`, `.tab`, `.modal`, …).
  Dialogs follow the library's window pattern: the themed window is `.modal-content.modal` with the title and a `.btn-close`
  in `.modal-header` (the theme draws the title bar, frame and close box); Bootstrap's modal JavaScript still drives
  them, but its full-screen overlay carries `.app-modal` so no theme ever styles it. App CSS lays out only what is inside
  a window. The cue sheet's rows are `tr.sheet-row` (not `.row`, which the library defines as a flex row).
A theme change restyles the shared pane chrome (pool, inspector) together.
Each theme draws its own dialog windows (e.g. Windows 95/XP/7 title bars and close boxes, LCARS elbow frames).

### 5.2 Top bar

One row, left to right:

- WebSocket status dot (hover: a styled card with live updating connection facts — live/offline, connected client count, server uptime)
- Wall clock (locale `HH:MM:SS`, tabular numerals).
- The GO button (the loudest control on screen)
- *selected* cue it fires
- playing cue number - cue name, progress indicator, time, remaining time, and a **Pause / resume** button. While
  paused it turns amber and reads **PAUSED** (a paused still looks unchanged on the wall, so the button carries the state).
  The bar's clock updates in place; its buttons are never re-rendered under the pointer, and a press held across a refresh
  still counts. The clock, the scrubber and the playing cue's row progress bar run from one client-side playback clock
  (100 ms steps, re-anchored by each server sample, never stepping backwards), so they glide instead of jumping.
- full screen button
- menu dropdown, Panic (confirm), Stop, Fade out, Export / Import / Logs / Restart / Shutdown.

### 5.3 Media Pool (left pane)

- multi-column, 16:9 thumbnails (video = frame at `duration/2`; image = copy; audio = `showwavespic`); per-type placeholder SVGs while pending;
  hover reveals details (name, mime, size, date added); column count tunable via a header stepper, persisted.
- **Hover details**: the info overlay is clipped to the tile; when the tile is too small for it (rows truncated), the details pop out in a styled
  floating card near the tile instead of spilling.
- Filters: filename text + type dropdown (video/image/audio/all). Sort: newest first (`date_added DESC, media_id DESC`).
- 3-dot menu per tile (bottom-right, opens downward next to the button):
  **Add** (as cue), **Delete** (confirm modal), **Refresh thumbnail**, **Analyse** (waveform rebuild).
  Deleting a media file also removes it from the custom test patterns (§12.10) and, if it was the panic holding image
  (§12.9), clears that setting.
- Double-clicking a tile adds it to the cuesheet as a new cue (same as **Add**).
- Drag-and-drop upload onto the pool (multi-file); items are draggable into the cuesheet (drop on a group header assigns membership).
- Panel collapsible to a sliver / drag-resized; width + collapsed state persisted. Scrollbars always visible. Empty pool shows info drag to upload placeholder.
- Missing source: warning-triangle icon on tiles whose file is absent from disk (startup scan only).

### 5.4 Cuesheet (right pane)

**Firing visuals on the sheet**: the playing row has an indicator on the left of the row, and a cue in a chain wait shows a live seconds pill.
- Live Progress bar shown as background behind numerals of PreWait, Duration, PostWait if one of those items is in progress.
- Sticky header, scrollable body. Columns: **icon** (media type / missing warning), **Number**, **Name**, **PreWait**, **Duration**, **PostWait**.
  per-cue actions live in the Inspector and the row context menu.
- All cells editable by double-click; save on blur/enter. **Escape cancels** the edit: the editor closes, the old value
  stays and nothing is saved (the Escape does not reach the transport, so it never fades out the show).
  Time parser: `hh:mm:ss.ms` or a bare number = seconds. Times (PreWait, Duration, PostWait) have **no upper limit**.
- **Cue numbers are unique** across cues and group headers. A blank number is allowed on any number of rows, and
  `12` and `12.0` count as the same number. Editing a number to one already in use (inline, or `cue_num` in the Group
  Inspector) is refused: the field goes back to its previous value and a tooltip on it reads "Cue number N is already
  used" for a few seconds. This is an operator mistake, not a fault, so nothing is logged and no error toast appears.
  A show imported in append mode keeps each cue's and group's number when it is free; a clashing number is replaced by
  the next whole number above the highest in the sheet.
- **Groups** are first-class rows: numbered (editable `cue_num`, maxlength 24), coloured, selectable and nestable — cue and subgroup headers render indented inside their parent's block**
  **An open group's block is enclosed by a fine hairline border** — the header draws the top edge, the last member the bottom, every row the sides so membership reads at a glance; collapsed groups draws the top edge, sides and bottom. Members' names indent same as subgroup headers; a collapsed group skips its whole subtree in keyboard nav. Right-click group rows: Delete group; group headers are draggable to move the whole subtree block.
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
- **Sort and renumber** (right-click a cue, a group header or blank sheet space, and the top-bar menu):
  - **Sort by cue number** reorders the sheet by number: numeric numbers by value, then text numbers alphabetically,
    blanks last. Sorting happens within each group and at top level, so members stay in their group and a group's
    block moves as one; a group header without a number sorts by the lowest number inside it.
  - **Renumber cues** (confirm) rewrites every number in sheet order as step, 2×step, 3×step… — 1, 2, 3… at the default
    step of 1 (Settings → General → Cue number step). Group headers that have a number take the next one in the same
    sequence; blank headers stay blank.
- **Number and time fields are text fields** everywhere (inspectors, sheet cells, settings), validated as you leave them:
  - Times accept `1:05.000`, `1:05`, `1m5s`, `1m 5.5s`, `65` (bare number = seconds), `500ms`, `1h2m3s` — all mean the
    same thing server-side (`ctp.ParseTime`) and in the browser.
  - Whole numbers and decimals are checked against their range.
  - An invalid entry is put back to its previous value with a tooltip saying what is accepted; nothing is sent.
- Selection: single-select, arrow-key navigable (Up/Down walk cues + group  headers; Right/Left open/close a selected group). Persisted in the DB; `POST /api/cue/:pos` selects. Space (or transport Play) acts on it.
- **Context menu** (cue rows): colour, delete. Escape closes an open context menu (and does nothing else: no fade-out).
- **Missing source** cues: warning badge + the Inspector shows a Re-link / Delete cue banner.
- **Scheduled** cues (schedule enabled, any time including midnight) show a clock icon after media icon.

### 5.5 Cue Inspector (bottom-docked panel)

- Built and behaved like the Media Pool pane: shared resizer/collapse chrome
  (collapse = fully hidden, one form spanning all tabs so any change saves instantly (htmx `change delay:200ms`).
- Tabs (static strip in `index.html`; audio panes are omitted for image cues:
  - **Time** — waveform trim timeline (canvas of JSON peaks, draggable In/Out markers; **only dragging a handle changes trim**; clicks elsewhere are inert),
    Trim In/Out fields, Pre-Wait, Post-Wait, Loop + loop-count, Hold-last-frame, Auto-continue, fade-stop scope/time, playback-rate slider with 1× reset.
    Renders even where duration is unknown (timeline duration-gated). The timeline shades the shared audio+video
    fade-in/out envelope over the trim window (same curve the engine ramps).
  - **Video** — video Fade In / Fade Out (times), then how the picture sits on the wall:
    - **Frame fit** (inside the picture's box — the display unless Position & size is set):
      **Fit** (whole picture, aspect kept, letterboxed), **Fill width** (as wide as the box; top/bottom overflow
      cropped), **Fill height** (as tall as the box; left/right overflow cropped), **Fill** (covers the box; the
      overflowing sides cropped, aspect kept), **Stretch** (fills the box exactly, aspect not kept). Overflow crops are
      centred.
    - **Crop**: Left, Right, Top, Bottom of the source picture, each in source pixels (`120`) or a percentage of its
      width/height (`10%`), applied before the fit. The display hardware crops (the plane reads only that part of the
      frame): no CPU, full frame rate. A crop that would leave under 16 px is ignored.
    - **Rotate** 0/90/180/270° and **Flip / Mirror** (none, mirror ↔, flip ↕). The display hardware does 0°/180° and
      mirroring; 90°/270° are turned in software after the frame is scaled to its on-screen size (full frame rate at 1080p).
    - **Opacity** 0–100 %: how solid the picture is over whatever is underneath (100 = opaque).
    - **Position & size**: X, Y, Width, Height, each blank (fill the display), pixels (`960`) or a percentage of the
      display (`50%`). The picture is fitted into that box.
  - **Audio** — output device picker (Settings Audio tab source; default
    HDMI embedded), volume (dB slider −60..+12, double-click resets to 0 dB), Fade In / Fade Out, Balance/Pan
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

### 5.6 Settings modal

A fixed-size window (820 × 640, smaller screens: full height) with a compact title bar, the tab rail on the left
(a swipeable row on phones), one scrolling pane and **Cancel / Save always in view**; switching tabs never resizes it.
Tabs, grouped by purpose:

- **Appearance** — theme (saved in this browser).
- **Playback** — ESC fade-out time (default 1000 ms, any time format), panic holding image (Clear), Advance selection
  after GO, Loop directly played clips.
- **Cue sheet** — Auto-number new cues, cue number step (default 1).
- **Display** — the HDMI output's current mode (read-only, e.g. `1920 × 1080 @ 60 Hz`), use the display's own mode
  (EDID), manual resolution and refresh rate.
- **Audio** — output device picker (default HDMI embedded ALSA; Custom for an ALSA name) + channels/rate, persisted to
  `config.json`.
- **Network** — instance name (renaming changes the machine hostname and refreshes mDNS, so the crew reaches it at the
  new `.local` address), web port (restart), remote control: one on/off toggle per protocol (HyperDeck, OSC UDP,
  OSC TCP/SLIP), all **off by default**, with ports, live status, listen address and the HyperDeck clip-list source
  (Cue sheet by default, or Media pool); Wi-Fi hotspot (SSID, password, join QR code).
- **Security** — operator password (plain text; see §7), remove password.
- **Keyboard** — the shortcut list.

### 5.7 Upload

- Desktop: modal from the mediapool (or Drag and drop onto the pool). Mobile: standalone `/upload`. Both: drag-and-drop + file picker, multi-file, **no size limit** —
  before uploading, warn if the total exceeds the available disk space.
  Any video, image or audio file is accepted, whatever its extension; the import checks reject files the playback
  engine cannot decode (422, with the reason — e.g. "this system has no GStreamer demuxer for video/x-ms-asf"). Metadata is extracted synchronously
  (validate-then-render: the pool only shows validated media); duration/resolution/codec failures reject the import.
  YouTube/URL via yt-dlp with stage logging.
  Uploads, deletions, and thumbnail changes broadcast a targeted WebSocket refresh.
- **Playback warning (planned)**: from the probed codec, resolution and frame rate, import estimates whether the file
  plays at full rate on this hardware (hardware decoder or software, and its measured capacity) and shows a warning on
  the pool tile and in the inspector when it will not, e.g. "HEVC 2160p: software decode, about 20 fps expected on this
  Pi". It informs; it never refuses a file.
- Ensure acurate and live updating progress bars for all upload / media import tasks.
- **Name clashes**: before any bytes are sent, the browser asks the server (`POST /upload/check`) which chosen files
  share a name with a file already in the pool, or with another file in the same batch. If any do, a dialog lists
  them and the operator picks one answer for the batch:
  - **Replace** — the upload overwrites the pool file of the same name (cues using it keep working);
  - **Keep both** — the upload is renamed `name (2).ext` (then `(3)`, …);
  - **Skip** — the pool file stays and those uploads are not sent into the pool;
  - **Cancel** — nothing is uploaded.
  The choice travels as the form field `onConflict` (`replace` | `rename` | `skip`); an upload that clashes without
  one is refused with 409 and nothing is changed. The finished upload reports what happened ("Uploaded clip.mp4",
  "Uploaded 2, skipped 1 already in the pool").
- **Every upload gives feedback** on every client, phones included: the chosen file names, then progress, then a
  success line or a failure line with the server's reason. Tapping "Choose a file" opens the picker exactly once.

### 5.8 Show export / import (`.CTP`)

- **Export** (`GET /api/show/export`): ZIP of `cutepi.json` (cuesheet +
  selection, per-cue settings, **groups incl. nesting + slideshow settings —
  manifest v2**) + referenced media files (`media/<filename>`).
- **Import** (Show modal): restore with **append to end** or **overwrite**;
  all referenced media validated as available (in the `.CTP` or local pool)

### 5.9 Log viewer & audit trail

- Web-UI modal (`GET /api/logs`): filter/record by level (debug/info/warn),
  clear action. Runtime level switch affects what is **recorded**.
- Audit trail (`cue_start`/`cue_end` events with pos/title/wall-clock) is an
  append-only ring; clearing the log never clears the audit.

### 5.10 Show / Edit mode

The top-bar toggle switches between **EDIT** and **SHOW**; the mode is stored on the server (`showMode`), so it
survives a reload and is the same on every client.

- **SHOW** hides the media pool, the Cue Inspector and the Tests button. Editing gestures (double-click to edit,
  drag and drop, delete) are refused with the toast "Switch to EDIT mode to change the sheet", and the test
  pattern API answers 403.
- The **transport stays live** in Show mode: arrow keys move the selection, Space fires GO, Escape fades out,
  and the menu's Stop / Fade out / Panic all work.
- Scheduled cues arm only in Show mode (§6.8b), and live Seek is disabled (§6.2).

### 5.11 Feedback: toasts and tooltips

Errors from the server appear as a red toast with the server's reason. Success toasts are few and confirm
actions whose effect is not otherwise visible:

- "Holding image set — PANIC now cuts to *file*" (tile menu, §12.9);
- "Added to test patterns" / "Removed from test patterns" (tile menu, §12.10);
- the upload summary from the Upload modal ("Uploaded *file*", "Uploaded 2, skipped 1 already in the pool").

Operator mistakes on a single field (a duplicate cue number) are shown as a short tooltip on that field, not as a
toast, and are not logged.

## 6. Playback & features

### 6.1 Pipeline manager (`gsp`)

Single mutex-guarded pipeline handle in a `manager` struct; atomic
stop-then-replace via `swap()`; EOS/error clear the current reference
(`clearIfCurrent`); one `buildPipeline()` for files and test patterns. A cue
position (`CurrentCuePos`), a generation counter (`Generation`, bumped on
every load), and playing-file (`CurrentPlaying`) are tracked for guards.
GStreamer runtime is smoke-tested against real `gst-launch-1.0`
assets (`gsp` test; skips when absent).
- **Wall: GPU compositor (target design, being built — §6.1.1).** Replaces the per-cue display planes below as the
  default once it matches them feature for feature; until then it is selected with `CUTEPI_WALL=gl`.
- **Wall: one display layer per cue (KMS)** — current default and fallback. With `CUTEPI_WALL_SINK=kmssink` (the Pi
  default, set in the unit's `playback-env.conf`) every pipeline that shows video gets its own hardware overlay plane on
  the HDMI output (`gsp/kms.go`, `gsp/wall.go`). All `kmssink`s share one DRM file descriptor, which the service opens at
  startup and holds as DRM master. The display controller does the compositing: per-plane alpha (fades, crossfades,
  opacity), zpos (stacking), position/size (render rectangle) and 0°/180°/mirror rotation — no CPU, at the display's
  refresh rate.
  - Video chain: `queue → kmssink` for hardware-decoded frames (DMABuf straight to the plane, no copy); `queue →
    videoconvert → capsfilter → kmssink` for system-memory frames (software decode, stills, test patterns). The
    capsfilter limits converted frames to layouts the kernel can allocate as display buffers (4:2:0 YUV and RGB): the
    planes also list 4:2:2/4:4:4 YUV, but allocating those fails, so DNxHR and ProRes 422 never prerolled; 4:2:2
    sources now go to RGB with full chroma; 90°/270° add
    `v4l2convert → capsfilter(I420, on-screen size) → identity drop-allocation → videoflip`; Stretch adds a
    `capssetter` pixel-aspect rewrite.
  - Fades write the plane's alpha from elapsed time on a per-layer writer. Each alpha write is a display commit that
    waits for a vblank, as is each video frame. The writer steps on a fixed grid counted from the start of each write
    (the commit's own wait is part of the step): **every two refreshes (30 steps a second) while the layer's video is
    moving** (a new frame within the last four refreshes, read from the sink's `stats.rendered`), so a 30 fps clip keeps
    every frame through a fade; **every refresh (60 steps a second) for a layer showing no new frames** (a still, a
    paused or held clip). Fade loops post levels every 8 ms (`fadeTick`) and the writer keeps only the latest, so a
    fresh level waits at each vblank. A 60 fps clip shows about 31 fps while it fades (the CRTC takes 60 commits a
    second in all). Alpha is blended over the black primary plane, so colours scale evenly (no hue shift, no grey
    wash).
  - **Frame-rate limits (measured on the Pi 4 at 1080p60; TEST_REPORT O1)** — the reason for the GPU compositor.
    Each `kmssink` frame is a blocking `SetPlane` commit and each alpha write is another commit on the same CRTC. The
    driver serialises them, so the whole wall gets **60 commits a second**, shared by every plane and every alpha
    write: one 1080p60 or 720p60 H.264 clip plays at 60 fps; a 60 fps clip fades at about 42 fps; two layers at once
    get about 27 fps each; two 1080p60 H.264 layers freeze. HEVC reaches the plane only through a software untile
    (about 1 fps at 1080p60): `kmssink` cannot import the decoder's SAND128 frames (it describes them as linear, and the
    kernel refuses the framebuffer).
  - The primary plane underneath is the console framebuffer, kept black: at startup the service switches the active
    virtual terminal to graphics mode (`KDSETMODE KD_GRAPHICS`), so no console text, login prompt, kernel message or
    cursor reaches HDMI while it runs; text mode returns on a clean shutdown.
  - Fallback (`fbdevsink`): the older single-picture chain (`videoconvert → videobalance → videoscale → videoflip ×2 →
    videoconvert → fbdevsink`) without layers, crossfades, opacity or geometry.
- **Extra streams**: only the first audio and first video stream play; further ones (a second language, an AC3
  track) drain into a real-time (`sync=true`) fakesink. Unsynced they ran ahead and made the pipeline's position — the
  furthest sink — leap (~20 s on the test film), which misreported the clock and trim-out.
- **Audio chain**: `queue → audioconvert → audioresample → volume →
  audiopanorama → scaletempo → sink` (`alsasink` when an Audio device is
  set — see §5.6 — else the default sink).
- **Wall sink**: the pipeline drives the HDMI connector directly, not a
  windowed sink; decode is hardware-first (v4l2 h264/hevc) with software
  fallback via GStreamer autoplugging.
- **Warm slot**: the next cue can be prerolled (`Warm`) and activated on GO (`InstallWarm`) for ~0-latency starts; a
  400 ms prewarm budget falls back to a cold build. Images are exempt from warm. On the KMS wall the warm cue prerolls
  on its own plane at alpha 0 (invisible) — nothing to relink; on fbdev it prerolls into a `fakesink` and the wall sink
  is relinked at activation. Geometry, rotation and mirror come from the cue's own options at build time, so a
  prewarmed cue is framed exactly like a cold one.
- **Soundtrack**: slideshow audio cues become a background-music playlist on
  their own `playbin` pipeline (`gsp.BackgroundPlaylist`) under the slides
  for the whole run. The soundtrack plays **only as part of a slideshow** —
  any main-pipeline decision (Stop, Panic, a new load) kills it with it.

### 6.1.1 GPU compositor wall (target design)

**Why.** The KMS plane wall cannot give full-rate fades, full-rate multiple layers or hardware HEVC on the Pi 4 (§6.1).
GStreamer's GPU mixer does all three on the Pi 4's V3D GPU, measured before this design was adopted (TEST_REPORT O1,
"GPU compositor feasibility"): on the HDMI display at 1080p60, one HEVC layer 59.8 fps, **two HEVC layers at 50 %
opacity 59.9 fps**, H.264 + HEVC at 50 % 60 fps once running, 0 frames dropped; headless throughput 176 fps for one
HEVC layer, 130 fps for two, 91 fps for three.

**Shape.** One long-running **wall pipeline** owns the display for the life of the service; cues come and go as
sources feeding it.

```
cue pipeline (one per playing cue, as today: decode, trim, seek, rate, loop, audio)
  … → decoder → glupload → glcolorconvert → RGBA GLMemory → appsink (bridge)
                                                                  │  exact timestamps (below)
wall pipeline (always running)                                    ▼
  appsrc (pad per cue layer) ─┐
  appsrc …                    ├→ glvideomixerelement (background black, one pad per layer:
  panic image (armed pad)  ───┘     alpha, zorder, xpos/ypos/width/height)
                                 → capsfilter (display size and refresh, e.g. 1920×1080 @ 60)
                                 → glimagesink (GBM/KMS: renders into scanout buffers, one page flip per refresh)
```

- **Mixer element.** Use `glvideomixerelement` with one explicit `glupload → glcolorconvert` per input. The
  convenience `glvideomixer` bin inserts extra per-frame conversions and managed only 33–40 fps for a single 1080p
  layer; the bare element manages 87 fps (H.264, decoder-bound) to 176 fps (HEVC).
- **Display.** `glimagesink` with the GBM window system (`GST_GL_PLATFORM=egl GST_GL_WINDOW=gbm`,
  `GST_GL_GBM_DRM_DEVICE` = the HDMI card) renders the composited frame into scanout buffers and page-flips once per
  refresh on the primary plane. It is the DRM master, so the service no longer opens the display itself in this mode
  (the per-plane code in `gsp/kms.go` is not used). GPU buffers cannot go to our own overlay planes instead: V3D
  renders in a tiled layout (UIF) that the display controller cannot scan out, and a CPU readback is too slow.
- **Every codec.** Hardware-decoded frames enter the GPU without copies: H.264 as DMABuf, HEVC as `DMA_DRM` NV12 with
  the Broadcom SAND128 modifier, which Mesa's V3D driver samples directly (this is what makes hardware HEVC usable).
  Software-decoded video does **not** go up from system memory: on V3D a `glupload` from system memory tiles every
  frame on the CPU (measured 2026-10-02: 41–52 fps for one 1080p layer into the mixer, 27 fps for two; 10-bit 4:2:2
  33 fps). It goes through the Pi's ISP instead: `videoconvert n-threads=4` (only when the decoder's format is not one
  the ISP takes: 10-bit, planar 4:2:2) → `v4l2convert` (ISP) writing **NV12 into DMABufs** → `glupload` imports the
  DMABuf without a copy. Measured: one layer 111 fps, two 97 fps, three 47 fps (the ISP converts about 140 1080p
  frames a second); with real files MPEG-2 33 → 70 fps, DNxHR HQ 38 → 70, FFV1 46 → 79, VP9 43 → 62, ProRes 422
  29 → 49, H.264 10-bit 9 → 28 (TEST_REPORT "GPU upload routes"). Codecs whose decoder is slower than the display
  (ProRes 4444, CineForm, AV1, Theora, 10-bit H.264) stay decode-bound. Alpha sources (RGBA) and stills keep
  `glupload` from system memory: a still uploads once, and the ISP route for alpha is still to be measured. Open: the
  ISP shares the VideoCore with the hardware H.264 decoder; measure an ISP layer beside a hardware-decoded one.
- **Bridge rules (proven by the spike, 2026-10-01).**
  - *Attach a layer only once its format is known.* The video mixer waits for every input's caps before it produces
    anything, so an input created ahead of its first frame freezes the whole wall. A cue prerolls first; its first
    sample's caps configure the layer's `appsrc`, and only then is the mixer pad requested and linked.
  - *The timing source is generated on the GPU.* The wall's always-on black input is `gltestsrc is-live=true`, not a
    CPU-uploaded `videotestsrc` (a 1080p CPU black source alone cost half the frame rate).
  - *Answer the decoder's allocation query at the bridge.* Hardware decoders that output DMABuf/`DMA_DRM` refuse to
    negotiate unless downstream supports `GstVideoMeta`, and they size their buffer pools from downstream's answer.
    The `appsink` answers neither, so a pad probe on it adds the video-meta API (registered through
    `GST_VIDEO_META_API_TYPE`, not looked up by name: the type does not exist until the video library first uses it)
    and a pool-size hint (no pool of our own) covering the frames held past the bridge: the appsrc queue, the upload,
    the mixer and the display flip. Without the hint the decoder stalls after a dozen frames.
  - *Move frames in C, never through Go objects.* go-gst releases samples and buffers in Go finalizers, so a Go
    per-frame loop holds decoder buffers until an unpredictable garbage collection and starves the decoder. The
    per-frame loop is a cgo function: pull the sample, shallow-copy the buffer (memory shared, no pixel copy),
    re-stamp it, push it (the push takes the copy), release the sample. It runs on its own OS thread per layer.
  - Spike results on HDMI at 1080p60 through the full bridge: HEVC ×1 59.9 fps, HEVC + HEVC at 50 % 59.8 fps
    (cues decoding 60 fps each), 3–4 frames dropped at start-up only. **Open:** H.264 layers import as three-plane
    YU12 (the H.264 decoder's default DMABuf layout): H.264 ×1 57.9 fps (13 dropped), and H.264 + HEVC only 29 fps.
    Measured since (2026-10-02, headless, no bridge): H.264 in that same YU12 layout 79 fps, H.264 + HEVC 70–72 fps,
    so the 29 fps is lost in the bridge, not in the frame layout; NV12 is not needed. The bridge is the next thing to
    fix.
- **Cue → wall bridge (timestamps).** Each cue stays its own pipeline, so trim, seek, pause, rate, loop and warm
  preroll keep working per cue. All pipelines use the same system clock. A frame's running time in its cue pipeline
  maps to the wall pipeline exactly: `wall_rt = cue_rt + (cue_base_time − wall_base_time)`, re-read on every frame
  (pause and resume move the cue's base time). Frames are pushed into the wall's live `appsrc` ahead of time (a
  small bounded queue), and the mixer takes, at each output frame, the newest frame due by then. No timestamping on
  arrival: no jitter, no judder for content at the display rate.
- **Layers.** A cue's layer is a mixer pad. Stacking is pad `zorder` (new cues under outgoing ones, as today, §6.5);
  opacity, fades and crossfades are pad `alpha`, applied **every output frame** (60 steps a second at 60 Hz, finer
  than the plane wall's 30) at no cost to the video; geometry is `xpos/ypos/width/height`. A layer that stops showing
  frames (pause, hold) keeps its last frame on screen. Black is the mixer background.
- **Rotation, mirror, fit, crop.** Rotation and mirror by `glvideoflip` in the cue's GPU chain; fit modes by pad size
  and position. Crop is applied as the frame's source rectangle in the GPU chain (to be verified: video crop meta
  through `glupload`, else a GPU crop step) — never a CPU copy.
- **Panic.** The holding image is a permanent mixer pad at the top `zorder`, alpha 0. A panic sets its alpha to 1: it
  shows on the next output frame (at most one refresh plus the sink's flip). The target stays a mean under 50 ms
  (§12.9).
- **Test patterns** become a source on their own pad, generated at the display's size and rate, as today.
- **Quality notes.** The mixer composites in 8-bit RGBA at the output size. Unscaled layers are pixel-exact. Scaled
  layers use the GPU's bilinear filter, softer than the display controller's polyphase scaler, so full-screen,
  unscaled output is the reference case. Colour conversion follows each stream's colorimetry (`glcolorconvert`).
- **Audio** stays per cue on its own sink for now (simultaneous cues, §6.1.2, add a mixer). The wall adds a fixed
  latency (about one output frame plus the page flip); audio is delayed by the same amount so lips stay in sync.
- **Limits that remain.** The H.264 decoder manages about 70 fps of 1080p in total (two 1080p60 H.264 layers cannot
  both be full rate); HEVC decodes about 90 + 90 fps; software codecs run at CPU speed. Import warns when a file is
  expected to play below full rate (§5.7).
- **Pi 5.** Same design: the Pi 5 has HEVC hardware decode (no H.264), a faster V3D GPU, and the same GBM/KMS path.

**Build order.** Each step lands only once measured on the Pi (frame rate traced per refresh, as in TEST_REPORT O1),
with the KMS plane wall as the default until the GPU wall covers everything it does:
1. Wall pipeline and display ownership (`CUTEPI_WALL=gl`): black background at the display rate; console stays
   hidden; clean start and shutdown.
2. One cue through the bridge: play, pause, seek, trim, rate, loop, hold, stop; A/V sync offset.
3. Layers: fades, fade-in, crossfade (fade and stop others), opacity, geometry, fit, rotation, mirror, crop.
4. Stills, test patterns, slideshow, warm preroll, panic holding image (armed pad).
5. Measure against the plane wall on the same clips, then make the GPU wall the default.

### 6.1.3 Transparency and animated images

**Requirement.** Three kinds of media must play like any other cue (fades, opacity, geometry, panic, hold, loop):

- **Video with an alpha channel**: ProRes 4444/4444 XQ with alpha, QuickTime Animation (qtrle ARGB), PNG video,
  CineForm RGBA, HAP Alpha, FFV1 and VP9 with alpha (`yuva420p`), in MOV and in MKV wherever the container carries
  the codec.
- **Stills with transparency**: PNG (RGBA and palette with tRNS), TIFF, WebP and GIF (1-bit) transparency.
- **Animated images**: animated GIF, APNG and animated WebP, played at the file's own frame delays, looping as the file
  says (GIF/APNG loop count, 0 = forever) unless the cue's own Loop/Hold says otherwise.

**What transparency means on the wall.** A transparent pixel shows whatever lies beneath it: lower cue layers (for
example a lower-third graphic over a playing video, once simultaneous cues land, §6.1.2; during a crossfade, the
outgoing cue), else the black background. Cue opacity and fades multiply the file's own alpha. The file's alpha is
**straight** (not premultiplied) — that is how every decoder above hands it over — and must be blended as such:
blending straight alpha as premultiplied brightens every semi-transparent edge and gradient.

**Decode.** The decoders keep the alpha plane: `avdec_prores`/`avdec_qtrle`/`avdec_png`/`avdec_cfhd`/`avdec_hap`/
`avdec_ffv1` output RGBA, ARGB, GBRA or `A444_10`-style formats; VP9/VP8 alpha travels as a Matroska side stream
(BlockAdditional) and needs `matroskademux` → `codecalphademux` → `vp9alphadecodebin` (present on the Pi); stills
decode through `pngdec`/`avdec_*`/`webpdec`, GIF through `avdec_gif`. The cue's video chain must not drop alpha: no
conversion to an opaque format on the way.

**KMS plane wall (current).** Per-pixel alpha is a plane feature:

- The converted frame must reach the plane in an alpha format (ARGB8888/ABGR8888 and friends — `kmsSysmemCaps`
  lists them; `videoconvert` keeps alpha when the source has it, and 10/12-bit alpha sources go to 8-bit ARGB).
- The plane's **`pixel blend mode` is `Coverage`** (straight alpha). The kernel default is `Pre-multiplied`, which
  mis-blends straight-alpha frames. `newWallLayer` sets Coverage on every layer when it claims the plane, beside its
  rotation; opaque formats carry no alpha, so it changes nothing for them. The layer's own `alpha` (opacity, fades)
  multiplies the file's alpha as before.
- Layers beneath show through, as the display controller blends planes in `zpos` order; the black primary plane is the
  bottom.
- Costs: alpha formats are 32-bit RGB, so 4:2:0 hardware-decoded video is never affected; alpha sources are software
  decoded and converted, so their frame rate is bound by the CPU as for any software codec (import warns, §5.7).

**GPU compositor wall (§6.1.1).** `glvideomixerelement` blends each pad with straight alpha (source alpha × pad
alpha) over the layers beneath, so alpha frames need only to arrive as RGBA through `glupload → glcolorconvert` with
alpha preserved. Same rules: no opaque conversion in the cue chain, background black.

**Animated images.** An animated image is a cue with a **timeline**, not a still:

- Kind stays *image* in the media pool (thumbnail = first frame), but playback treats it like video: it runs from the
  first frame with its own frame timing, and its media duration is the sum of its frame delays (ffprobe's format
  duration, recorded at import). A pool badge showing "animated" with the frame rate is planned.
- Single-frame vs animated is read from the file header (`media.ImageAnimation`): a second GIF image descriptor,
  an APNG `acTL` chunk before the first `IDAT`, the WebP `VP8X` animation flag, plus the loop count (GIF NETSCAPE2.0,
  APNG `num_plays`, WebP `ANIM`). It costs microseconds, so playback decides at load without a probe or a stored
  flag, and the extension never decides (a `.gif` is often a still; an APNG is often a `.png`).
- The still-image shortcuts (§6.2 infinite hold, brightness re-render of the single frame in `applyBrightness`, the
  armed panic holding image) apply only to **single-frame** images (`gsp.isStillFile`). A fade over an animated image
  runs on the plane's alpha and never restarts or freezes the animation.
- Looping: a **cue** follows its own Loop/loop count, like a video cue, so auto-continue and waits stay predictable;
  an animated image cue holds its last frame when it ends (blank display duration = until stopped; a set duration =
  until its timer). **Direct playback** from the pool (no cue) repeats as the file says (`gsp.DirectOpts`: loop count
  0 = forever) and then holds the last frame, as a browser shows it.
- Frame timing: GIF delays are in 1/100 s, so 25 fps (4/100) and 50 fps (2/100) are exact, and 60 fps cannot be
  stored. On a 60 Hz wall every frame is shown at its due refresh (a 25 fps animation repeats frames in a 2-3 cadence,
  as any 25 fps video does). Browsers treat GIF delays of 0 or 1/100 s as 10/100 s; whether `avdec_gif` does the
  same is still to be checked.
- Decoders: `avdec_gif` (animated GIF, palette transparency → RGBA). APNG and animated WebP need decoders that this
  GStreamer may lack (no `avdec_apng`; `webpdec` decodes stills only): import checks them like any file (§2,
  `gsp.CheckDecodable`), and if they are refused the gap is recorded in the codec table as unsupported, not hidden.
  Converting such files at import to a lossless intermediate is a fallback to decide with the user.

**Measured on the KMS wall (2026-10-01, TEST_REPORT "Codec support: transparency and animated images").** Alpha
reaches the planes for every alpha file that imports (AB24/AR24, VP9 alpha included). The first run found every plane
blending it as premultiplied; since 2026-10-02 every layer blends as Coverage (TEST_REPORT "Codec support round 2").
A 25 fps animated GIF plays every frame, through the fades as well, now as a timeline rather than a still. A 50 fps
1080p GIF is decode-bound (`avdec_gif` about 37 fps). APNG and animated WebP are refused at import.

**Tests.** The codec support set includes every alpha codec in MOV and MKV, transparent stills and animated images
(§8, `tools/codec-corpus/make-support.sh`), and the support test checks transparency and animation on the real
display: the plane's pixel format must carry alpha and its blend mode must be straight (Coverage), and an animated
image must present every one of its frames at its own rate through the fades.

### 6.1.2 Simultaneous cues (planned, after §6.1.1)

Several cues playing at once, each on its own layer, with one audio mixer (`audiomixer`) feeding the HDMI device;
the interface and remote protocols show every running cue. What "stop others" means per cue is still to be decided.

### 6.2 Trim, Hold, Loop, Volume, Seek

- **Trim**: on load, seek to `posStart`; reaching `posEnd` = EOS. No trim if both 0.
- **Hold**: on EOS of a held video cue, seek to final frame and pause (frame stays until Stop/Panic); an image cue with blank duration holds indefinitely (infinite hold) — a slideshow group's per-image duration overrides this for its members.
- **Loop**: on EOS (natural or trim-Out), seek back to the in-point and continue. `loop_count` 0 = infinite, N = N plays; loop wins over auto-continue.
- **Volume**: per-cue dB (−60..+12, default 0 = 0dB), converted
  `10^(dB/20)` for the GStreamer volume element; live-set via `SetVolume`.
- **Seek**: absolute, clamped to clip duration and trim Out. Live Seek disabled during Show Mode

### 6.3 Auto-continue & waits

Per-cue `autoContinue`; `preWait` pauses before every start and `postWait`
after every end; advances down the sheet. A generation counter guards the
chain — any operator playback during the wait disarms it (even replaying the *same* cue; a cuePos-equality guard could not detect that).

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
- Slideshow (`POST /api/group/:id/play`): one runner at a time — starting a slideshow cancels any earlier run.
  - **The group owns the timing**: every image holds for the group's hold time (`duration_ms`, default 5 s); the member
    cues' own durations and hold settings do not apply inside a slideshow. Video slides play their own (trimmed,
    rate-corrected) length.
  - **Crossfades**: the next slide starts on a layer under the current one and the current one fades out over it for the
    group's fade time — no fade to black, no gap. Slide changes keep a steady cadence (counted from each load).
  - **Shuffle** reshuffles on every loop pass and never shows the same slide twice in a row. **Loop** off: the last slide
    stays on screen until the operator acts.
  - Any other playback decision (GO, Stop, Panic, ESC) ends the slideshow.
- **Slideshow soundtrack**: audio cues inside a slideshow group do not slide — they become the background-music playlist (`gsp.BackgroundPlaylist`, its
  own audio pipeline, shuffled when the group shuffles) played underneath the slides for the whole run. The soundtrack exists only inside the slideshow run: any main-pipeline decision — Stop, Panic, a new
  load — kills the soundtrack with it.

### 6.5 Crossfade: fade and stop others

A cue with a **fade-stop others** time (`fadeOut` > 0, right-click menu and Time tab) starts **at once** when fired — by
GO, the cue's play button, auto-continue, remote protocols or the scheduler — on a display layer **under** whatever is on
screen. Everything playing above it then fades out (picture and sound) over that time and stops. New cues always start
on a lower layer, so the outgoing picture fades away to reveal the new one. The fade starts when the new cue is actually
on screen, never while it is still loading, so the old picture never fades over black.
- **Dissolve**: give the new cue a Fade In as well and it fades up while the old one fades out.
- Without display layers (fbdev fallback) the old cue fades out first and the new one starts after it.

### 6.6 Cue trigger

Space (not in an editable field) → plays the selected cue; on a group selection this triggers the group action.

### 6.7 Synchronization & inspector auto-follow

WebSocket hub (`/api/ws`) is the **only** channel: while playing, the pipeline ticker pushes one sync per displayed second over the socket.
No polling anywhere — every widget (nowplaying, cuesheet, clocks) renders
from socket pushes. The topbar shows the socket state as a status dot.

### 6.8b Scheduled fire (wall-clock)

Per-cue recurring trigger: `schedule_enabled` + `schedule_days` bitmask (bit0=Mon) + `schedule_time_ms` (ms since midnight, set to whole seconds).
Second precision end to end (inspector `step=1` time input, `HH:MM[:SS]` API): minute-rounded times can never hit an exact-second sync-fire.
The scheduler ticks every 200ms and fires cues due within the last 1s (one missed tick + jitter); anything older is stale and never fires, so
enabling Show mode late never replays the day's past cues. Each cue fires once per day (tracked by `cue_id`, so reordering the sheet
mid-day neither re-fires nor blocks a cue); schedules arm only in Show mode. An armed fire re-checks at its second that Show mode is still
on and the cue's schedule is unchanged, and records the cue's result and playing position on both the warm and cold paths.
Multi-node sync-fire (several Pis firing the same second) assumes NTP-synced clocks and identical shows — each node fires on its own clock
crossing. Decision-accurate, not output-accurate: pipeline build takes ~100s of ms, so frame-exact joint output needs timed pre-roll (v2).

### 6.8 Identity guards (three places, one invariant)

The invariant *"start only if the generation hasn't moved"* is implemented in `slideshowRunner`, the auto-continue chain, and the queued fade-then-play
goroutine — all comparing `gsp.Generation()`, which bumps on every load. Generation (not cuePos) is the guard because replaying the *same* cue still
moves the generation. Keep them consistent.

### 6.9 ESC / Panic

- **Single ESC** fades video and audio out together — to black and silence —
  over the configured fade time (default 1000 ms, adjustable in Settings
  General), then stops.
- **Double ESC** (second press within ~1 s) cuts everything immediately:
  video and audio stop at once, the screen goes black, no sound plays.
  With a panic holding image configured (§12.9) the wall shows that image
  instead of black.
- **Menu > Fade out** is the same action as a single ESC (same fade time).
  **Menu > Stop** and **Panic** are immediate cuts.
- **Black means black.** On the KMS wall a stopped cue's plane is removed, revealing the black primary plane; nothing
  else (console text, cursor) is ever drawn there while the service runs. On the fbdev wall sink the last frame would
  otherwise stay in the framebuffer after the pipeline is torn down, so Stop,
  Panic (without a holding image), the end of ESC/Fade out, and a clip that
  ends naturally without a follow-up (nothing new loaded within ~150 ms)
  all clear the framebuffer. A held clip (§6.2) and the holding image are
  deliberate exceptions and keep their frame.

## 7. Error handling & logging

- Failures at trust boundaries reject cleanly with conventional statuses:
  undecodable imports 422 at import rather than cue time; absent pool items
  404; delete-while-playing 409; firing a cue whose source is missing 409;
  Test/pattern calls in Show mode 403; unknown test pattern 400; settings
  validation 400/422.
- Optional auth: `AuthMiddleware` (config `auth_password`, editable in Settings) applies to all routes including static assets; browser basic-auth
  prompt; 401 wrong password; 200 once accepted. `GET /api/settings` reports `authEnabled` but never the password.
- **Operator password is stored in plain text.** `auth_password` is kept
  unhashed in `config.json` (HTTP Basic needs nothing more, and the operator
  may need to read it back off the SD card). The file is created mode `0600`,
  but anyone with read access to the data directory — shell access as the
  service user or root, or physical access to the SD card — can read it.
  Basic auth also sends it on every request in cleartext over plain HTTP, so
  it guards against casual access on the show LAN, not a hostile network.
  Don't reuse a valuable password here. The Wi-Fi hotspot password is
  stored the same way, and is also visible in the process list while
  `nmcli` runs.
- **Cross-site guard** (`SameOrigin`, before auth): state-changing requests
  (anything but GET/HEAD/OPTIONS) whose `Origin`/`Referer` names another
  host get 403 — browsers attach cached Basic credentials to cross-site
  form posts, so the password alone does not stop CSRF. Requests with
  neither header (curl, Companion) pass. The `Host` header must be an IP
  literal, `localhost`, a dotless name, a name under a local-only suffix
  (`.local`, `.lan`, `.home.arpa`, `.internal`), the machine hostname, or be
  listed in config `allowed_hosts` (e.g. a reverse-proxy domain); anything
  else gets 421 (DNS-rebinding guard: public DNS can't serve those names). The WebSocket handshake checks
  `Origin` the same way.
- **Resource bounds**: request bodies cap at 2 GiB; a `.CTP` import is
  refused (507) when its declared media size plus 256 MiB headroom exceeds
  the media volume's free space. Scratch files live in `<working dir>/tmp`
  rather than `/tmp`, which is RAM-backed tmpfs on current Raspberry Pi OS.
  ffprobe/ffmpeg calls have timeouts (probe 30 s, verify/thumbnail 60 s,
  full-file analysis 15 min, waveform window 30 s), as do system tools run
  from requests (20 s).

## 8. Testing & verification

- The Go suite speaks HTTP only — template renders, auth, groups,
  slideshow, audio/display/video settings, warm slot, awards, QLab/remote,
  themes, inspector behaviour, plus real-GStreamer smoke and
  migration/export-import round-trips. It never executes the browser JS
  (`ui.js`/drag-and-drop hover, intent and modifier-click logic), so
  sheet-interaction behaviour is verified manually; no browser harness
  exists (deliberate: the interaction bugs to date all lived in the server
  model, which the suite does cover).
- Codec support (README "Codec support", `tools/codec-corpus/support.py`): every codec and container in the support
  set is played through the live service with a 1 s fade in and out and measured from the kernel (frames latched
  per refresh, opacity writes). The set covers video with alpha (`*_alpha`: ProRes 4444, qtrle, PNG, CineForm, HAP,
  FFV1, VP9), transparent stills (PNG, TIFF, WebP, GIF) and animated images (GIF at 25 and 50 fps, with and without
  transparency; APNG; animated WebP). Alpha files must reach the plane in an alpha format with straight-alpha blending;
  animated images must present every frame at the file's rate.
- Expected build process (until the build pipeline is confirmed): tidy the
  module, build every package, vet, then run the full test suite — and
  repeat that check before closing a session.


### 12.4 Multi-select + bulk edit

- Selection extends from a single id to an **anchor + set** (persisted):
  Shift+arrows/click extend the range, Ctrl/Cmd-click toggles, Ctrl/Cmd+A selects
  all visible units. Arrow navigation keeps the anchor; Space still plays the anchor cue only.
  Shift+arrows step the range head one visible unit (stepping back shrinks; reaching the anchor clears).
  A range covers **visible rows in sheet order** (`SelectUnits`: collapsed
  members excluded, group headers included); headers persist in the set as
  and highlight. Every single-selection write (cue or group) clears the set.
- Row visuals: every selected row shares the anchor's highlight — one
  selection look; anchor identity lives in the GO bar, not a second tint.
- Dragging any selected cue moves the whole selection as one block
  (relative order preserved).

### 12.5 Cue-number arithmetic on insert

- Auto-numbering (setting, default on): new cues numbered 1, 2, 3…;
  an insert *between* numbered cues takes a fractional number (12.5), kept as
  text — no schema change (`cueNum` is TEXT; existing CAST-INTEGER MAX for
  next-number computation still works).
- Appends take the next multiple of the **cue number step** (Settings → General, default 1) above the highest number
  in the sheet (cue or group header).
- **Renumber cues** recomputes the visible sequence as step, 2×step… (see §5.4); **Sort by cue number** reorders it;

### 12.6 Topbar clock

- The current wall clock (locale `HH:MM:SS`) lives in the topbar next to the
  live-status dot, rendered client-side (1 s tick, no server round trip, no
  sync path).

### 12.7 Fade curves (volume envelope automation)

- Per-cue **fade curve** setting selects the envelope shape `f(t)`, applied
  identically to fade-in, fade-out and the slideshow fade (one ramp function,
  three callers) with visual representation next to the drop down.
  - `linear` — constant slope (current behaviour, default)
  - `smooth` — S-curve (smoothstep: slow start, fast middle, slow end) —
    natural-sounding audio fades, gentler light changes
  - `log` — logarithmic (perceptual: fast initial change, long tail) —
    matches how loudness is heard
  - `exp` — exponential (slow start, sharp finish)
- Data: `cuesheet.fade_curve` TEXT default `'linear'`; validated against the
  list above. UI: a curve picker next to the fade time in the inspector.
  Slideshow groups may set a group-level curve used for their inter-slide fades.

### 12.8 Remote control protocols (OSC + HyperDeck)
Reference clients are Bitfocus Companion's **HyperDeck** module
(bmd-hyperdeck 3.1, library hyperdeck-connection 3.1) and **QLab** module
(figure53-qlab-advance 2.14, osc.js). Both must reach status OK, and their
actions, feedbacks and variables must work without changes on the
Companion side.

- **HyperDeck** (TCP, default port 9993; `routes/hyperdeck.go`). CuTePi
  presents itself as a **HyperDeck Studio Mini**, protocol 1.11, in the
  greeting and in `device info`. The module selects that model from the
  model string.
  - *Framing.* Commands are single-line (`play: speed: 100`) or multi-line
    (`notify:` plus one `param: value` line each, then a blank line).
    Replies are `{code} {name}` or `{code} {name}:` plus lines and a blank
    line. Errors use the protocol's codes (100 syntax, 101 unsupported
    parameter, 102 invalid value, 103 unsupported, 105 no disk,
    107 timeline empty, 109 out of range, 111 remote disabled,
    120 connection rejected).
  - *Session.* `watchdog: period: N` closes a client that stays silent past
    N seconds (plus a 2 s grace). The client's `ping` keeps it alive. At
    most one client at a time, like a real deck; the watchdog frees the
    slot of a controller that vanished.
  - *State the module reads at connect.* Every one of these must answer, or
    the module drops the connection:
    - `notify` (209, or set: 200);
    - `device info` (204, with `slot count: 2`);
    - `slot info` (202: slot 1 mounted with the clip list, slot 2 empty);
    - `transport info` (208);
    - `configuration` (211: SDI, embedded audio, H.264; settable, kept in
      memory);
    - `remote` (210: enabled). `remote: enable: false` makes transport
      commands answer 111, as on a deck.
  - *Clips.* The cue sheet in play order is the clip list (or the media
    pool, per Settings). Clip ids are 1…N in that order, as on a deck's
    timeline: the module sorts clips by id. `clips get` answers versions 1
    and 2; `clips count` answers 214. `disk list` gives the files.
  - *Transport.*
    - `play [clip id|timecode|speed|loop]`, `stop`, `jog` and `shuttle`.
    - `goto` by clip id, `clip: start|end|±n`, `timeline: start|end|n|±n`
      or `timecode` (absolute or `±`). `goto` moves the playhead (the
      selected cue) without starting it; the next bare `play` starts that
      clip.
    - A bare `play` with nothing loaded plays from the playhead, or from
      the first clip when nothing is selected.
    - Speed is a percentage of the cue's own programmed rate (100 = as
      designed): controllers send `speed: 100` with every play. A negative
      speed answers 103, because there is no reverse playback.
    - `loop: true` loops the running clip only; `loop: false` doesn't
      override a cue that loops by design.
    - `stop` stops the cue (the show-controller meaning). A paused cue, or
      a clip held on its last frame (a still), reports `status: stopped`
      with its clip id, like a stopped deck holding a frame.
  - *Notifications* go to clients that subscribed with `notify:`:
    - 508 transport (debounced one 100 ms tick, so a half-loaded state is
      never sent);
    - 502 slot, when the clip list changes, which makes the module re-read
      the clips;
    - 510 remote, 511 configuration and 513 display timecode.
    - A command's reply always precedes the notifications it triggers.
  - *Playback-only.* `record`, `format` and `clips add/remove/clear` answer
    103. `playrange set` answers 103, and `playrange` reports none.
- **QLab** (OSC; `routes/qlab.go` and `routes/qlabws.go`). CuTePi presents
  itself as **QLab 5** (`/version` → 5.4.0), with one workspace (named
  after the host) holding one cue list, the cue sheet.
  - *Transports.* TCP 53000, SLIP-framed (END before and after each
    packet), answers every message with `/reply{address}` and QLab's JSON
    envelope `{workspace_id, address, status, data}`. Unknown addresses
    answer `status: error`, as QLab does. UDP 53000 runs the same
    dictionary without replies (QLab's UDP mode is fire-and-forget).
  - *Handshake.* `/connect` grants `ok:view|edit|control` (there is no
    passcode; see the trust boundary). `/workspaces`, `/updates 1`,
    `/cueLists`, `/selectedCues`, `/playheadID`, the audition, override,
    show-mode and min-GO queries, and `/overrides/*` all answer, so the
    module reaches OK.
  - *Cues.* Every sheet row is a cue: unique ID from its database id,
    number from its cue number (else its sheet position), type Video or
    Audio. The palette colour maps to QLab's colour names.
    `valuesForKeys` returns the full dictionary the module requests:
    running/paused, duration, elapsed and percent through the trimmed
    span, pre/post wait, continue mode, loop, hold, broken. A clip held on
    its last frame is running; only an operator pause is `isPaused`. Cue
    lists never carry `isPaused`: the module (2.14) throws on a paused cue
    it hasn't stored yet, and its follow-up `/cue/active/valuesForKeys`
    carries the pause.
  - *Addresses.*
    - Rootless or `/workspace/{id}`-scoped.
    - Transport: `/go`, `/stop`, `/pause`, `/resume`, `/panic`,
      `/panicInTime` (a fade-out), `/reset`, `/togglePause`.
    - Playhead: `/playhead/next|previous[Sequence]`,
      `/playhead/{number}`, `/playheadID/{id}`.
    - Per cue: `/cue/{number|selected|playhead|active}/…` and
      `/cue_id/{id}/…` with `start`, `go` (playhead there, then GO),
      `stop`, `panic`, `panicInTime`, `pause`, `resume`, `togglePause`,
      `select`, and property reads. `load`, `preview` and `audition` are
      accepted and do nothing.
    - Cue-list commands on `/cue_id/{list}/…`.
  - *Updates.* After `/updates 1`, a client gets
    `/update/workspace/{id}/cue_id/{cue}` when a cue's state (the one that
    was, and the one now on the board) or its shown content changes. It
    gets `…/cueList/{list}/playbackPosition {cue}` when the playhead
    moves, and the list's own update when cues are added, removed or
    reordered.
- **Trust boundary**: neither protocol authenticates (protocol limitation),
  and the operator password does **not** cover them: anyone who can reach
  an enabled port can drive playback. They are **off by default** and
  documented as control-room-LAN features. The bind address applies to all
  three listeners, so they can be pinned to one interface. DoS bounds:
  HyperDeck lines cap at 4 KiB and a multi-line command at 32 parameter
  lines, SLIP packets cap at 64 KiB, and control writes time out after 2 s.
  Each HyperDeck client has a 64-frame notification queue: frames beyond it
  are dropped, and a client that stops reading is cut by the write
  timeout.
- **Configuration lives in the Settings modal Network tab**: one
  enable/disable toggle per remote item (HyperDeck, OSC UDP, OSC TCP/SLIP)
  + port per protocol (and one bind address for all listeners), persisted in `config.json`;
  the tab lists every server with its live on/off state. The HyperDeck clip
  listing source is selectable: Cuesheet (default — cue rows in play order)
  or MediaPool.

### 12.9 Panic holding image

- Setting: **"Panic cuts to a holding image"** + holding image picker (from
  the media pool). When on, Panic does not go to black — it immediately
  loads and holds the configured image (full-frame), so screens never show dead black mid-show.
- Fallback: holding image missing/unplayable → plain panic to black, logged
  as an error. The setting lives with the other panic/transport settings.
- **Target: the holding image is on screen within 50 ms of the panic, on
  average.** On the KMS wall the image is kept **armed** (`gsp/panichold.go`):
  built, decoded and presented on its own display plane at alpha 0, parked at
  the top zpos (17) outside the visible stack. A panic mutes the running audio,
  sets that plane's alpha to full (one display commit, landing on the next
  refresh), then retires everything else underneath and adopts the armed
  pipeline as the transport, the same state a cold load leaves. The armed copy
  is then rebuilt for the next panic (about 230 ms).
- Kept in step by a reconciler (`routes/panichold.go`): it re-checks the setting
  every 2 s and at once after the setting changes or a panic. If the file
  changed since it was armed (size or modification time), or the armed pipeline
  isn't ready, the panic takes the cold load path (~330 ms) and re-arms. Stills
  only; the fbdev wall always loads cold. The armed image holds one display
  plane while idle.
- Measured on the Pi 4 (1080p60), 20 runs polling the armed plane: picture up
  a mean 26 ms after the request (median 21, max 44; about 9 ms inside the
  service); audio stream closed 20–49 ms. The cold path took 326–359 ms.

### 12.10 Test patterns (built-in + custom)

- **GStreamer built-ins**: the Tests menu (topbar) lists a curated set of
  `videotestsrc` patterns — SMPTE, SMPTE100, Snow, Black, White, Red, Green,
  Blue, Checkers-1..4, Circle, Blink, Solid, Barcode — played fullscreen via
  the existing `ShowTest` path (no cue created; ESC/Stop ends). Test
  patterns are **not available in Show mode** (the Tests entry is locked;
  the API refuses with 403).
- **Full screen at the display's own mode**: patterns are generated at the display resolution and fill it edge to
  edge. Generation rate: static patterns a few frames a second (the display scans the layer out at its own refresh
  anyway), Snow 30 fps (generated at a third of the resolution and scaled up by the display), **Blink at the display
  refresh rate** — it alternates every refresh, the frame-rate check.
- **Resolution & frame-rate label** (checkbox in the Tests picker, remembered): prints `1920 × 1080 @ 60 Hz` (the
  display's current mode) on the pattern. Changing it while a pattern is showing re-shows it at once.
- **Tests button state**: off = a quiet grey "Tests"; on = red, pulsing **TEST ON** on every connected client. The
  picker highlights the pattern currently on the output.
- **Custom patterns**: the operator can flag any media-pool item as a test
  pattern (media context menu → *Add to test patterns*), which pins it into
  the same Tests menu; selecting one Loads it directly (images hold their
  frame). Patterns persist in `state`; clearing unlists them without
  deleting media.

- **Output preview thumbnail** — mirror the Pi's HDMI output in the topbar
  (click to expand): pipeline `tee → appsink` → MJPEG endpoint / WS frames.
  The one real pipeline change on the list; costs decode headroom on the Pi.
  Add this as a new browser pop-out window.

- **active/active machine sync** — a second Pi mirrors the show over the
  network: every playback decision is replayed to the standby over WS so it
  sits one click behind the same state; mid-show failover is a single
  operator action. Requires a command-replay protocol, media mirroring
  strategy and conflict rules — deliberately out of the v1 scope.
  The sync should be driven by the client. So that if one machine drops, the front-end client is still connected to the second machine.
  Fired cues, fire on both servers, the servers negotiate with each other and keep mediapool in sync.
  Notify the operator if any media isn't in sync, only start a media sync from a user action.
- For upload/yt-dlp operations, one server downloads then syncs to the second.
- There needs to be a graceful client handoff if the main goes down.
- Server A must be able to see Server B, Client A must be able to see both Server A and B
- Client A, started via Server A's http://cutepi.local address, then discovered Server B and allowed the operator to link/sync the two together. mDNS must be handled correctly.
- A joining Client B should instantly be able to follow or lead current actions.
- If Server A disconnects, Client A must still function correctly and talk to Server B. A error warning must be displayed but UI functionality must not be interupted.
  

### 12.11 A/V sync test signal (later phase)

A test signal for checking audio/video sync end to end: a full-screen flash on the wall exactly when a short tone plays
on the audio output (e.g. once a second), with an optional on-screen sweep/counter, so the delay between picture and
sound can be measured at the venue (camera + microphone, or a sync meter). Planned for a later phase; not implemented.

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
