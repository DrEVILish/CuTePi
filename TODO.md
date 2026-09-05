# TODO

Shared tracker for the CuTePi project. Both of us add/edit items. Format:
`- [ ] **Title** — description (owner: `you`/`me`/`us`, priority)`. Mark `[x]` when done.

## Current focus
The UI shell, MediaPool, CueList, multi-client sync, themes, transport
controls (scrub/loop/volume), trim timeline, and responsive upload flow are
implemented and tested. Record new decisions/changes in DESIGN.md.

## Decided 2026-09-04 — features now in scope (from the operator Q&A)

Product-level answers (hardware, trigger, defaults, auto-continue, exports,
logs, settings rule) are recorded in DESIGN.md → **Product Decisions**. These
are the build items those decisions imply:

- [ ] **Cue trigger (Space)** — pressing Space while the control UI is focused,
      not in an editable field, plays the currently selected cue. Media Pool
      tiles are not selectable; the sheet behaves as if a cue is always
      selected. (owner: `us`, priority: **high**)
- [ ] **Auto-continue + waits** — per-cue `autoContinue` flag; `preWait`/
      `postWait` drive a pause before-start / after-end only for
      auto-continuing cues. Advances in sheet order downward; a cue-group row
      triggers the group action and the chain continues. Non-auto cues always
      stop on EOS (cues are a list, never a playlist). (owner: `us`, priority:
      **high**)
- [ ] **Loop counter** — `loop_count` column (0 = infinite, N = play N times);
      loop always wins over auto-continue: a finite loop count is exhausted
      before an auto-continuing cue advances. (owner: `us`, priority: **high**)
- [ ] **Default `loop=off`, `hold=off`** — new cues default both off (`loop_count`
      0); the config `loop` default for loaded clips also off. (owner: `us`,
      priority: **high**)
- [ ] **Import-time playability probe** — reject undecodable/corrupt files at
      import instead of failing at cue time; undecodable cue start = immediate
      error to the user. (owner: `us`, priority: **high**)
- [ ] **Cue Groups** — visual folders grouping cues, nestable; rendered as
      collapsible rows with indentation, drag-reorder across folder boundaries;
      global order stays flat `cuePos` (folder membership is a presentation
      layer); a group can act as a playlist. New `cue_group` table (incl.
      `parent_group_id`). (moved from Deferred, owner: `us`, priority: **high**)
- [ ] **Slideshow** — a cue group's images are visible cue rows inside the
      group; the group inspector toggles slideshow mode (shuffle, loop, fade,
      duration-per-image stored on `cue_group`); an indicator marks the image
      currently displayed; new folder icon + distinct icon for slideshow
      groups in the type column. (moved from Deferred, owner: `us`, priority:
      medium)
- [ ] **Missing-source detection** — one scan at startup; Media Pool entries
      with a missing source file get a warning-triangle icon; cues referencing
      a missing file show a warning offering delete-the-cue or choose-a-
      replacement-file (re-link). No periodic scan. (owner: `us`, priority:
      medium)
- [ ] **.CTP export + import** — ZIP of a JSON manifest (all cue info incl.
      audit trail) + referenced Media Pool content; import restores a show with
      a modal choice of append-to-end or overwrite. (owner: `us`, priority:
      medium)
- [ ] **Web UI log viewer** — view logs (debug/info/warn) with clear; the level
      selector switches the *recording* level (runtime toggle), and a
      structured playback audit trail (cue started/stopped, wall-clock) feeds
      the manifest. (owner: `us`, priority: medium)

## Deferred (large schema-level features, queued for a follow-up pass)

- [ ] **Multi-image slideshow** — moved to active (2026-09-04), see above.
      (owner: `us`, priority: low)

## Build / Setup (blockers first)

- [x] **go-gst build break** — DECIDED: bump `go-gst`/`go-glib` to v1.4.0 (user
  approved). `go build ./...`, `go vet ./...`, and `go test ./...` now pass.
  (owner: `me`, priority: **high**)

- [x] **DESIGN.md** — documents architecture, decisions, and the active build
  direction. (owner: `me`, priority: high)

- [x] **Standalone /upload page** — full mobile-friendly page
  (`templates/upload.html`) with drag-and-drop + picker, a plain (non-htmx)
  multipart form, and a redirect back to `/` on success. `GET /upload/`
  renders it. (owner: `me`, priority: high)

- [x] **`/` desktop + link to /upload** — desktop topbar now has a visible
  link to `/upload` alongside the Upload modal; error.html renders the actual
  error message. (owner: `me`, priority: medium)

## MediaPool

- [x] **2-column thumbnail gallery, sticky scrollbars** — fixed 2-column CSS
  grid (`.media-grid`, always two columns), pane scrolls with
  `scrollbar-gutter: stable`. (owner: `me`, priority: medium)

