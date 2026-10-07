# Feature request: zoom for `.waveform` (windowed view of a long clip)

*For DrEVILish/ftl-themes. Filed from CuTePi (show playback), which uses `.waveform`, `.waveform-region` and
`.waveform-marker` for its cue trim timeline since ftl-themes 8c471d5.*

## What we need

A way for `.waveform` to show **part of a clip** (a time window) instead of the whole of it, with regions, markers,
the seek input and the hover time all following that window, and a standard way to change the window (zoom in, zoom
out, zoom to a selection, pan, reset).

## Why

A cue's trim In/Out has to land on an exact frame or a beat. On a 10-minute clip in a ~1,400 px inspector, one pixel is
about 0.4 s, and one bar (3 px) about 1.3 s: a handle cannot be placed closer than that, and a 1 s fade is two bars
wide. Every editor that trims media (QLab, Premiere, Audacity, Reaper) zooms for that reason. The component docs say
zoom is the app's job; doing it in the app means re-implementing parts of the component around it (below), and each
app will do it differently and hit the same traps.

## How CuTePi does it today (`public/src/trimline.js`)

- **The window.** `view = {from, to}` in seconds (`null` = whole clip). Every render maps the window onto the
  component: the seek input's and both region edges' `min`/`max` become `from`/`to`; region `--start`/`--end` and
  marker `--at` are computed against the window; bars are resampled for it.
- **Bars.** About one `<i>` per 3 px, created once per width; `--level` is updated per frame (zoom animates over
  200 ms, so nodes are reused, not rebuilt). Zoomed past the stored envelope's resolution, the app fetches a finer
  envelope for the window from the server (`/api/media/:name/wave?from&to&bins`, under 60 s windows).
- **Controls.** A Zoom button arms a box-select (drag over the waveform to zoom into that span), +/− step (halve /
  double around the centre), Zoom reset, and the mouse wheel pans a zoomed window. The window survives the
  inspector re-rendering, and resets when another clip is selected.
- **The trap: silent clamping.** A region edge whose time is outside the window cannot be represented: setting
  `input.value` outside `[min, max]` makes the browser clamp it **without an event**, and an app that reads both edges
  back on save writes the clamped value, moving the other end of the trim. CuTePi keeps the true times in its own
  state, disables (and hides) an edge that is out of view, and only ever reads the edge that fired. Body-drag of a
  region (`controls.js`) moves both edges and can't be used while zoomed for the same reason (we use `.is-static`).
- **Markers** out of the window are hidden; region labels/handles clamp to the window edge.

## What a component-level zoom could look like

1. **Window as data.** `data-from` / `data-to` (seconds) on `.waveform`, defaulting to the seek input's `min`/`max`.
   CSS positions regions and markers by time against the window (`--start`/`--end`/`--at` stay fractions, or regions
   and markers accept times: `data-start`/`data-end`/`data-time`), so the app no longer converts everything.
2. **Out-of-window edges handled by the component**: an edge outside the window is drawn as an arrow at the
   window's side (".is-before" / ".is-after"), not clamped, and is not focusable while out of view; `controls.js`
   never rewrites an out-of-window edge's value.
3. **Zoom controls** as markup: a `.waveform-zoom` group (in, out, reset, "zoom to selection") that `controls.js`
   drives, firing a `waveform-view` event with `{from, to}` so the app can fetch finer peaks.
4. **Pan**: wheel / horizontal scroll / two-finger drag on a zoomed waveform, and a minimap (`.waveform-overview`: the
   whole clip with the window drawn as a draggable box) for long material.
5. **Box zoom**: an armed mode (`.is-zoom-select`) where a drag draws a selection box and zooms into it.
6. **Bars**: a documented way to swap bar data for a window (`waveform-view` → app sets `--level`s), and a note on
   reusing `<i>` nodes during animated zoom.
7. **Time ruler** (`.waveform-ruler`): tick labels that step out as the window widens (1 s, 5 s, 10 s, 1 min…).

## Accessibility

Zoom buttons need names ("Zoom in", "Show the whole clip"); the seek input's `aria-valuetext` keeps absolute times.
An out-of-view edge should say so ("Trim In, 0:06, before the visible part").
