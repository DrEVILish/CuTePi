# TODO — status board

Compact status of CuTePi. History lives in git log (CHANGELOG.md was
removed 2026-09-17); the design/UX spec lives in DESIGN.md. Everything below
the "remaining" lines is **[done]** — built, tested, covered.

## Status by area

| Area | Status |
|------|--------|
| Media pool (grid, thumbnails, filters, menu, DnD upload, /upload page, yt-dlp) | done |
| Cuesheet (6-column spec, inline edit, reorder, column resizing, context menu) | done |
| QLab-style UI pass (row visuals, context menu, group parity, tabs) | done |
| Cue Inspector (Time/Audio/Media/Colour tabs, trim timeline, silent save, auto-follow) | done |
| Groups + slideshow (collapsible, nestable, playlists, slideshow runner, identity guards) | done |
| Transport (play/pause/stop/panic/fade, Show Test, Space trigger) | done |
| Playback (trim, hold, loop + loop_count, per-cue dB volume, seek, auto-continue+waits) | done |
| Sync (WebSocket hub, change-detection poll fallback, persisted selection) | done |
| .CTP export/import (ZIP manifest + audit trail + media; append/overwrite; **groups + nesting + slideshow settings, manifest v2**) | done |
| Nestable groups (render/keyboard/collapse via shared FlattenSheet, subtree moves, cycle-safe parenting) | done |
| Sheet model v2 (stored membership, single drop model, startup heal) | done |
| Slideshow soundtrack (audio cues in a slideshow group become a background-music playlist) | done |
| Logs + audit trail (web viewer, recording level, clear) | done |
| Settings (port, poll interval, theme incl. custom tokens, default loop, operator password / auth) | done |
| Restart / Shutdown, QR upload link, systemd install | done |
| Media tooling (ffprobe metadata, thumbnail/waveform worker, missing-source scan) | done |
| Testing (unit, route-level, gsp runtime smoke, smoke-test.sh, migration + export tests) | done |

## Remaining

- **Hardware validation on the Pi** — run `./smoke-test.sh` on the real
  Raspberry Pi 4/5 (KMS/DRM HDMI output, HDMI-exclusive audio, decode
  fallback path). The one blockable item before fielding.
- **Undo/redo for sheet edits** — local undo stack for destructive actions
  (delete, reorder, import-overwrite).
- **Remote control** — OSC + HyperDeck protocol servers (DESIGN.md §12.8);
  a read-only phone status/run page remains an open idea beyond that.
- **Doc gaps** — no API reference, no screenshots, no hardware bring-up
  checklist.
- **Remote control** — OSC + HyperDeck protocol servers (DESIGN.md §12.8);
  a read-only phone status/run page remains an open idea beyond that.

Built since the 2026-09-14 spec (DESIGN.md §12) and now covered above:
GO bar with next-cue preview, running wait countdowns, cue health + F8,
multi-select + transactional bulk edit, cue-number arithmetic + Renumber ×5,
topbar wall clock, fade curves, panic holding image, test patterns.

Slated for v2:
- master/standby machine sync.
- output preview thumbnail (HDMI mirror), scrollbar minimap.