- [x] **Filtering** — filename text filter + type dropdown
  (video/image/audio/all, default All), client-side on `data-media-type` /
  `data-media-name`. (owner: `me`, priority: medium)

- [x] **Item controls layout** — kept the 3-dot dropdown. DELETE now opens an
  "Are you sure you want to delete [filename]?" confirm modal (DELETE / KEEP);
  added the thumbnail-refresh entry. (owner: `me`, priority: medium)

- [x] **Thumbnail placeholder SVGs** — while a thumbnail is pending/regenerating,
  show a per-type SVG (video/audio/image/other) instead of a broken image. SVGs
  created under `public/img/`; `mediapoolView` serves an empty thumbnail while
  pending and the template falls back to `/img/placeholder-{type}.svg`. (owner:
  `me`, priority: medium)

- [x] **Show "date added"** — tile now shows date added (DD--MM used
  `date_added`); pool already sorts newest-first. (owner: `us`, priority: low)

- [x] **Media pool panel chrome** — the panel has a defining right border; it
  can be collapsed to a sliver (header chevron, restore via the floating
  expand button) or resized by dragging the splitter; width/collapsed state
  persist in localStorage; an empty pool shows an "Upload media here"
  placeholder with an Upload button and drag-and-drop help text. (owner:
  `me`, priority: medium)

- [x] **Cross-browser media refresh** — media registration, deletion, and
  thumbnail changes send a targeted WebSocket refresh event. (owner: `us`,
  priority: medium)

## CueList

- [x] **Column spec mismatch** — matched the 6-column spec; Actions has play,
  move up, move down, delete. Internal cue order is not displayed. (owner: `me`,
  priority: **high**)

- [x] **DB schema for waits/duration** — added `preWait`, `cueDuration`, `postWait` columns with migration, time parser (hh:mm:ss.ms, bare number = seconds), FormatTime, tests. (owner: `me`, priority: **high**)

- [x] **Move up/down** — MoveCueUp/MoveCueDown functions + routes + tests passing. (owner: `me`, priority: **high**)

- [x] **Cue play endpoint** — `/api/cue/:cuePos/play` loads media and plays via gsp. (owner: `me`, priority: **high**)

- [x] **MediaPool/CueList render fixes** — DELETE cue/media now re-render their partials (empty 200 would wipe the panel); added `GET /mediapool` and `POST /api/load/:filename` routes. Regression tests added. See CHANGELOG 2026-08-31.

- [x] **Column widths adjustable** — spec: user-adjustable column widths (width
  only, not order). Drag a header's right edge; widths persist in localStorage
  and survive htmx re-renders. (owner: `me`, priority: low)

- [x] **Steps to reproduce handover** — Cue No remains the user-defined
  alphanumeric label (TEXT UNIQUE in the DB, editable by double-click); the
  reorderable integer row order remains internal.

## TopBar / Controls

- [x] **Branding and themes** — added the 80s corporate-style CuTePi logo plus
  browser-local LCARS, QLab, and Future SciFi themes (Future SciFi is default).

- [x] **Show Test modal** — the Test button already calls gsp ShowTest(pattern)
  and plays the pattern, but the spec wants a modal: "test is loaded" with a
  "Hide Test" button (currently hidden by stopping/clearing). (owner: `us`,
  priority: low)

- [x] **Settings modal** — new `templates/settingsModal.html` wired to the
  TopBar Settings dropdown item (`data-settings-open`); fetches
  `GET /api/settings` on open, Save posts to `POST /api/settings` and shows
  the restart-required message. NOTE: spec's "editing restricted to
  first-connecting client" is NOT enforced — no client/session id mechanism
   exists anywhere in the codebase, so it's documented (see DESIGN.md) rather
  than half-implemented. (owner: `us`, priority: medium)

- [x] **Restart / Shutdown controls** — the Settings dropdown also holds
  Restart and Shutdown items (each behind a native `hx-confirm` dialog),
  backed by `POST /api/restart` (self re-exec that waits for the port to
  free, env `CUTEPI_RESTART_WAIT`) and `POST /api/shutdown` (SIGTERM → the
  existing graceful DB-close handler). Both respond 200 before acting.
  ponytail: self-managed; under systemd use `systemctl restart cutepi`.
  (owner: `me`, priority: medium)

## Playback (gsp)

- [x] **Verify Play/Pause/Stop/Panic runtime** — `gsp` now has a
  self-contained runtime smoke test (`gsp/runtime_scratch_test.go` →
  `TestPlaybackRuntime`) that generates audio+video assets via
  `gst-launch-1.0` and exercises Load, position/duration tracking, pause/
  resume (TogglePause), Stop, and Panic through real GStreamer. Passes
  headless in this environment (it resolves to headless sinks); skips
  cleanly when gst-launch-1.0 is absent. (owner: `me`, priority: high)

