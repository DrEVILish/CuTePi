# Feature request: showing fades on `.waveform`

*For DrEVILish/ftl-themes. Filed from CuTePi (show playback). Its cue trim timeline uses `.waveform` with the trim
window as a `.waveform-region`; every cue can fade in from its In point and out into its Out point, over a time and
a curve (linear, smooth, log, exp). The operator needs to see those fades where they happen.*

The library has no fade or gain-envelope part yet. Below are ten ways to show a fade on a waveform: five simple and
five more extreme. CuTePi picked **#1 (gain-shaped bars)** plus fade-boundary markers. It works with the component
as shipped. What we'd like upstream is that, plus the ghost layer from #6, as a supported part of `.waveform`.

## Simple, elegant

1. **Gain-shaped bars** — ✅ *CuTePi's pick.* Each bar's `--level` is the peak times the engine's fade gain at that
   moment, so the waveform shows what will actually be heard: bars grow out of silence at the In point and sink
   into it at the Out point, in the exact curve. No new CSS: it is the app's data. Two `.waveform-marker`s name the
   boundaries ("Fade in 2 s", "Fade out 3 s"), so a fade too short to see on a long clip is still marked.
   *Why picked:* it is the truth (what you see is what plays), costs nothing in the component, and needs no
   explanation. *Weakness:* the material under the fade is no longer visible at full height (fixed by #6).
2. **Fixed fade regions** — two inputless `.waveform-region`s (`data-color`), from In to In+fade and from Out−fade to
   Out, labelled "Fade in 2 s". Uses what exists; doesn't show the curve's shape.
3. **Ramp curve overlay** — an SVG path of the gain curve drawn over the bars (CuTePi's old canvas timeline did this).
   Shows the shape; a second drawing system on top of the component.
4. **Gradient tint** — the fade regions' tint is a gradient whose stops follow the curve (solid where silent, clear at
   full level). CSS-only once the app computes a few stops; reads at a glance.
5. **Draggable fade handles** — the fade-in is a region with its start pinned to In and its end draggable (and the
   reverse for the fade-out), so the fade length is set by dragging on the waveform instead of typing a time.

## Extreme (the wackier, the better)

6. **Ghost waveform** — the full-level bars drawn as a faint ghost behind the gain-shaped bars (#1), so the gap
   between them is literally the sound the fade removes. *Needs the component:* a second bar layer or a
   `--ghost-level` per bar (the bars are windows cut into a mask, so two layers of `<i>` intersect instead of
   overlaying today).
7. **Fisheye ends** — the time axis is non-linear: the trim window's first and last few seconds are magnified (a lens)
   so a 1 s fade on a 10-minute clip gets real width, with handles and markers following the warped scale. Solves
   the "fades are invisible on long clips" problem without zooming.
8. **Elastic curve band** — grab the middle of a fade and pull: the ramp bends like a rubber band through linear →
   smooth → log → exp, snapping to each preset with a little overshoot, and saves the curve on release. The curve
   picker becomes a gesture.
9. **Hear it on hover** — hovering a fade asks the server for a 2 s preview of exactly that moment with the fade
   applied (same gain code as the show) and plays it on the operator's device (never the wall); the bars light up as
   it plays. Audition without firing the cue.
10. **Fog of distance** — the faded parts recede: CSS perspective tilts the waveform away into the distance and a
    fog (blur and desaturation by gain) swallows the bars as they fade, so a fade-out looks like the sound driving
    off into the night. Pure theatre, very on-brand for a theatre playback system.

## What we'd like in ftl-themes

- **Gain envelope as a first-class part**: `--gain` (0–1) per bar alongside `--level`, the visible height being
  `level × gain`, with an optional **ghost** (#6) showing `level` behind it (`.waveform.has-ghost`).
- **Fade region variant**: `.waveform-region.is-fade-in` / `.is-fade-out` with the curve drawn as the region's top
  edge (from `data-curve="linear|smooth|log|exp"`), optionally with one draggable edge (#5).
- **Marker rows**: a way to put markers on a second row (CuTePi offsets fade-boundary chips by hand) so time chips
  and fade chips don't collide.

## Notes from CuTePi's implementation

- The gain curve has to be the engine's, mirrored exactly (`fadeShape`): smooth = `t²(3−2t)`, log = `log10(1+9t)`,
  exp = `(e^{3t}−1)/(e³−1)`. A display curve that differs from what plays is worse than none.
- Outside the trim window, bars are shown at full level: that is the material you pick trim points from.
- Overlapping fades (a short trim with long fades) take the lower of the two gains.