## Polling / State

- [x] **Selection sync** — cue selection writes advance the server sync version;
  arrow-key changes propagate through WebSocket/polling to other browsers.
  (owner: `us`, priority: high)

- [x] **Separate polling endpoints with change detection** — spec wants separate
  endpoints returning only changed state or a "not changed" marker. Currently
  only `/api/nowplaying` is polled (500ms) and it always re-renders. DECIDED:
  added `GET /api/nowplaying/status?version=N` (JSON `{changed, version}`)
  backed by a monotonic change counter in `gsp` (bumped on pipeline swap,
  play/pause/stop/panic, and position ticks that change the displayed clock);
  `mediainfo.html` no longer htmx-polls itself, `public/src/ui.js` now polls the
  status endpoint every 500ms and re-renders via `/api/nowplaying` only when a
  change is reported. Multi-client safe (counter is server-side; each client
  tracks its own last-seen version). (owner: `us`, priority: low)

## Tooling / Quality

- [x] **Error codes for logging** — added a `logs` package with stable
  `<SUBSYS>-E<NNN>` codes (`GSP`/`RTE`/`NET`) + `logs.Printf` helper; replaced
  bare `fmt.Println`/`log.Println` in `gsp/gsp.go`, `routes/api.go`,
  `network.go`. Code table in DESIGN.md. (owner: `me`, priority: low)

- [x] **Unit tests** — config/ctp/routes/worker/media have tests. Added time-parse/format, move-up/down, and the 2026-08-31 route render regression tests. (owner: `me`, priority: medium)

- [x] **README** — refreshed as a concise overview + quick start + tests +
  current Features doc (2026-08-31); detailed docs pointed to via PROJECT/DESIGN/
  CHANGELOG/TODO. Keep updated with requested features. (owner: `us`, priority: low)

## Next build (implemented 2026-08-31 — see DESIGN.md for details)

- [x] **Cue Inspector panel** — new collapsible panel docked at the bottom of
  the screen; shows/edits the selected cue's details (name, media, trim In/Out,
  Hold toggle, waits). (owner: `us`, priority: **high**)

- [x] **Cue trim (start/end)** — edit `posStart`/`posEnd` (columns already in
  the DB) from the Cue Inspector; on play gsp seeks to `In` and auto-stops at
  `Out` (`0/0` = whole clip, no trim). (owner: `us`, priority: **high**)

- [x] **Per-cue Hold (last frame)** — new `cuesheet.hold` column (default 1,
  migrated like the other cue columns); after EOS of a held video cue, gsp
  keeps the final frame on screen until Stop/Panic. (owner: `us`, priority:
  medium)

- [x] **Drag-and-drop cue reorder** — drag rows between rows/to end with the
  drop-line indicator; new `PUT /api/cue/reorder` (full ordered list of cuePos,
  reindexed 1..N transactionally). (owner: `me`, priority: **high**)

- [x] **QR-code button** — TopBar button opens a modal with a QR encoding the
  host-derived `/upload` URL (replaces the original "gate `/` on mobile"
  spec; no UA sniffing). (owner: `us`, priority: medium)

- [x] **Multi-browser sync over WebSockets** — new `ws` hub;
  **server-authoritative**: the server broadcasts playback/cuesheet/selection
  state and clients are mirrors only. Current change-detection polling
  (`/api/nowplaying/status`) becomes the reconnect/fallback path. (owner:
  `us`, priority: **high**)

- [x] **Cue row selection persisted** — single-select rows, arrow-key
  navigable, "the cue that can play next"; stored in the DB (survives reload,
  shared across clients). (owner: `us`, priority: medium)

- [x] **Dead-code cleanup (approved)** — delete `templates/dropzone.html`;
  remove the `mediapool-updated` listener/trigger in `mediapool.html` +
  `public/src/ui.js` (re-bind filter re-apply to `htmx:afterSwap`); remove the
  empty `gsp.Next()`/`Prev()` and the Prev/Next toolbar buttons plus the
  now-unused `POST /api/next`|`/api/prev` routes (RTE-E207/E208, gated on an
  audit of other usages). (owner: `me`, priority: low)

## Earlier (htmx v4)

- [x] **HTMX v4 upgrade** — vendored `htmx.org@4.0.0`, updated footer.html,
  added `htmx-config` compat meta (implicitInheritance + noSwap). (done)

- [x] **HTMX v4: drop compat flags, go explicit** — migrated the `htmx-config`
  compat meta out: inheritance is now explicit
  (`hx-target:inherited` on the cuesheet `<tr>`), no-content/status-only
  actions are `hx-swap="none"`, and 4xx/5xx swaps are suppressed via a
  `htmx:before:swap` handler in `ui.js`; see CHANGELOG 2026-08-31. (done)
