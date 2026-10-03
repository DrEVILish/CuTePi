# CuTePi system test report — 2026-09-29

## How it was tested

- **Target:** the real `cutepi` service on the test server (`192.168.10.73:80`, Raspberry Pi 4, 1080p60 HDMI monitor, `fbdevsink` wall).
- **UI:** driven with Playwright on the dev server (`192.168.10.162`), pointed at the test server. Nothing was installed on the test server.
- **Picture:** measured from outside the process by sampling `/dev/fb0` (RGB565 luma trace, 20ms resolution) on the test server. The sampler's timestamps were merged with `MARK` lines from the UI scripts.
- **Before each phase:** the baseline database and config were restored.
- **Baseline:** 6 cues and a slideshow group "New Group" (2 members), `escFadeMs=1000` (the default), `panicHoldImage=test_blue_1080p.png`.
- **Go tests:** `go test ./...` passes for every package, and `gofmt` and `go vet` are clean.

### Phases and results (all green after fixes)

| Suite | Area | Result |
|---|---|---|
| p1a–p1c | Shell, top bar, media pool | pass |
| p2a | Selection model, GO bar | pass |
| p3a | Cue inspector (26 checks) | pass |
| p4a–p4d | Cue sheet editing, drag and drop, groups, import/export | pass |
| d9, p5 | Context menus, delete, show import | pass |
| p6a | Transport: Space/GO, ESC fade, double-ESC, Panic confirm, progress (13) | pass |
| p6b | Tests picker, custom patterns, two-client WebSocket sync, Show mode (17) | pass |
| Proxy | Browser load through `cutepi-test.drevilish.com` | pass (after fix P1) |

## Verified behaviour (cause → effect)

| Action | Observed effect |
|---|---|
| Space in the control UI | GO fires the selected cue; the progress clock advances via WebSocket. |
| Enter | Does not fire GO. |
| ESC once | Audio and video fade over `escFadeMs`. The framebuffer shows black at about 600ms with a 500ms setting (after fix D19). |
| ESC twice | Immediate cut to the holding image, about 420ms after the second press (image load latency; see O7, since fixed: 54–80ms). |
| Panic | Confirm dialog appears. Cancel keeps playing; accept cuts to the holding image. |
| Stop, Panic (no holding image), natural end of stream | Framebuffer goes to 0 (black) (after fix D17). |
| Menu > Fade out | Same fade as a single ESC (after fix D16). |
| Two clients | Client B follows A's selection, GO and NOW bar without reload. ESC from B stops both. Idle: 0 HTTP requests in 6s. |
| Show mode | Persists across reload. Arrow keys and Space still work. Pool, inspector and Tests button are hidden. Edit gestures show the toast "Switch to EDIT mode to change the sheet". Test APIs return 403. |
| Tests | Toggle, 16 built-in patterns, Hide Test, pin/unpin custom media, list refreshes without reload (after fix D18b). |
| Direct play of a still | Holds on screen (after fix D18a). |

## Deviations

Each entry gives:
1. The possible root causes, ranked by likelihood.
2. How each cause was verified.
3. The fix for the most likely cause.
4. Related issues to check.

### P1 — `cutepi-test.drevilish.com` does not load (fixed)

1. **Causes:**
   - (a) CuTePi's DNS-rebinding guard (`routes/origin.go`, `SameOrigin`) refuses any public Host name not listed in config `allowed_hosts`, with HTTP 421.
   - (b) The proxy host is misconfigured in Nginx Proxy Manager.
   - (c) The WebSocket upgrade isn't forwarded.
   - (d) The Origin check rejects POSTs behind the proxy.
2. **Verification:**
   - `curl -H 'Host: cutepi-test.drevilish.com' http://192.168.10.73/` returned **421 directly from the app**, so the app, not the proxy, was refusing (a is confirmed).
   - The proxy forwards correctly: after the fix, `http://cutepi-test.drevilish.com/` returns 200 (b is ruled out for HTTP).
   - The WebSocket upgrade through the proxy returns 101 (c is ruled out).
   - A browser POST with Origin `http(s)://cutepi-test.drevilish.com` returns 200 (d is ruled out, because NPM forwards `Host`).
3. **Fix:**
   - Added `"allowed_hosts": ["cutepi-test.drevilish.com"]` to `/root/cutepi/config/config.json` on the test server (and to the test baseline).
   - The 421 used to be an empty page. It now says which Host was refused and how to allow it, and logs `RTE-E229`.
   - AGENTS.md documents the requirement.
   - A Playwright load through the proxy name gives 200, 6 cues, a WebSocket connection and no errors.
4. **Related:**
   - **HTTPS is not configured on the proxy.** Both `cutepi-test` and `cutepi-dev` fail the TLS handshake with alert 112 (unrecognized_name), so NPM has no certificate for these proxy hosts. Attach a certificate in NPM > Proxy Hosts > SSL.
   - The dev server runs the committed code, which has no guard, so `cutepi-dev` works today. Once this change is deployed there, add `cutepi-dev.drevilish.com` to that machine's `allowed_hosts`.

### D19 — ESC fade of 500ms took about 2s on 1080p video (fixed)

1. **Causes:**
   - (a) Setting `videobalance.brightness` blocks on the element's streaming lock while a frame is processed. This happens inline in the fade loop, under `mgr.mu`.
   - (b) Sleep drift in the 20-step loop.
   - (c) Lock contention from status readers.
2. **Verification:**
   - Instrumented `FadeAndStop`: each sleep was exactly 25ms and the lock wait was 0 (b and c are ruled out).
   - Every 5th to 7th step, `applyBrightness` took **425–490ms** (a is confirmed).
   - A framebuffer trace showed 211 → 44 at 1.3s → 0 at 2.04s.
3. **Fix:** `gsp.applyBrightness` hands brightness updates for video to a `brightWorker` goroutine that always applies the latest level. The audio ramp and the fade deadline no longer wait on the video branch. Result: the fade ends and the picture is black at about 600ms. The UI's "Nothing playing" state arrives 0.73s after ESC instead of 2.1s. p6b 19.5 is green.
4. **Related:** the brightness fade itself only shows 1–3 intermediate frames. See O1; that is the real underlying problem.

### D17 — Last frame stayed on HDMI after Stop, Panic, fade end or natural end (fixed)

1. **Causes:**
   - (a) `fbdevsink` leaves its last frame in the framebuffer at teardown.
   - (b) The pipeline was not torn down.
2. **Verification:** the luma trace kept the frame's value after teardown, and pipeline state was NULL (a).
3. **Fix:** `gsp/blank.go` `blankWall` zero-fills `/dev/fb0` (stride × height from sysfs), guarded by the generation counter. It runs immediately on Stop and Panic, and 150ms after end of stream. Unit tests were added. The trace shows 0 after every stop path.
4. **Related:** this masked D18a.

### D18a — Direct play of an image tore down after its first frame (fixed)

1. **Causes:**
   - (a) Direct `/api/play` and `/api/load` built `LoadOpts` without `Hold` for stills, so end of stream (immediate for an image) tore it down.
   - (b) An image decoding issue.
2. **Verification:** after D17 the screen went black at once. Cue playback of the same file held (a).
3. **Fix:** `Hold: gsp.IsStill(filename)` on the direct routes and `Load`, plus the test `TestDirectPlayImageHolds`.
4. **Related:** holding-image load on Panic already used Hold.

### D18b — Custom test patterns were stale in the Tests picker until reload (fixed)

1. **Causes:**
   - (a) The picker was built once, guarded by `testPatternsLoaded`.
   - (b) The pin API didn't persist.
2. **Verification:** after a reload the pin showed (b is ruled out).
3. **Fix:** the grid is rebuilt on every open.
4. **Related:** none found.

### D16 — Menu > Fade out cut instead of fading (fixed)

1. **Causes:**
   - (a) `/api/fadeOut` called a different code path from ESC.
   - (b) `escFadeMs` wasn't read.
2. **Verification:** the log and luma trace showed an instant cut (a).
3. **Fix:** a shared `fadeStop` handler serves both `/api/fadeOut` and `/api/esc`. The fade is now about 600ms, then black.
4. **Related:** Stop and Panic remain immediate cuts, as designed.

### D15 — `DELETE /api/cue/N` returned 500 (UNIQUE) after a drag reorder (fixed)

1. **Causes:**
   - (a) The `RemoveCue` re-index updated `cuePos` in place and collided with the `UNIQUE` constraint.
   - (b) Stale positions on the client.
2. **Verification:** the SQL error named `cuesheet.cuePos` (a).
3. **Fix:** a two-pass re-index (offset, then compact), with tests in `ctp_test.go`.
4. **Related:** media delete now also unpins the test pattern and clears the holding image.

### D8 — Inspector autosave lost-update race (fixed)

1. **Causes:**
   - (a) The inspector PUT took about 350ms, so a second edit raced the first and the older response overwrote it.
   - (b) Debounce ordering on the client.
2. **Verification:** timed PUTs, and two quick edits lost the rotation value (a).
3. **Fix:** the PUT path was made fast (about 12ms). Both gap cases now keep the last value.
4. **Related:** D6.

### D6 — Inspector trim times truncated to centiseconds on unrelated saves (fixed)

1. **Causes:**
   - (a) The client `fmtClock` formatted to 10ms, and a save re-posted the displayed value.
   - (b) The server rounded.
2. **Verification:** the posted body contained `posStart=0:01.60` for a 1.605s value (a).
3. **Fix:** millisecond formatting that matches `hh:mm:ss.mmm`.
4. **Related:** D7.

### D7 — Misleading trim error message (fixed)

Invalid Trim Out gave text that blamed the wrong field. The message was corrected.

### D1 — Group inspector not rendered on first page load (fixed)

1. **Causes:**
   - (a) `inspectorData()` in `routes/index.go` handled only a selected cue (positive id), not a group (negative id).
   - (b) The client didn't request it.
2. **Verification:** the server-rendered HTML lacked the group inspector (a).
3. **Fix:** handle group selection in the initial render, with a regression test.

### D2 — Unexpected client count (not a defect)

The extra WebSocket client was the user's own browser (192.168.10.100). Tests now compare against a baseline count.

### D3 — GO bar label wrong for some selection states (fixed)

Fixed for all three states: a cue, a group, and nothing selected.

### D9–D14 — UI error handling (fixed)

- **Error text:** the htmx v4 event names were wrong (`htmx:after:swap` and `htmx:response:error` are the correct forms). Error toasts showed a full HTML page because `error.html` is a whole document. Drop zones and drag-and-drop didn't show the reason.
- **Fixed in:** `ui.js`, `dropzone.js` and `dnd.js`. They now extract the `<pre>` reason. Toasts gained kinds (success, error).
- **Import audit:** the show import now records an audit event (`show_imported`).
- **Invalid times:** now return 400 with a message instead of 500.
- **Related:** O4 is the root of D12.

## Round 2 — answers applied (2026-09-29)

| # | Your answer | What changed | Verified |
|---|---|---|---|
| 1 | Always match the display's frame rate | Requirement added to DESIGN.md §2. Not met yet: see O1 and its new `kmssink` measurement. | — |
| 2 | Docs say "Space" only | GO tooltip is "Fire the selected cue (Space)"; Enter removed from Settings > Keys. DESIGN.md already said Space only. | p7 21.1, 21.2 |
| 3 | Escape cancels the inline editor | `cueeditcol.html`: Escape closes the editor, keeps the old value, sends nothing and does not reach the transport. DESIGN.md §5.4. | p7 22.1, 22.2 |
| 4 | No upper limit on times | Documented in DESIGN.md §5.4 (the code already had no limit). | — |
| 5 | Default fade 1000ms, changeable | Code default was already 1000ms (`ctp.DefaultEscFadeMs`). DESIGN.md §5.6 notes it can be changed. | — |
| 6 | Keep the menus, don't document | No change. | — |
| 7 | Remove F8; Escape closes menus and double-click adds a cue are intended; no duplicate cue numbers | F8 handler and its row in the Keys table removed. Escape-closes-menu and double-click-adds-cue documented (§5.3, §5.4). Duplicate cue numbers are refused (below). Toasts are listed in §5.11. | p7 21.3, 23.x |
| 8 | Offer replace / rename / skip | Upload name-conflict dialog (below). Mobile upload retested on an emulated iPhone (below). | p7 24.x, p7m 25.x |
| 9 | Delete media also unpins and clears the holding image | Documented in §5.3. | earlier test |
| 10 | Show mode as designed | New DESIGN.md §5.10 Show / Edit mode. | p6b 20.x |

**Suite p7 (desktop Chromium):** 14 pass, 0 console errors, 0 failed requests. **p7m (iPhone 15 on WebKit, via the proxy):** 2 pass.

### D20 — Duplicate cue numbers gave a 500 and a logged database error (fixed)

1. **Causes:**
   - (a) Nothing validated numbers. The `UNIQUE` constraint on `cuesheet.cueNum` caught cue-to-cue clashes as a database error: 500, "Error updating cue: UNIQUE constraint failed" in the log, and an error toast.
   - (b) Group numbers live in another table, so a group could take a cue's number, and `12` and `12.0` counted as different numbers.
   - (c) Append import renamed clashes to `"20 (2)"`.
2. **Verification:** `PUT /api/cue/3/edit/cueNum val=20` returned 500 with the UNIQUE error in the journal (a). Setting a group to `30` succeeded while cue 30 existed (b).
3. **Fix:**
   - `ctp.checkCueNumFree` checks numbers across cues and group headers (blanks allowed, numeric equality), only when a number actually changes. It is used by `UpdateCue`, `UpdateCueFields`, `UpdateGroup` and auto-numbering.
   - Routes answer 200 with the unchanged sheet and an `HX-Trigger: cueNumRejected` event. `ui.js` shows the reason as a tooltip on the field for 3.5s and resets an input field. Nothing is logged.
   - Import appends keep a free number, otherwise they take the next whole number above the highest.
   - Test: `TestCueNumbersStayUnique`.
4. **Related:**
   - A blank group or cue number cell had zero width, so it could not be double-clicked. Fixed with a minimum size in `cuesheet.css`.
   - `GetCue` logged "Error Getting Cue: no rows" whenever a group was selected, which is noise that looks like a fault. It now logs only real database errors.

### D21 — Upload silently replaced a same-name file (fixed)

1. **Causes:**
   - (a) `importMedia` always set aside and replaced an existing file, and nothing asked the operator.
   - (b) Duplicate names within one batch overwrote each other.
2. **Verification:** code reading, and the earlier O3 observation.
3. **Fix:**
   - `POST /upload/check` returns the clashing names, including repeats within a batch, before any bytes are sent. The dialog offers Replace, Keep both (`name (2).ext`), Skip or Cancel.
   - The choice is sent as `onConflict`. An unresolved clash gets 409 and changes nothing.
   - The result summary comes back in `X-Upload-Result` and is shown on the status line and in the toast.
   - This applies to the upload modal, the standalone `/upload` page and pool drag-and-drop.
   - Test: `TestUploadNameConflictChoices`.
4. **Related:** YouTube/URL downloads still replace on a name clash. Not covered by your answer; same dialog could apply.

### D22 — Mobile upload gave no feedback (cause found and fixed; please retest on the real iPhone)

1. **Possible causes, ranked:**
   - (a) If the upload page was opened through `cutepi-test.drevilish.com` before today's proxy fix, every request (page, scripts, upload) was refused with a blank 421.
   - (b) One tap on "Choose a file" opened the file picker **twice**: the label opens it natively, then the drop zone's click handler called `fileInput.click()` again. On iOS Safari the second call can dismiss the first picker, so the chosen files are lost and nothing is shown.
   - (c) iPhone formats: HEIC photos are not an accepted type. They are refused with a clear message, but only if the upload reaches the server.
   - (d) A proxy body-size limit.
   - (e) Files larger than the 2 GiB request cap (long 4K iPhone videos).
2. **Verification:**
   - The service journal had already rotated past your attempt (it only reached back to 19:42), so it can't show which cause hit you.
   - **Playwright WebKit with the iPhone 15 profile:** JPEG, MOV and HEIC uploads, through the proxy and via the IP, all showed a status line (HEIC: "unsupported media type"). The emulation counted **2 file-chooser openings per tap** (b is confirmed as a defect).
   - A 150MB upload passed through the proxy (d is ruled out up to that size).
3. **Fix:**
   - The drop zone no longer re-opens the picker when the tap lands on the label; the emulation now counts 1.
   - (a) was fixed by the proxy change.
4. **Related:**
   - **Please retest on the iPhone** (Safari on iOS differs from the WebKit emulation): upload a photo and a video from the Photos picker, then the same photo again to see the name-clash dialog.
   - DESIGN.md §5.7 says "no size limit", but §7 caps request bodies at 2 GiB. Decide which is right.
   - HEIC support would need a GStreamer HEIF decoder on the Pi. Today iOS usually converts photos to JPEG on upload, but not always.

## Round 4 (2026-09-30)

| # | Issue | Root cause (verified) | Fix | Verified |
|---|---|---|---|---|
| D37 | 20 s fade-in "jumps the video 20 s" | Not the fade: the film has a second audio track drained by an unsynced fakesink, and the pipeline position (furthest sink) read ~20 s after 1 s of play, then lurched. Same numbers with fade-in 0 and with direct play. The fade itself ramped alpha correctly | Extra streams drain in real time (`sync=true`) | Position 1.05, 2.11, 3.18, 4.26 s… |
| D38 | Progress bar and time jumpy | D37 plus a 500 ms clock tick, a row bar that moved only on sheet re-render, and a 0.4 s CSS transition | One 100 ms client clock drives the clock, scrubber and row bar; never steps back | 40 samples: steps 0.05–0.2 s, 0 backward |
| D39 | Space/ESC ignored after clicking a button or in dialogs; Space re-pressed the focused button | Key handler treated focus on any button, select or input as "not plain" | Only text entry blocks them; Space never presses the focused control; no auto-repeat | Keys suite 4/4 |
| New | Fit options and crop | — | Fill width, Fill height, Fill (cover), plus crop L/R/T/B in px or %; hardware crop via the plane's source rectangle | Plane SRC/CRTC rects exact, 30 fps each |

## Codec corpus (2026-10-01)

CuTePi must play any codec the user provides (DESIGN §2), so playback is tested against a corpus of many codecs, not
only H.264/HEVC test clips. `tools/codec-corpus/make.sh` generates 52 five-second files with the Pi's own ffmpeg and
GStreamer (nothing installed) into `/root/cutepi-testmedia` (359 MB, outside the media pool):
- **video (24):** H.264 (720p50, 1080p30/60, interlaced, High 10, vertical), HEVC (1080p30/60, Main 10, 2160p30),
  VP8, VP9, AV1, MPEG-2 (1080i), MPEG-4 ASP, ProRes 422 HQ and 4444 with alpha, DNxHR HQ, MJPEG, Theora, WMV2,
  FFV1, QuickTime Animation, HAP;
- **audio (12):** AAC, MP3, Opus, Vorbis, FLAC 96 kHz/24-bit, PCM 16- and 24-bit, AC-3 5.1, E-AC-3 5.1, ALAC,
  AIFF mono 22 kHz, WMA;
- **image (16):** JPEG (baseline, progressive, 4000×3000), PNG (8-bit, 16-bit, RGBA, portrait), WebP (lossy and
  lossless), GIF (still and animated), BMP, TIFF, JPEG 2000, AVIF, JPEG XL.

Two checks, both run on the test server:
1. `probe.py` decodes each file through GStreamer as the service does (playbin, same decoder ranks) into fake sinks
   and records the decoder chosen, hardware or software, and decode speed (decoder capacity, not display rate).
2. `service.py` runs each file end to end through the live service: upload as the web UI does, play, then check the
   output from outside the process (a visible display plane for video and images; a running HDMI PCM stream for
   audio), stop, delete.

### Findings and fixes

| # | Finding | Root cause (verified) | Fix | Verified |
|---|---|---|---|---|
| K1 | 13 of 52 files refused at upload: `.ts .mpg .ogv .wmv .opus .ac3 .eac3 .aiff .wma .tiff .jp2 .jxl .avif` | An extension allow-list (`media.KindFromExtension`) gated uploads before any probe, contradicting "any codec" | The extension no longer decides; the kind list is broadened and only used as a hint; images are recognised by ffmpeg's image demuxers (`image2`, `gif`, `*_pipe`) or the extension | all 13 import (4 then refused by K4 with a reason) |
| K2 | TIFF, JPEG 2000 etc. would not have been held as stills | Playback's still check had its own six-extension list | `gsp.isStillFile` uses the same list as import | stills hold |
| K3 | JPEG 2000 fails to decode | `openjpegdec` fails to negotiate its output on the Pi | Demoted (`gsp.decoderRankOverrides`); `avdec_jpeg2000` decodes it | probe: plays |
| K4 | WMA/WMV, AVIF and JPEG XL imported (ffmpeg reads them) but could never play | Import verified with ffmpeg; this system's GStreamer has no ASF demuxer and no AVIF or JPEG XL decoder | Import also prerolls the file through GStreamer (`gsp.CheckDecodable`) and refuses with the reason | refused at upload: "this system has no GStreamer demuxer for video/x-ms-asf" etc. |
| K5 | DNxHR HQ and ProRes 422 HQ (4:2:2) imported but never reached the screen | On the KMS wall `videoconvert` chose a 4:2:2 layout the plane lists but the kernel cannot allocate ("failed to activate bufferpool") | Converted frames limited to 4:2:0 YUV and RGB (`kmsSysmemCaps`); 4:2:2 goes to RGB, full chroma | both on screen 1920×1080 |

End to end through the service: **37 of 52** before the fixes, **48 of 52** after. The 4 remaining are refused at
import with the reason.

### Open (decisions)

- **WMV / WMA** need GStreamer's ASF demuxer (Debian package `gstreamer1.0-plugins-ugly`); the decoders
  (`avdec_wmv2`, `avdec_wmav1`) are already installed. Installing it is a deployment decision (AGENTS.md: no extra
  packages on the test server without approval).
- **AVIF and JPEG XL stills** have no GStreamer decoder on this system. Options: install decoder plugins if Debian
  ships them for this GStreamer, or convert such stills losslessly to PNG at import with the Pi's ffmpeg (which reads
  both).
- **AV1 1080p30** decodes at 28 fps in software (`av1dec`, libaom), below its 30 fps. The much faster dav1d decoder is
  in this ffmpeg but not exposed to GStreamer (`dav1ddec`/`avdec_libdav1d` absent). Needs the GStreamer dav1d plugin
  (a package decision); meanwhile the planned import warning (DESIGN §5.7) will flag it.
- **Waveform/loudness analysis** fails (ffmpeg exit 254) for PCM audio inside `.mov` (ProRes, DNxHR files). Playback is
  unaffected; the waveform display and loudness gain are missing for those files.

### Decode probe (`probe.py`): decoder and capacity per file

Speed is decoding as fast as possible into fake sinks, no display.

| File | Codecs | Format | Decoder | HW | Decode speed | Result |
|---|---|---|---|---|---|---|
| audio_aac_48k_stereo.m4a | aac | 48000 Hz 2ch | avdec_aac | no | 10× real time | ok |
| audio_ac3_5.1.ac3 | ac3 | 48000 Hz 6ch | avdec_ac3 | no | 14× real time | ok |
| audio_alac_44k1.m4a | alac | 44100 Hz 2ch | avdec_alac | no | 15× real time | ok |
| audio_eac3_5.1.eac3 | eac3 | 48000 Hz 6ch | avdec_eac3 | no | 14× real time | ok |
| audio_flac_96k_24bit.flac | flac | 96000 Hz 2ch | flacdec | no | 37× real time | ok |
| audio_mp3_44k1_stereo.mp3 | mp3 | 44100 Hz 2ch | mpg123audiodec | no | 35× real time | ok |
| audio_opus_48k_stereo.opus | opus | 48000 Hz 2ch | opusdec | no | 26× real time | ok |
| audio_pcm_mono_22k05.aiff | pcm_s16be | 22050 Hz 1ch | - | no | 45× real time | ok |
| audio_pcm_s16_48k.wav | pcm_s16le | 48000 Hz 2ch | - | no | 44× real time | ok |
| audio_pcm_s24_96k.wav | pcm_s24le | 96000 Hz 2ch | - | no | 43× real time | ok |
| audio_vorbis_48k_stereo.ogg | vorbis | 48000 Hz 2ch | vorbisdec | no | 36× real time | ok |
| audio_wma_44k1.wma | wmav1 | 44100 Hz 2ch | - | no | - | **fails**: Missing element: Advanced Streaming Format (ASF) demuxer |
| image_avif_1920x1080.avif | av1 Main | 1920x1080@1 | - | no | - | **fails**: ERROR: from element /GstPlayBin:playbin0/GstURIDecodeBin:uridecodebin0/GstDecodeBin:decodebin0/GstQTDemux:qtdemux0: This file contains no playable streams. |
| image_bmp_1920x1080.bmp | bmp | 1920x1080@25 | gdkpixbufdec | no | 161 ms | ok |
| image_gif_1920x1080.gif | gif | 1920x1080@1 | avdec_gif | no | 366 ms | ok |
| image_gif_animated.gif | gif | 640x360@10 | avdec_gif | no | 432 ms | ok |
| image_jpeg2000_1920x1080.jp2 | jpeg2000 0 | 1920x1080@25 | avdec_jpeg2000 | no | 747 ms | ok |
| image_jpeg_1920x1080.jpg | mjpeg Baseline | 1920x1080@25 | jpegdec | no | 169 ms | ok |
| image_jpeg_4000x3000.jpg | mjpeg Baseline | 4000x3000@25 | jpegdec | no | 193 ms | ok |
| image_jpeg_progressive.jpg | mjpeg Baseline | 1920x1080@25 | jpegdec | no | 132 ms | ok |
| image_jxl_1920x1080.jxl | jpegxl | 1920x1080@25 | - | no | - | **fails**: ERROR: from element /GstPlayBin:playbin0/GstURIDecodeBin:uridecodebin0/GstDecodeBin:decodebin0/GstTypeFindElement:typefind: Could not determine type of stream. |
| image_png_16bit.png | png | 1920x1080@25 16-bit | pngdec | no | 206 ms | ok |
| image_png_1920x1080.png | png | 1920x1080@25 | pngdec | no | 148 ms | ok |
| image_png_portrait_1080x1920.png | png | 1080x1920@25 | pngdec | no | 148 ms | ok |
| image_png_rgba_transparent.png | png | 1920x1080@25 | pngdec | no | 156 ms | ok |
| image_tiff_1920x1080.tiff | tiff | 1920x1080@25 | gdkpixbufdec | no | 201 ms | ok |
| image_webp_1920x1080.webp | webp | 1920x1080@25 | webpdec | no | 148 ms | ok |
| image_webp_lossless.webp | webp | 1920x1080@25 | webpdec | no | 169 ms | ok |
| video_av1_1080p30_opus.mkv | av1 Main + opus | 1920x1080@30 48000 Hz 1ch | av1dec, opusdec | no | 28 fps (BELOW at 30 fps) | ok |
| video_dnxhr_hq_1080p25_pcm.mov | dnxhd DNXHR HQ + pcm_s16le | 1920x1080@25 48000 Hz 1ch | avdec_dnxhd | no | 109 fps (OK at 25 fps) | ok |
| video_ffv1_1080p25_flac.mkv | ffv1 + flac | 1920x1080@25 48000 Hz 1ch | avdec_ffv1, flacdec | no | 78 fps (OK at 25 fps) | ok |
| video_h264_1080p25_interlaced.ts | h264 High + ac3 | 1920x1080@25i 48000 Hz 1ch | avdec_ac3, v4l2h264dec | yes | 54 fps (OK at 25 fps) | ok |
| video_h264_1080p30_aac.mp4 | h264 High + aac | 1920x1080@30 48000 Hz 1ch | avdec_aac, v4l2h264dec | yes | 71 fps (OK at 30 fps) | ok |
| video_h264_1080p60_aac.mp4 | h264 High + aac | 1920x1080@60 48000 Hz 1ch | avdec_aac, v4l2h264dec | yes | 72 fps (OK at 60 fps) | ok |
| video_h264_720p50_mp3.mkv | h264 High + mp3 | 1280x720@50 48000 Hz 1ch | mpg123audiodec, v4l2h264dec | yes | 154 fps (OK at 50 fps) | ok |
| video_h264_high10_1080p30.mkv | h264 High 10 + aac | 1920x1080@30 10-bit 48000 Hz 1ch | avdec_aac, avdec_h264 | no | 56 fps (OK at 30 fps) | ok |
| video_h264_vertical_1080x1920.mp4 | h264 High + aac | 1080x1920@30 48000 Hz 1ch | avdec_aac, v4l2h264dec | yes | 70 fps (OK at 30 fps) | ok |
| video_hap_1080p25.mov | hap | 1920x1080@25 | avdec_hap | no | 97 fps (OK at 25 fps) | ok |
| video_hevc_1080p30_aac.mp4 | hevc Main + aac | 1920x1080@30 48000 Hz 1ch | avdec_aac, v4l2slh265dec | yes | 177 fps (OK at 30 fps) | ok |
| video_hevc_1080p60_aac.mp4 | hevc Main + aac | 1920x1080@60 48000 Hz 1ch | avdec_aac, v4l2slh265dec | yes | 226 fps (OK at 60 fps) | ok |
| video_hevc_2160p30_aac.mp4 | hevc Main + aac | 3840x2160@30 48000 Hz 1ch | avdec_aac, v4l2slh265dec | yes | 67 fps (OK at 30 fps) | ok |
| video_hevc_main10_1080p30.mkv | hevc Main 10 + aac | 1920x1080@30 10-bit 48000 Hz 1ch | avdec_aac, v4l2slh265dec | yes | 166 fps (OK at 30 fps) | ok |
| video_mjpeg_1080p30_pcm.avi | mjpeg Baseline + pcm_s16le | 1920x1080@30 48000 Hz 1ch | jpegdec | no | 101 fps (OK at 30 fps) | ok |
| video_mpeg2_1080i25_ac3.mpg | mpeg2video Main + ac3 | 1920x1080@25i 48000 Hz 1ch | avdec_ac3, avdec_mpeg2video | no | 103 fps (OK at 25 fps) | ok |
| video_mpeg4asp_720p30_mp3.avi | mpeg4 Simple Profile + mp3 | 1280x720@30 48000 Hz 1ch | avdec_mpeg4, mpg123audiodec | no | 189 fps (OK at 30 fps) | ok |
| video_prores422hq_1080p25_pcm.mov | prores HQ + pcm_s24le | 1920x1080@25 10-bit 48000 Hz 1ch | avdec_prores | no | 71 fps (OK at 25 fps) | ok |
| video_prores4444_1080p25_alpha.mov | prores 4444 | 1920x1080@25 12-bit | avdec_prores | no | 46 fps (OK at 25 fps) | ok |
| video_qtrle_720p25_animation.mov | qtrle | 1280x720@25 | avdec_qtrle | no | 152 fps (OK at 25 fps) | ok |
| video_theora_720p30_vorbis.ogv | theora + vorbis | 1280x720@30 48000 Hz 1ch | theoradec, vorbisdec | no | 120 fps (OK at 30 fps) | ok |
| video_vp8_720p30_vorbis.webm | vp8 0 + vorbis | 1280x720@30 44100 Hz 1ch | vorbisdec, vp8dec | no | 196 fps (OK at 30 fps) | ok |
| video_vp9_1080p30_opus.webm | vp9 Profile 0 + opus | 1920x1080@30 48000 Hz 1ch | opusdec, vp9dec | no | 115 fps (OK at 30 fps) | ok |
| video_wmv2_720p30_wma.wmv | wmv2 + wmav1 | 1280x720@30 48000 Hz 1ch | - | no | - | **fails**: Missing element: Advanced Streaming Format (ASF) demuxer |

### End to end through the service (`service.py`), after the fixes

| File | Import | Play | Output | Result |
|---|---|---|---|---|
| audio_aac_48k_stereo.m4a | ok | ok | HDMI audio running | ok |
| audio_ac3_5.1.ac3 | ok | ok | HDMI audio running | ok |
| audio_alac_44k1.m4a | ok | ok | HDMI audio running | ok |
| audio_eac3_5.1.eac3 | ok | ok | HDMI audio running | ok |
| audio_flac_96k_24bit.flac | ok | ok | HDMI audio running | ok |
| audio_mp3_44k1_stereo.mp3 | ok | ok | HDMI audio running | ok |
| audio_opus_48k_stereo.opus | ok | ok | HDMI audio running | ok |
| audio_pcm_mono_22k05.aiff | ok | ok | HDMI audio running | ok |
| audio_pcm_s16_48k.wav | ok | ok | HDMI audio running | ok |
| audio_pcm_s24_96k.wav | ok | ok | HDMI audio running | ok |
| audio_vorbis_48k_stereo.ogg | ok | ok | HDMI audio running | ok |
| audio_wma_44k1.wma | refused 422: could not import 1 of 1: "audio_wma_44k1.wma": cannot be played: this system has no GStreamer demuxer for video/x-ms-asf | - | - | **fails** |
| image_avif_1920x1080.avif | refused 422: could not import 1 of 1: "image_avif_1920x1080.avif": cannot be played: GStreamer cannot decode it: This file contains n | - | - | **fails** |
| image_bmp_1920x1080.bmp | ok | ok | on screen 1920x1080 | ok |
| image_gif_1920x1080.gif | ok | ok | on screen 1920x1080 | ok |
| image_gif_animated.gif | ok | ok | on screen 1920x1080 | ok |
| image_jpeg2000_1920x1080.jp2 | ok | ok | on screen 1920x1080 | ok |
| image_jpeg_1920x1080.jpg | ok | ok | on screen 1920x1080 | ok |
| image_jpeg_4000x3000.jpg | ok | ok | on screen 1440x1080 | ok |
| image_jpeg_progressive.jpg | ok | ok | on screen 1920x1080 | ok |
| image_jxl_1920x1080.jxl | refused 422: could not import 1 of 1: "image_jxl_1920x1080.jxl": cannot be played: GStreamer cannot decode it: Could not determine ty | - | - | **fails** |
| image_png_16bit.png | ok | ok | on screen 1920x1080 | ok |
| image_png_1920x1080.png | ok | ok | on screen 1920x1080 | ok |
| image_png_portrait_1080x1920.png | ok | ok | on screen 607x1080 | ok |
| image_png_rgba_transparent.png | ok | ok | on screen 1920x1080 | ok |
| image_tiff_1920x1080.tiff | ok | ok | on screen 1920x1080 | ok |
| image_webp_1920x1080.webp | ok | ok | on screen 1920x1080 | ok |
| image_webp_lossless.webp | ok | ok | on screen 1920x1080 | ok |
| video_av1_1080p30_opus.mkv | ok | ok | on screen 1920x1080 | ok |
| video_dnxhr_hq_1080p25_pcm.mov | ok | ok | on screen 1920x1080 | ok |
| video_ffv1_1080p25_flac.mkv | ok | ok | on screen 1920x1080 | ok |
| video_h264_1080p25_interlaced.ts | ok | ok | on screen 1920x1080 | ok |
| video_h264_1080p30_aac.mp4 | ok | ok | on screen 1920x1080 | ok |
| video_h264_1080p60_aac.mp4 | ok | ok | on screen 1920x1080 | ok |
| video_h264_720p50_mp3.mkv | ok | ok | on screen 1920x1080 | ok |
| video_h264_high10_1080p30.mkv | ok | ok | on screen 1920x1080 | ok |
| video_h264_vertical_1080x1920.mp4 | ok | ok | on screen 607x1080 | ok |
| video_hap_1080p25.mov | ok | ok | on screen 1920x1080 | ok |
| video_hevc_1080p30_aac.mp4 | ok | ok | on screen 1920x1080 | ok |
| video_hevc_1080p60_aac.mp4 | ok | ok | on screen 1920x1080 | ok |
| video_hevc_2160p30_aac.mp4 | ok | ok | on screen 1920x1080 | ok |
| video_hevc_main10_1080p30.mkv | ok | ok | on screen 1920x1080 | ok |
| video_mjpeg_1080p30_pcm.avi | ok | ok | on screen 1920x1080 | ok |
| video_mpeg2_1080i25_ac3.mpg | ok | ok | on screen 1920x1080 | ok |
| video_mpeg4asp_720p30_mp3.avi | ok | ok | on screen 1920x1080 | ok |
| video_prores422hq_1080p25_pcm.mov | ok | ok | on screen 1920x1080 | ok |
| video_prores4444_1080p25_alpha.mov | ok | ok | on screen 1920x1080 | ok |
| video_qtrle_720p25_animation.mov | ok | ok | on screen 1920x1080 | ok |
| video_theora_720p30_vorbis.ogv | ok | ok | on screen 1920x1080 | ok |
| video_vp8_720p30_vorbis.webm | ok | ok | on screen 1920x1080 | ok |
| video_vp9_1080p30_opus.webm | ok | ok | on screen 1920x1080 | ok |
| video_wmv2_720p30_wma.wmv | refused 422: could not import 1 of 1: "video_wmv2_720p30_wma.wmv": cannot be played: this system has no GStreamer demuxer for video/x | - | - | **fails** |

48 of 52 files import and play.

## Codec support: transparency and animated images (2026-10-01)

Raspberry Pi 4 Model B Rev 1.5, KMS display planes, 1080p60 HDMI, live service. The support set
(`make-support.sh`) gained 13 alpha videos (`*_alpha`: ProRes 4444, qtrle ARGB, PNG, CineForm RGBA, HAP Alpha in MOV
and MKV; FFV1 and VP9 with alpha in MKV), 4 transparent stills (PNG, TIFF, WebP, GIF) and 5 animated images (GIF at
25 fps, 25 fps with transparency and 50 fps; APNG and animated WebP at 30 fps). Every alpha file uses one fixed mask:
opaque left third, a ramp to transparent across the middle, transparent right third. `support.py` now also checks:
alpha files must reach the plane in an alpha pixel format (read once from the DRM debugfs atomic state, at the end
of steady play) with the plane's `pixel blend mode` set to Coverage (straight alpha); animated images must present
every frame (≥ 98 % of the file's own rate) through the fades and steady play. Results are in the README table.
Setting: ESC fade 1000 ms (the default) restored after the run.

**Result: 0 of 21 supported.** What was measured:

| Finding | Files | Measured |
|---|---|---|
| Alpha reaches the plane | every alpha file that imports (11 videos, 4 stills, animated alpha GIF) | Plane format AB24 or AR24 (8-bit RGBA with alpha), including VP9 alpha through `vp9alphadecodebin`. Nothing drops the alpha channel on the way. |
| **Straight alpha blended as premultiplied** | all of them | Every plane keeps the kernel default `pixel blend mode` = Pre-multiplied. The decoders hand over straight alpha, so semi-transparent pixels (the ramp) show too bright. Fix: set Coverage per alpha layer through `kmssink plane-properties` (DESIGN §6.1.3). |
| Animated GIF plays every frame | gif_anim (25 fps) | Steady 25.1 fps, fade in 25, fade out 25.6: the animation keeps running through the fades. Fails only on fade smoothness (below). |
| Animated GIF with transparency | gif_anim_alpha | Steady 25.1 fps, fade in 21, fade out 24.4: loses a few frames while fading. |
| 50 fps GIF decode-bound | gif_anim_50fps | `avdec_gif` decodes 1080p at about 37 fps (400 frames in 10.7 s, no sink). The cue falls behind its clock and the sink drops the late frames: 0.5 fps on screen. |
| Fades on stills are coarse | every still, as in the opaque stills of the first batch | About 20 opacity steps a second (criterion 59), against 30 designed for the plane wall's alpha writer (one write per two refreshes). The cause is not yet traced. The GPU wall (§6.1.1) applies opacity on every output frame. |
| Software alpha codecs too slow at 1080p60 | ProRes 4444 0.3 fps, CineForm 0 fps, PNG video 0 fps, VP9 alpha 0.8 fps, FFV1 11 fps, qtrle 14.5 fps, HAP 17.8 fps | CPU decode plus the conversion to RGBA for the plane. As for the opaque versions of these codecs. |
| APNG refused at import | apng_anim.apng | No GStreamer APNG decoder (`avdec_apng` absent); the decode check times out (10 s). |
| Animated WebP refused at import | webp_anim_30fps.webp | `webpdec` decodes stills only: "Internal data stream error". This ffmpeg cannot decode animated WebP either. |
| HAP Alpha in MKV refused | hap_alpha.mkv | Same as hap.mkv: `matroskademux` has no codec-ID mapping for HAP (video/x-unknown). |
| qtrle in MKV unreadable | qtrle_alpha.mkv (and qtrle.mkv) | ffmpeg's own decoder refuses it ("Unsupported colorspace: 0 bits/sample"): Matroska does not store the QuickTime bit depth that qtrle needs, so the container cannot really carry qtrle. |

**Test-tool fixes in this batch.**
- *Generating the GIFs rebooted the Pi twice.* A one-pass `split → palettegen → paletteuse` graph over an endless
  `testsrc2` source buffers every 1080p frame, because palettegen emits its palette only at the end of the stream.
  The source never ended, so memory filled up. GIFs are now made in two passes (palette to a file, then
  `paletteuse`), with every source bounded by `duration=`. The generator was then run under `ulimit -v` as a guard.
- `support.py` only scans `video/`, `image/` and `audio/`: the alpha mask beside them had been run as a test file.
- `--only` takes a comma-separated list. Animated WebP is named with its rate (`*_anim_30fps`) because ffprobe cannot
  count its frames.

**Next.** On the KMS wall: Coverage blend mode for alpha layers, and animated images handled as timelines (not
stills: no re-render on fades, no infinite hold; DESIGN §6.1.3). Then rerun this batch. The fade-step rate and the
software codecs' frame rate are what the GPU compositor wall is for (§6.1.1). Decisions for the user: APNG and
animated WebP need a decoder this system lacks (or conversion at import).

## GPU upload routes for software-decoded video (2026-10-02)

Headless on the test server (EGL surfaceless on the render node; the service keeps the display), 1080p, into
`glvideomixerelement` (which samples every frame; a `glupload ! fakesink` never uploads, GL memory is lazy, so that
number means nothing). `videotestsrc pattern=solid-color` as the source unless a file is named.

| Route | 1 layer | 2 layers | 3 layers |
|---|---|---|---|
| I420 / NV12 system memory → `glupload` → mixer | 49.7 / 52.3 fps | 27.3 | – |
| RGBA system memory → `glupload` → mixer | 40.8 | – | – |
| I422_10LE system memory → `glupload` → mixer | 33.5 | – | – |
| I420 → ISP (`v4l2convert`) → **NV12 DMABuf** → `glupload` → mixer | **111.4** | **96.9** | 47.4 |
| I420 → ISP → RGBA / BGRx DMABuf → mixer | 82.6 / 83.1 | 41.8 (RGBA) | – |

Real files (8 s, 1080p60), decode → route → mixer, one layer:

| File | Direct `glupload` | `videoconvert n-threads=4` (if needed) → ISP NV12 DMABuf |
|---|---|---|
| MPEG-2 | 33.0 | 69.6 |
| DNxHR HQ / LB | 37.7 / 39.3 | 69.5 / 79.7 |
| FFV1 | 46.4 | 79.0 |
| VP9 | 43.2 | 62.1 |
| ProRes 422 / LT | 28.7 / 27.8 | 49.1 / 49.8 |
| Theora | 37.2 | 45.4 |
| H.264 High 10 | 9.3 | 28.1 |
| AV1 | 22.6 | 25.2 |

- On V3D a system-memory upload is tiled by the CPU, so the GPU wall as first designed would have played software
  codecs below 60 fps even on one layer. The ISP route writes linear NV12 into DMABufs that the GPU imports without a
  copy. DESIGN §6.1.1 amended.
- `glupload` and `glcolorconvert` accept every decoder format here (10/12-bit, 4:2:2, 4:4:4, alpha), so nothing
  fails to negotiate; the question was only speed. The ISP takes 8-bit YUV (I420, NV12, YUYV/UYVY) and RGB, not
  10-bit or planar 4:2:2, so those get a CPU repack first.
- Still open: the ISP route for alpha sources, and an ISP layer beside a hardware-decoded H.264 layer (they share the
  VideoCore).

**Hardware decoders into the mixer (no bridge)**, measured with a small C harness (`gst_parse_launch` refuses to link
a mixer pad behind a pinned DMA_DRM capsfilter at parse time, so the harness links the pads without that check):

| Layers | Mixer output |
|---|---|
| H.264 (default hand-over: DMA_DRM YU12, imported as one external texture) | 78.9 / 79.0 fps |
| H.264 + HEVC | 70.3 / 71.6 fps |
| HEVC | 214.6 fps |
| HEVC + HEVC | 138.4 fps |

Headless, the bridge costs nothing either. A C harness rebuilt from the DESIGN bridge rules (cue pipeline → appsink
answering the allocation query with video meta and a pool hint; a C thread per layer pulls, shallow-copies and pushes
into the wall's appsrc; layers attach after preroll): H.264 80.0 fps, HEVC 207.8–216.6, H.264 + HEVC 70.9–72.5, every
frame pulled and pushed. Two more bridge rules found: the cue's appsink must ask for `video/x-raw(memory:DMABuf)`
(accepting anything, the HEVC decoder hands over SAND-tiled frames in system memory, which `glupload` refuses), and a
layer's appsrc caps come from the cue's preroll sample.

**On HDMI (real time, `glimagesink` GBM, service stopped for the test):** `glimagesink` presents with legacy page
flips (`drm_mode_page_flip_ioctl`, one per presented frame; no atomic commits), so flips count presented frames.

| Layers, through the bridge | Mixer output | Flips (presented) |
|---|---|---|
| HEVC | 57.8 fps | 60.0/s |
| HEVC + HEVC | 57.0 | 58.8/s (longest gap 33 ms) |
| H.264 | 55–56 | 18.7–22.3/s (gaps up to 0.9 s) |
| H.264 + HEVC | 53–54 | 1.3–1.7/s |
| H.264, sink drops nothing late (`qos=false max-lateness=-1`) | 42.5 | 42.7/s (every mixed frame shown) |
| H.264 + HEVC, sink drops nothing late | 36.7 | 37.2/s |

Without the bridge or the mixer, H.264 → `glupload` → `glimagesink` presents 59.8–60.0/s, with or without a
`glcolorconvert` pass, and so does HEVC. Pushing cue frames ahead (unsynced appsink, bounded appsrc) instead of in real
time, and a mixer latency of 50 or 150 ms, change nothing. So on the display the mixer pass for an H.264 layer takes
about 23 ms (42 fps) where it took 12.5 ms headless, and the late frames are dropped by the sink; HEVC layers are not
affected.

Isolation runs (all with `GST_GL_API=gles2`, which the first HDMI runs lacked; H.264 through the bridge unless noted):

| Run | Result |
|---|---|
| Real time, surfaceless context, live black source, mixer → `fakesink sync=true` | 58.5 fps mixed |
| Display (GBM) context, same chain → `fakesink sync=true` (nothing presented) | 58.5 fps mixed |
| Display, mixer → `glimagesink`, sink drops nothing late | 43.1 fps mixed, 44.6 flips/s |
| Display, mixer → `glimagesink` (default) | 50.2 fps mixed, 29.8 flips/s, gaps up to 1 s |
| H.264 → `glupload` → `glimagesink` (no bridge, no mixer) | 60.0 flips/s |
| `avdec_h264` → `glupload` → `glimagesink`, no late drop | 25.4 flips/s |
| Hardware H.264 → system-memory NV12 → `glupload` → `glimagesink`, no late drop | 12.6 flips/s |

So the GBM context is not slower and real-time pacing is not the problem: the loss appears only when presenting is added
to a GL chain that also uploads and mixes. Most likely cause (not yet confirmed): GL elements that share a context run
on its single thread, so `glimagesink`'s present, which waits for the next refresh, blocks upload and mixing for that
time. HEVC's per-frame GPU work (about 5 ms) still fits in a refresh, H.264's (about 12 ms) does not, and
system-memory uploads (12–20 ms) fit even less. Earlier wording that the mixer pass "takes about 23 ms" on the display
was an inference; what is measured is that the whole chain manages 42.5–43 fps.

**Cause confirmed.** During the failing HDMI run (H.264 through bridge and mixer into `glimagesink`, no late drop) the
single `gstglcontext` thread was sampled 100 times (`/proc/<pid>/task/<tid>/wchan` and `syscall`): 34–63 % in
`poll_schedule_timeout` (ppoll: the page-flip wait) and about 27 % in `drm_syncobj_array_wait_timeout` (waiting for the
GPU). For HEVC: 68–86 % in the flip wait, no GPU waits (it has time to spare). The present's wait for the next refresh
and the H.264 layer's GPU work run one after the other on one thread and do not fit in a refresh.

**Splitting the present off fails with stock elements:** a presenter pipeline given the wall's GL display and context
(`glimagesink` then makes its own context and thread) crashes in `gbm_surface_lock_front_buffer`: GStreamer's GBM window
does not support a second presenting context on the same display. `gldownload` can export the mixer's output as a
DMABuf, but only as `AB24:0x0700000000000006` (Broadcom UIF tiling, not scan-out-able); asking for linear AB24
negotiates and then logs "DMABuf export didn't work. Falling back to system memory" (a CPU readback).

**Our own presenter is possible** (`tools/gpu-wall/lineartarget.c`): a 1920×1080 dumb buffer created on the HDMI card
(no DRM master needed; the service kept running), exported as a DMABuf and imported on the render node as an AB24
linear EGLImage, is a complete framebuffer for V3D. A clear reads back exactly through the dumb buffer's mapping (64,
128, 191, 255), a textured draw lands correctly, and 600 full-screen 1080p draws take 4.95 ms each on average (worst
8.89 ms, 202 fps) with a `glFinish` per frame. DESIGN §6.1.1 "Display" revised accordingly: the mixer's output goes
into a ring of linear dumb buffers and the service presents them on a KMS plane from its own thread.

**Our presenter on HDMI (`bridgebench -kms`: mixer → one GPU copy pass into a ring of three linear dumb buffers,
native fence per frame, page flips from a presenter thread; service stopped for the runs):** HEVC + HEVC 57.8
flips/s; H.264 45.2; H.264 + HEVC 31.0; MPEG-2 via the ISP 16.4. The cue pipelines are back-pressured, not dropping
(H.264 pulled 396 of 480 frames in 9.3 s): the chain is bound by GPU work per frame, and the extra copy pass costs
about 5 ms on top of the mix. Pool hints, queue depth, mixer latency and `glFinish` vs fence change nothing.

**The fresh-buffer mode (the real cause of most of the above).** The slow runs are bimodal and sticky: the same
command gives 58.5 fps or 15 fps, and a slow run stays slow. The kernel function profiler shows why: in a slow run
`v3d_create_bo_ioctl` is called 350 times in 147 frames (8.7 s, 25 ms each, nearly all in page allocation with direct
reclaim), against 28 times in a whole fast run; the allocations are 1080p RGBA buffers (8,298,496 bytes, caught with
a gdb breakpoint on the ioctl). The GL buffer pools reuse their buffers (6 allocations per run, GStreamer's own
pool trace), so it is the GL driver (Mesa 26.2.2, v3d) allocating a fresh backing buffer per frame for a texture it is
asked to render into while the GPU is still busy with it — and each fresh allocation keeps the GPU behind, so the mode
feeds itself. It is triggered by load: with the wall alone (black source → mixer → sink, no cue) a 3 s burst of
competing GPU work at start-up leaves it allocating 69–181 buffers per 240 frames for the rest of the run, against 0
in steady state; `V3D_DEBUG=always_flush` does not prevent it (223). It never self-recovers within a run. The
trigger explains the ISP route's bimodality, the H.264-on-HDMI losses (the display pass is the extra load) and the
"pool size" sensitivity (bigger pools shift the timing).

**Rendering into imported buffers is immune**: the same test drawing 600 frames into three linear dumb buffers
imported as EGLImages (`lineartarget -loop`), with the same 3 s load burst, allocates 0 buffers in the window (0
without load). The driver cannot replace an imported buffer, so the trap cannot form. Consequence for the design: the
mixer must render **straight into the ring** (no driver-owned 1080p render target in the per-frame path), and the
wall's pacing source must not be a 1080p driver-owned texture either (a 16×16 live black source scaled by the mixer
pad is enough: 38 allocations per run, all at start-up). DESIGN §6.1.1 updated.

Other findings from this round: the ISP route needs a pool hint of 32 buffers from the bridge (the V4L2 maximum;
8 → 15 fps, 32 → 41 fps even in the slow mode); `-copy` vs ref-and-make-writable in the pump makes no difference
(`gst_buffer_copy` refs whole memories); the memory objects are reused (13 distinct per run); the upload texture
target (external-OES vs 2D) is not the switch; the kernel log shows one V3D MMU fault during the HDMI runs and CMA
allocation failures for 3 MB frames earlier in the day (fragmentation; relevant to the ring buffers, which are CMA).

**The mixer rendering straight into the ring (`bridgebench -ring -tiny`, 2026-10-02).** A `GstGLBufferPool`
subclass hands the mixer buffers that wrap the four ring textures (EGLImages over linear dumb buffers), offered in the
mixer's allocation query from a pad probe on its src pad; the wall appsink holds each mixed buffer until it has left the
screen (one on screen, one in flight), a native fence per frame from the GL thread, page flips from the presenter
thread; the pacing source is a 16×16 live black frame scaled by its mixer pad. On HDMI, service stopped for the runs:

| Layers | Presented (flips/s over 5 s) | Refreshes over 20 ms |
|---|---|---|
| H.264 | **60.0** | 2 (longest 24 ms) |
| H.264 + HEVC | **60.0** | 1 (longest 22 ms) |
| HEVC + HEVC | **60.0** | 0 (longest 19 ms) |

Every cue frame pulled and pushed (480 of 480). The H.264-on-HDMI problem is solved: no copy pass, no driver-owned
render target, so the GPU work per frame is the mix alone. Two things found on the way: the mixer's allocation query
runs more than once (a layer attaching renegotiates), so the pool must be offered idempotently and reuse ring slots
freed with its buffers (the first version made a second pool, orphaned the first's textures and stalled H.264 after
two frames); and the mixer samples the hardware decoders' frames as external-OES textures directly (`glcolorconvert`
passes them through), which is why H.264 costs the mix alone now.

**Software video through the ISP: solved (2026-10-02).** Three things had to be right at once, and the harness's
own per-buffer checks found them where debug tracing could not (any `glupload` debug level changes the negotiation
and sends even H.264 down the 2D path):
- *YU12, not NV12.* The GL driver imports only `YU12` directly as an external-OES texture ("driver only supports
  external import of fourcc YU12"); NV12 and RGB from the ISP arrive as 2D textures, which cost the GL thread about
  23 ms of user CPU per frame (Mesa copies a linear 2D import into its tiled layout) plus a conversion pass. The ISP
  writes YU12 as readily as NV12.
- *A colour matrix on the layer caps.* The ISP's caps carry no colorimetry; `glcolorconvert` refuses an external-OES
  YUV input without one ("Need to specify a color matrix"), so a layer stalls after one frame. The bridge sets
  `colorimetry=bt709` on the layer's appsrc caps when the source has none (the H.264 decoder's caps say bt709).
- *A pool hint below 32.* With 32 capture buffers requested, `v4l2convert` silently copies every frame into system
  memory (353 of 353 pushed buffers were SystemMemory, 3,110,400 bytes each) and the whole route goes CPU-side;
  8–31 all export DMABufs. The earlier "8 → 15 fps" was the NV12 path's allocation storm, not starvation. 16 is used.

On HDMI through the ring wall (service stopped for the runs):

| Layers | Presented (flips/s) | Refreshes over 20 ms | Cue frames delivered |
|---|---|---|---|
| MPEG-2 via ISP | **60.0** | 0 | 480 of 480 |
| HEVC + MPEG-2 via ISP | **60.0** | 0 | 480 + 480 |
| VP9 (software decode) via ISP | **59.8** | 2 | 480 of 480 |
| DNxHR HQ (`videoconvert` to I420) via ISP | **60.0** | 2 | 480 of 480 |
| ProRes 422 (`videoconvert` to I420) via ISP | 60.0 | 42 | 318 (decode + repack bound, ~35 fps) |
| H.264 + MPEG-2 via ISP | 59.8 | 31 | 480 + 382 |
| MPEG-2 + VP9 via ISP (two software layers) | 60.0 | 8 | 477 + 367 (CPU-bound) |

The wall itself holds 60 in every case; what falls short is a cue's own decode rate (ProRes 422, two software
decoders at once) or, beside an H.264 layer, the MPEG-2 cue (382 frames, 42 fps) — the H.264 decoder and the ISP
share the VideoCore (HEVC decodes on its own block and the same pair with HEVC is clean).

**Load-burst immunity on the real path:** H.264 and H.264 + HEVC with a 3 s burst of competing GPU work at start-up,
counted after the burst: no refresh over 20 ms, longest gap 17 ms (the flips/s figures of 57.0 and 54.8 are
under-counts, the window ran past the end of the clip). The wall returns to a solid 60 by itself.

Pinning the decoder to NV12 or YU12 DMA_DRM caps fails at runtime (no frames), and is not needed.

## SD card and decode-path measurements (2026-10-02)

Test server, service idle. Card: SanDisk SN256 (256 GB, 07/2024), ext4 `noatime`, running **UHS DDR50** (50 MHz,
4-bit, 1.8 V): the fastest mode the Pi 4's SD host supports, about 50 MB/s at the bus.

| Read | Result |
|---|---|
| Raw sequential, 1 GiB, `O_DIRECT`, at 2, 60 and 150 GiB | 43.8, 46.0, 46.0 MB/s |
| Raw random 4 KiB | 2357 IOPS, 9.7 MB/s, 0.42 ms each |
| Raw random 64 KiB | 562 IOPS, 36.8 MB/s, 1.78 ms each |
| Raw random 1 MiB | 42 IOPS, 44.2 MB/s, 23.7 ms each |
| Files, cold cache (ProRes 4444 alpha 614 MB, qtrle alpha 240 MB, DNxHR HQ 441 MB) | 44.8, 44.6, 44.6 MB/s |
| The same files from the page cache (RAM) | 1.1–1.3 GB/s |

Read rate each support-set file needs at 1× (size ÷ duration): ProRes 4444 alpha 76.8 MB/s and DNxHR HQ 55.1 MB/s
exceed the card; CineForm RGBA/alpha 42.8 and RGB 38.3, ProRes 4444/XQ 35–36 and qtrle alpha 29.9 sit near or under
it; the median file needs 11.4 MB/s. Two layers at once (a crossfade) need the sum.

Decode speed with the file already in RAM (480 frames of 1080p60, `decodebin ! fakesink sync=false`), against what
the KMS wall shows on screen:

| File | Decode only | Decode + `videoconvert` to a plane format, 1 / 4 threads | On screen (KMS, steady) |
|---|---|---|---|
| DNxHR HQ (MOV) | 121 fps | 43.9 / 71.7 | 13.7 |
| VP9 (MKV) | 102.7 | 102.0 / 102.2 | 35.1 |
| qtrle alpha (MOV) | 94.5 | – | 2.7–42.8 (card-bound) |
| HAP alpha (MOV) | 73.1 | – | 18.1 |
| ProRes 422 (MOV) | 71.2 | 24.0 / 40.4 | 0.8 |
| H.264 High 10 (MOV) | 64.1 | 45.2 / 47.5 | 0.3–0.5 |
| ProRes 4444 alpha (MOV) | 35.6 | – | 0.3–0.5 |
| CineForm alpha (MOV) | 7.8 | – | 0 |

**Findings.**
- The card limits only files above about 45 MB/s (ProRes 4444/XQ with alpha, DNxHR HQ) and, with two layers, the
  sum of both. For most codecs the card is not the limit.
- Most software codecs decode well above 60 fps. What they lose is after the decoder: the colour conversion to a
  plane format on the CPU (single-threaded by default; 4 threads gives ProRes 422 24 → 40 fps, DNxHR 44 → 72), then
  the copy into the display buffer and the shared 60 commits a second. A clip that falls behind its clock has its
  late frames dropped and does not recover (ProRes 422: 24 fps possible, 0.8 shown).
- CineForm (7.8 fps) and ProRes 4444 (35.6 fps) are decode-bound on the Pi 4's CPU even from RAM.

## GPU wall in the service: steps 1 and 2 (2026-10-03)

`CUTEPI_WALL=gl` (the KMS plane wall stays the default). The wall from the harness now runs inside the service
(`gsp/glwall`, cgo): the GL mixer pipeline on the render node, the ring of four dumb buffers on the service's own
DRM fd offered to the mixer as its pool, a presenter thread putting each fenced frame on the lowest overlay plane with
`SetPlane` (the service stays DRM master; the panic plane logic is replaced by a mixer layer at the top), and the
layer bridge. Measured through the live service and its API, `/api/debug/glwall` giving the wall's and each layer's
counters:

| Step 1 (wall alone) | 300 `SetPlane` commits in 5 s traced from outside: black at 60 fps; HTTP serving. |
|---|---|

| Step 2, cue layers (frames pushed to the mixer per second; the wall presented 60 throughout) | |
|---|---|
| HEVC (`v4l2slh265dec`, DMABuf) | 60 |
| H.264 (`v4l2h264dec`, DMABuf) | 60 |
| DNxHR HQ (software, `videoconvert` → ISP → YU12 DMABuf) | 60 |
| MPEG-2, VP9 (software → ISP) | 60 |
| Animated GIF (25 fps file, via ISP) | 25 (its own rate) |
| PNG with alpha (still) | 1 frame, held |
| Pause / resume | frames stop, then resume |
| Stop, then Play (resume after stop) | layer re-attached after the new preroll |
| Crossfade H.264 → HEVC with a 1 s fade-in | incoming layer level 0 → 1, 56 frames in the first second |
| ESC fade-out | layer gone when the fade ends |
| Panic to the armed holding image | layer on top at full level within the second |

Found on the way:
- decodebin exposes a stateless V4L2 decoder's pad (HEVC) with its system-memory tiled caps (`NV12_128C8`) before
  anything downstream exists and never renegotiates: a DMABuf-only tail then fails to preroll, and the "software"
  tail untiles on the CPU (12 fps). A reconfigure event does not change that. **GL mode builds cues with decodebin3**,
  which plugs the decoder against the real downstream; every route then negotiates as in the harness.
- decodebin3 adds its pads *after* the pipeline reports PAUSED, so the tail (and the appsink's preroll) can arrive
  after `startPlayback`'s wait: `showLayer` waits for the tail record and then for the preroll (10 s), and the
  record is registered only once the tail is in the pipeline and synced (an appsink still in NULL answers the
  preroll pull with nothing: the first version raced and attached nothing).
- The tail is chosen by what the decoder *can* produce (a caps query: memory:DMABuf listed or not), not by the pad's
  current caps.
- Stop keeps the pipeline for a resume, so it hides the layer (frees the wall side, keeps the record) and the resume
  re-attaches.
- A still through decodebin3 is pushed repeatedly (30 frames a second of the same picture): harmless, to trim.

**A hang found after the step 2 commit, and fixed (2026-10-03).** Repeated play/stop cycles through the API hung
the service after a few rounds: `Stop` blocked in `glwall_layer_free`, the mixer's output thread waited forever in
`gst_buffer_pool_acquire_buffer` with every ring buffer counted out, while the pull and presenter threads were idle and
held nothing. Cause: the presenter kept each frame's sample in a per-slot field (`ring[idx].sample`); a new mixed frame
in the same ring slot overwrote it while the old one was still queued, so that reference was never released and the
buffer never returned to the pool. Each such overwrite lost a slot until none were left. Fix (`gsp/glwall/glwall.c`):
- frames travel to the presenter as items that own their sample and their fence, so nothing per slot is ever
  overwritten or closed twice;
- the ring pool preallocates nothing (min 0, max 8, forced in its own `set_config`), and each slot's state is
  tracked: wrapped by a live buffer, or on screen; the slot on screen is never handed to the mixer until another frame
  has replaced it; allocation waits up to 200 ms for a free slot and reports exhaustion instead of failing silently;
- the layer is detached from the mixer before its chain is stopped;
- the presenter skips to the newest finished frame when it is behind: a standing queue had formed (6 frames, about
  100 ms of picture delay against the sound) and never drained;
- `/api/debug/glwall` now reports the pool: allocations, frees, exhaustions, configs, activations, allocation queries
  answered, slots allocated, the slot on screen, frames queued and frames skipped.

Soak (service with `CUTEPI_WALL=gl`): 12 rounds, then 10 more after the catch-up change, each round four play/stop
runs (H.264 twice, HEVC, MPEG-2 via the ISP, one with the API polled every 30 ms) and a crossfade H.264 → HEVC with a
1 s fade-in. Every run pushed 59–60 frames a second, crossfades 60; no hang; 0 exhaustions; 193 allocation queries
answered (16 per round: the mixer re-asks on every attach) with the pool configured and activated only once; queue
depth 0–1 (3 at most). Open: the presenter skips about two mixed frames per cue start (and 67 at the first play after
the service starts) while the new layer's first frames settle; measure whether a video frame is lost there.

`support.py` on the GL wall (`--section gl`, wall detected from `/api/debug/glwall`) samples the newest visible layer's
counters instead of tracing planes. Its opacity-step count is not valid yet: it counts calls that set a layer's alpha
(about 100–120 a second, two writers), not changes the viewer sees, which wrongly marked a PNG Supported in a smoke run.
It must count, per mixed frame, whether the newest layer's alpha differs from the previous frame's, capped at the frames
presented. No GL batch has been written to the README.

Not yet on the GL wall: rotation and mirror (`glvideoflip`), crop, the warm preroll (disabled in GL mode: cold builds
are 1–3 ms + preroll), the audio offset for the wall's latency, alpha detection for sources whose format is unknown at
pad-added time under decodebin3 (they go through the ISP, which drops alpha; use the import metadata), test
patterns (they take the ISP route and should work; not measured), and `support.py` measuring on the GL wall (count a
layer's frames per window from `/api/debug/glwall` instead of plane commits).

## Codec support round 2: straight alpha, animated images, fade pacing (2026-10-02)

Pi 4 Model B Rev 1.5, KMS planes, live service; the full support set (94 files) rerun, then the slow-clip rows rerun
with nothing else running. README table refreshed.

| Change | Before | After |
|---|---|---|
| **Straight-alpha blending**: every wall layer gets `pixel blend mode` = Coverage when it claims its plane (`newWallLayer`, `blendCoverage`) | all alpha files blended as Pre-multiplied (edges too bright) | every alpha file that imports: plane AB24/AR24, blend **Coverage** |
| **Animated images as timelines**: `media.ImageAnimation` reads the header (GIF second image descriptor, APNG acTL, WebP VP8X flag, loop count); `gsp.isStillFile` excludes animated images; image cues of an animated file hold their last frame; direct pool playback loops as the file says (`gsp.DirectOpts`); import records the animation length as the media duration | GIF treated as a still (extension only) | 25 fps GIF: 25.1 fps steady, 25–26 fps through both fades, with and without transparency |
| **Fade pacing** (`wallLayer.writer`): the step grid counts from the start of each alpha write instead of sleeping two refreshes after the blocking commit; a layer showing a single-frame image, or a clip with no new frame for 0.5 s, steps every refresh; fade loops post every 8 ms (`fadeTick`), and fade teardown waits `alphaLand` (64 ms) for the last alpha | stills ~20 steps/s; video ~20 steps/s | stills **56–58 steps/s**; video and animated images **28–31 steps/s** (the designed 30) |

**What it costs.** A 60 fps clip now shows about 31 fps while it fades (was 39–43), because opacity steps 30 times a
second instead of 20: the CRTC takes 60 commits a second, shared by frames and alpha writes. Fade smoothness was
kept over frames, per the output-quality rule (coarser fades were rejected). Steady play is unchanged: H.264 57–59.8,
MJPEG/MPEG-2/MPEG-4/VP8/WMV2 58.6–60, FFV1 58.

**A regression found and fixed in this round.** The first version judged "moving" by a new frame within four
refreshes. A slow decoder (DNxHR at about 10 fps) looked still, so its fade-in stepped every refresh and took every
commit; the clip fell behind its clock, the sink dropped its late frames, and it never recovered (DNxHR HQ 10.7 →
0.3 fps). Now only a single-frame image (`still`, set when the layer is built) or a clip idle for 0.5 s steps every
refresh. After the fix, measured with nothing else running: DNxHR HQ 13.7, DNxHR LB 12.6, VP9 35.1, HAP 22.7, HAP
Alpha 18.1, Theora 44.3, qtrle 20.1 fps (all at or above the first batch).

**Measurement notes.**
- Running a Go build or test on the Pi during a measurement costs software-decoded clips frames (VP9 measured 26–29
  fps that way, 35 quiet). Measurements are now taken with nothing else running.
- qtrle with alpha (240 MB for 8 s, about 30 MB/s) is bound by reading the SD card: 2.7, 14.2, 6.7 and 42.8 fps on
  consecutive runs, highest once the file sat in the page cache.
- Stills reach 56–58 opacity steps a second, under the 59 criterion. A per-refresh trace of one still's fade: 113 of
  about 120 refreshes got a new opacity; the rest are the ~3 refreshes before the fade-in's first write (the picture
  is already on the plane at opacity 0, invisible) and 4 single-refresh gaps mid-fade (writer thread scheduling). The
  GPU wall applies opacity on every output frame.
- The 50 fps GIF stays decode-bound (0.3–0.5 fps on screen).

**Still not Supported:** no video or image meets the 1080p60 + smooth-fade criterion on the KMS wall; the 17
supported rows are audio. That is what the GPU compositor wall (DESIGN §6.1.1) is for.

**Tests.** `media/animated_test.go` (still and animated GIF, PNG/APNG, WebP; loop counts) and the full Go suite pass.

## Round 5 — Companion compatibility (2026-09-30)

Target: the Companion instance at companion.drevilish.com (v5.0.4), with the connections **CuTePi-Hyperdeck** (bmd-hyperdeck 3.1.1, model HyperDeck Studio Mini) and **CuTePi-QLab** (figure53-qlab-advance 2.14.1, TCP 53000). Only CuTePi was changed. Conformance was checked three ways:
- Go tests replay each module's connect sequence over a real socket.
- The dev server replays it with the modules' own client libraries (hyperdeck-connection 3.1.0, osc.js 2.4) against the test server, including playback.
- The real Companion's status and variables were read through its HTTP API while CuTePi played.

| # | Issue | Root cause (verified) | Fix | Verified |
|---|---|---|---|---|
| C1 | HyperDeck connection failed every 5 s | Remote listeners were off. Once on, the library's first command after the greeting (`watchdog: period: 6`) answered `100 unknown command`, so the library drops the connection | `watchdog` implemented (idle clients closed after the period plus 2 s) | Library connects and stays up through pings (14 s hold) |
| C2 | Multi-line commands misread | The library sends `notify:` followed by param lines and a blank line. Each line was run as its own command | Multi-line command reader | `notify` set: 200, subscription applied |
| C3 | Connect reads answered with wrong codes | `device info` answered 201 (library wants 204 with `slot count`); `remote` 200 (wants 210); no `slot info` (202) or `configuration` (211). Any mismatch aborts the module's init | Studio Mini replies for all of them; greeting model "HyperDeck Studio Mini", protocol 1.11 | Library init sequence completes; module auto-selects Studio Mini |
| C4 | No transport feedback in Companion | Pushes went out as `500 transport info` (500 is the connect banner, which the library ignores). Transport notifications are 508 | 508 transport, 502 slot (clip list changed), 510 remote, 511 configuration, 513 display timecode; reply always before its notification | Companion `status/speed/clipId/clipName` follow play, pause and stop |
| C5 | Clip list out of order in Companion | Clip ids were cue positions. The library keys clips by id, so they come out sorted by id, not in play order (this sheet plays 1, 3, 4, 2, 5, 6) | Clip ids are 1…N in play order, as on a deck's timeline | Clips listed in sheet order; `goto clip 2` selects the sheet's 2nd row |
| C6 | Companion Play reset a cue's programmed rate | Play sends `speed: 100`, which set the rate to 1.0 on a 0.45× cue | Speed is a percentage of the cue's own rate | Timecode advances at 0.45×, reported speed 100 |
| C7 | `loop: true` shown while idle; Companion Play turned the saved loop default off | `Loop()` reports the last pipeline after stop. The deck `loop:` flag persisted to config | Loop reported only while playing; deck loop is per clip (`SetClipLoop`), and only `true` is applied | Idle `loop: false`; config untouched |
| C8 | Momentary "clip none, speed 45" at every deck play | The cue position was attached after the load bumped the state | `LoadOpts.CuePos` installs it with the pipeline; pushes debounced one tick | First push already carries clip 2, speed 100 |
| C9 | QLab module stuck in "No Workspaces"/Error | CuTePi sent no replies at all over TCP | QLab 5 workspace emulation (`routes/qlabws.go`): replies for the full handshake, cue list with every key the module reads, playhead, running cue, and `/update` pushes | Module status OK; `r_name/r_stat/e_secs/r_left` follow playback; `n_name` follows the selection |
| C10 | QLab module threw `Cannot read properties of undefined (reading 'pctElapsed')` | Module bug: it dereferences its old copy of a paused cue while first loading the list. A held still was reported paused | Held-at-end clips report running (`gsp.HeldAtEnd`); lists never carry `isPaused` (the next `valuesForKeys` does) | No module errors in the Companion log since |
| C11 | Each GO pushed an update for every cue, twice | The cue-sheet version also moves on selection and last-played stamps | Only cues whose shown content changed are pushed; the list only on add/remove/reorder | GO → only the running cue and the playhead |
| C12 | QLab elapsed/percent wrong for trimmed cues | Measured from the file start | Measured through the trimmed span, in cue time | 0 % at the in-point, rising at the cue's rate |

Not changed:
- One HyperDeck client at a time, as designed (§12.8). A second controller gets `120`; Companion retries every 5 s.
- The module's "play range" read sends `device info` (a library bug) and gets rejected. The module tolerates this.
- The race detector can't run on the Pi: ThreadSanitizer doesn't support its 39-bit address space.

## Round 3 — playback engine and web UI (2026-09-30)

### New video output: one hardware layer per cue

The test Pi (Raspberry Pi 4, HDMI at 1920×1080 60 Hz) now shows every cue on its own display plane (`kmssink` on a
shared DRM handle, the service as DRM master). The display hardware stacks and blends the layers. Measured on the wall
by reading the kernel's plane state (frames = `FB_ID` changes):

| Check | Before | Now |
|---|---|---|
| 1080p30 film, steady | ~16 fps | **30.0 fps** |
| 1 s ESC fade | freezes 1.4 s, then cuts | **1.0 s smooth fade, film holds 30 fps** |
| Colour during fades | hue shifts (brightness offset) | alpha over black: colours scale evenly |
| Full-colour image fade | banded left-to-right, then cut | smooth plane fade |
| Crossfade (fade and stop others) | hard cut, or fade to black then start | new cue under, old fades over it: 2.0 s, 28–30 fps |
| Rotate 90°/270° + mirror | not applied at all | portrait 606×1080, **30 fps** |
| Rotate 180°, mirror | not applied | display hardware, 30 fps |
| Opacity 50 %, box 50 % at (10 %, 10 %) | n/a | alpha 32768, 960×540 at (192,108), 30 fps |
| Test pattern SMPTE | 320×240 in a corner | full screen, 8 % CPU |
| Test pattern Blink | — | alternates at the display's 59.9 Hz |

### Deviations found and fixed

| # | Issue | Root cause (verified) | Fix |
|---|---|---|---|
| D23 | Console text and cursor on HDMI after start; after a panic the wall showed the console | The active VT stayed in text mode: fbcon draws login prompt, kernel messages and cursor | `ClaimWallConsole`: VT to graphics mode at startup, framebuffer blanked; text mode restored on clean exit. Verified: 0 non-zero framebuffer bytes after writing to `tty1` (25,848 with the service stopped) |
| D24 | Fade and stop did not fade | `videobalance` on DMA buffers: about 2 fps once brightness ≠ 0, and each set blocked ~450 ms | Hardware plane alpha (above); fbdev fallback got an async brightness writer |
| D25 | Full-colour images fade badly, and fades shift colour | Still re-rendered per step and interrupted mid-paint; brightness offset instead of a scale | Plane alpha |
| D26 | Test patterns never change | `videotestsrc` `pattern` set with a Go string, which GObject silently ignores for an enum | `SetArg`. Verified: Red, Green and Blue read (255,0,0), (0,250,0), (0,0,255) |
| D27 | Test patterns not full screen | No caps: default 320×240 | Generated at the display's size and rate |
| D28 | Rotate and mirror do nothing | Same enum bug on `videoflip` `method`; a prewarmed cue also never got its geometry | `SetArg`; geometry from the cue's own options at build time |
| D29 | Fade and stop others hard-cut | The GO path ignored `fadeOut`; the other path faded to black, then started | Crossfade on every path (§6.5 of DESIGN.md) |
| D30 | Slideshow doesn't advance, wrong timing, fades and shuffle | Member cue timers ended slides early and the runner quit; fade to black plus sleep; duplicate runners; shuffle only once | Runner rewritten: group owns timing, crossfades, one run at a time, reshuffle per pass. Verified: 2.0 s cadence, loops, Stop and a second start cancel it |
| D31 | Header Pause did nothing | The NOW bar was replaced every second, so a click straddling the swap was lost; paused state was invisible | Bar updates in place and holds refreshes while pressed; PAUSED state shown. Verified 6/6 slow presses |
| D32 | Theme selector on every Settings tab | The first tab lacked Bootstrap's `active` class, so its pane was never hidden | Class added |
| D33 | Your 10 s Fade In entry was refused | Parser only took `1:05.000` or seconds | Unit forms (`1m5s`, `10s`, `500ms`, …); every number field is validated text |
| D34 | Stop left "test showing" set | `Stop()` kept the stopped pipeline and did not clear the flag | Cleared |
| D35 | A tool opening the DRM device first broke playback | Plane writes need DRM master, taken lazily | The wall is opened at startup with an explicit `SET_MASTER` |
| D36 | gsp test hang (the old O8) | Tests used a real display sink and fought the service for HDMI | Package tests default to `fakesink` |

### Also done
- Cue number step setting (default 1); right-click and menu **Sort by cue number** and **Renumber cues**.
- Test-pattern resolution/frame-rate label (remembered); unmistakable Tests on/off state on every client; the live
  pattern is marked in the picker.
- Opacity and position/size (px or %) in the Video tab, carried through show export/import.
- Windows 95 / XP / 7 themes draw modals as windows.
- DESIGN.md: §2, §5.1, §5.2, §5.4, §5.5, §6.1, §6.4, §6.5, §6.9, §12.5, §12.10, and the A/V sync test signal (§12.11,
  later phase). AGENTS.md: how to measure video on planes; DRM master rule.

### Suites
p9 10/10, p11 3/3, p13 6/6. p6b 16/17 and p7 13/14: the two failures are my test thresholds (request bound during a GO
burst; the new "Nothing uploaded" wording), not behaviour. Go: every package passes.

### Open / follow-up
- **A 60 fps clip loses frames while it fades.** Every alpha write is its own display commit, and so is every frame
  `kmssink` shows. A small in-app compositor that commits frame and alpha together would remove this; that means
  writing our own sink.
- **Panic holding image appears ~300 ms after the cut**, black in between. Keeping it prerolled on an invisible layer
  would make it instant.
- **Background refresh traffic (O2):** 4 requests per second per client while playing.

## Open findings (not fixed; need a decision)

### O1 — Frame rate: 1080p60 plays at 60 fps; fades, two layers and HEVC do not (major)

**Status (2026-09-30, round 6):** the KMS wall (one display plane per cue, Round 3) fixed steady playback. A 1080p30
film plays at 30.0 fps and a 1080p60 H.264 clip plays at **60.0 fps with no missed refresh**. The §2 requirement is
still not met in three cases, each with a measured cause:
- a fade on a 60 fps clip;
- two video layers on screen at once;
- HEVC files.

No code was changed in this round. The one fix attempted (below) cannot work with this GStreamer version, and it was
reverted.

History: on `fbdevsink` a 1080p30 film played at ~16 fps. The causes were a software colour convert/scale/flip chain,
`videobalance` on uncached DMA buffers (~2 fps during fades) and the framebuffer copy. The plane-per-cue `kmssink`
wall removed all three (Round 3).

#### Results

The test clips were generated on the Pi with `ffmpeg testsrc2` plus a frame counter and a tone, and uploaded through
`/upload`. They were played as cues through the real service (`POST /api/cue/N/play`, inspector for opacity, fade and
rate). The display ran 1920×1080 at 60 Hz. "fps" means new frames that reached the screen per second, measured per
plane (method below).

| Clip (H.264 High unless noted) | Planes | Case | fps per plane | Refreshes without a new frame |
|---|---|---|---|---|
| bbb 1080p30 (Round 3, fbdev → kms) | 1 | steady | ~16 → **30.0** | — |
| 1080p60 | 1 | steady, 3 × 10 s | **60.0 / 60.0 / 60.0** | 0 of 599 each |
| 720p60 | 1 | steady | **60.0** | 0 of 599 |
| 1080p60 | 1 | 8 s fade-in | **42.2** | 146 of 492 (147 alpha writes) |
| 1080p60 | 1 | 6 s ESC fade | **43.5** | 102 of 372 (103 alpha writes) |
| 720p60 A + 720p60 B, both 50 % | 2 | 30 s crossfade, first 10 s | **26.9 + 26.8** | 64 alpha writes; 268 + 266 + 64 = 598 commits in 600 refreshes |
| same, both cues muted | 2 | same | 26.9 + 26.8 | same (audio is not the cause) |
| 1080p60 A + B at rate 0.5 (30 fps each), 50 % | 2 | 30 s crossfade | **26.7 + 26.9** | 266 + 268 + 63 = 597 commits |
| 1080p60 A + 1080p60 B, both 50 % | 2 | 30 s crossfade, muted or not | **0.3–0.4 each** (picture frozen) | decoder-bound, see (b) |
| 1080p60 A → B, 100 % | 2 | 2 s crossfade | A: last frame 0.5 s into the fade; B: 7 frames in 3.25 s, then 60 | both pictures frozen ~3 s |
| 1080p60 **HEVC** (Main) | 1 | steady | **1.0** | streaming thread 100 % CPU, see (c) |

**Two simultaneous full-screen 1080p60 layers at 50 % opacity do not play.** Both pictures freeze (0.3–0.4 fps). The
closest working case is two 720p60 layers scaled to full screen by the display, at about 27 fps each. The display
hardware blends two 50 % planes without trouble; the limits are the decoder and the commit rate.

Hardware decoder capacity, measured with `gst-launch-1.0 filesrc ! qtdemux ! parse ! decoder ! fakesink sync=false`
on the same clips:

| Decoder | One stream | Two streams at once |
|---|---|---|
| H.264 (`v4l2h264dec`, bcm2835-codec) 1080p60 | 70.6 fps | 38.6 + 38.6 |
| H.264 720p60 | — | 66.0 + 66.0 |
| HEVC (`v4l2slh265dec`, rpi-hevc-dec) 1080p60 | 122.4 fps | 90.8 + 90.7 |
| H.264 1080p60 + HEVC 1080p60 | — | 58.6 + 48.8 |

#### Causes (verified)

- **(a) One display commit per refresh, shared by every plane and every alpha write.**
  - `kmssink` shows each frame with a legacy `SetPlane`, which is a blocking commit. The traced call returns at the
    flip: median 16.3 ms, p95 16.5 ms.
  - Each alpha write from `wall.go` is also a commit (`OBJ_SETPROPERTY`) on the same CRTC. The vc4 driver serialises
    commits per CRTC, so the wall gets 60 commits a second in total.
  - One 60 fps plane uses all 60. Every alpha write then costs one video frame: in the fade-in, 147 writes gave 146
    refreshes without a new frame. The writer posts at most one write per two refreshes (Round 3), which keeps a
    30 fps clip whole but leaves a 60 fps clip at about 42 fps during the fade.
  - Two planes split the 60 commits. The traces add up exactly: frames on plane 1 + frames on plane 2 + alpha
    writes = 597–598 commits in 600 refreshes.
- **(b) The H.264 decoder does about 70 fps of 1080p in total.** Two 1080p60 streams get about 38.6 fps each. Every
  frame then arrives late, the sink drops late frames (QoS), and the picture freezes instead of degrading. The same
  happens in a normal 2 s crossfade between two 1080p60 H.264 cues: both pictures stall for about 3 s.
- **(c) HEVC frames are untiled on the CPU.**
  - The Pi HEVC decoder only outputs the Broadcom column format `NV12_128C8` (SAND128).
  - The display planes can scan that out directly: plane `IN_FORMATS` lists NV12/NV21 with modifier
    `0x0700000000000004`.
  - But GStreamer 1.26.2 `v4l2codecs` has no DRM mapping for it: the debug log shows
    `Selected format NV12_128C8 DRM ....:0x00ffffffffffffff`. So it offers the frames only in system memory, and
    `videoconvert` untiles them in software from uncached memory, at about 1 fps.
  - Tried: linking SAND pads straight to `kmssink` (no converter). Preroll fails (not negotiated). Reverted.

#### Method

The measurements come from the kernel's ftrace, outside the service, without opening `/dev/dri`.
- **Probes:**
  - kprobes on `drm_mode_setplane` (plane, fb) and `drm_mode_obj_set_property_ioctl` (object, property, value), each
    with a matching kretprobe;
  - the `drm:drm_vblank_event` tracepoint, whose (sequence, timestamp) pairs give the refresh grid;
  - `trace_clock=mono`, the same clock as the vblank timestamps.
- **Counting:** a blocking `SetPlane` returns when its frame is latched. Each return is therefore one new frame on
  screen, and it is assigned to the refresh at its return time. For each plane the counts are frames per second,
  refreshes without a new frame, and the gaps between new frames.
- **Why not the 10 ms `FB_ID` poll:** it agrees for one plane (59.7 against 60.0). With two planes, reading a plane
  waits for that plane's lock, which each blocking commit holds. The poll then samples only every ~60 ms and
  under-reads (15.9 against 27). A 10 ms poll of a 16.7 ms signal only counts correctly while every sample interval
  stays under the frame interval, and it does not here.

#### Decision (2026-10-01): GPU compositor, option A

GStreamer's GPU mixer replaces the per-cue planes (DESIGN §6.1.1). Feasibility, measured on the Pi 4 at 1080p60
before the decision. Test clips were made on the Pi: H.264 `testsrc2` with a frame counter, HEVC `testsrc`.
The GPU ran at 500 MHz, not throttled.

| Chain | Where | Result |
|---|---|---|
| H.264 hw decode → `glupload` → `glcolorconvert` (RGBA) | headless | 95 fps (decoder-bound) |
| … → `glvideomixer` (convenience bin), 1 layer | headless | **33–41 fps**: the bin's extra conversions |
| … → `glvideomixerelement` (bare element), 1 layer | headless | **87 fps** |
| HEVC hw decode as `DMA_DRM` SAND128 → `glupload` → mixer, 1 layer | headless | **176 fps** (GPU reads the tiled frames) |
| HEVC + HEVC, both 50 % | headless | 130 fps |
| H.264 + HEVC, both 50 % | headless | 72 fps |
| HEVC ×3, all 50 % | headless | 91 fps |
| H.264 ×1 → mixer → `glimagesink` (GBM) | HDMI | 58.8 fps average, 0 dropped |
| HEVC ×1 | HDMI | 59.8 fps, 0 dropped |
| **HEVC + HEVC, both 50 %** | HDMI | **59.9 fps, 0 dropped** |
| H.264 + HEVC, both 50 % | HDMI | 55.6 average, 60.0 once running, 0 dropped |

HDMI figures come from `fpsdisplaysink` (rendered/dropped counts); build step 5 re-measures with the per-refresh
kernel trace. Dead ends:
- `gldownload` to a DMABuf for one of our own planes: V3D renders UIF-tiled buffers, which it refuses to export as
  linear, and the display controller cannot scan UIF.
- `glvideomixer` into `glimagesink` gave 1–3 fps on the display.
- `kmssink` with the HEVC SAND128 DMABuf: caps negotiate, but it describes the buffer as linear and the kernel
  refuses the framebuffer (ERANGE).

#### Earlier proposal (superseded)

A **wall compositor in the service** replaces `kmssink`:
- An appsink per cue hands its DMABuf frames to one Go goroutine.
- Once per refresh, that goroutine makes **one** non-blocking atomic commit that carries every plane's latest
  `FB_ID` together with its alpha, zpos and rectangle.
- Frames are imported with an explicit format and modifier, which covers HEVC SAND128 without GStreamer's mapping.
- This removes (a) and (c): fades and several layers at the full rate, HEVC in hardware.
- (b) is hardware. The mitigations are HEVC for layered or overlapping 1080p60 material (the separate HEVC block has
  headroom), or freezing the outgoing cue's picture during a crossfade.
- Details and estimate: DESIGN.md §6.1 "Frame-rate limits".

### O2 — Background refresh traffic during playback

While a clip plays, a second client makes about 28 HTTP requests in 2s. These are `/api/nowplaying`, `/api/cuesheet/status`, `/api/schedule/next`, `/mediapool` and `/api/audio/devices`, each triggered by a WebSocket `sync`.

1. **Causes:**
   - (a) Every sync makes each client re-fetch several fragments.
   - (b) Syncs fire on every position tick.
2. **Verification:** count `sync` messages versus requests.
3. **Fix:** carry state versions in the sync message and fetch only what changed.
4. **Related:** DESIGN §6.7 "no polling" is met when idle (0 requests).

### O3 — Same-name upload silently replaces the file

Resolved in round 2 (D21).

### O4 — `error.html` is a full page even for htmx requests

Resolved (2026-09-30). Every handler now reports failures through one helper,
`respondError` (`routes/errors.go`): htmx requests (`HX-Request` set) get the
bare message as `text/plain; charset=utf-8` with the same status, and normal
navigations still get the full `error.html` page. The clients (`ui.js`,
`dropzone.js`, `dnd.js`) show a plain-text body as-is and keep the `<pre>`
extraction as a fallback for HTML bodies. Verified with Go tests
(`routes/errors_test.go`) and with curl against the service: `POST /api/volume`
with `volume=loud` returns `400 text/plain` "volume must be a number" with
`HX-Request`, and the HTML page without it.

### O5 — The ffprobe error message leaks a temp path

Resolved (2026-09-30). `importMedia` (the shared upload, YouTube and show-import
path) now rewrites its error: the staging path and temp name (`upload-*.ext`)
become the user's file name, and `redactPaths` strips the temp, media,
thumbnail, database and working directories. The same redaction applies to
every `respondError` message, the YouTube stream's error line and the
plain-text errors from show import/export. Verified with Go tests and with curl:
uploading 2 kB of random bytes as `o5-probe-test.mp4` returns `422` "could not
import 1 of 1: "o5-probe-test.mp4": media: ffprobe failed for
"o5-probe-test.mp4": exit status 1", with no directory. Nothing was left in the
media pool or `tmp/`.

### O6 — Deleting a group with Ctrl+Backspace while its inspector is open

A harmless 404 appears in the console, and the page corrects itself.

### O7 — Panic cut to the holding image takes 300–400ms (image load)

Resolved (2026-09-30), first to under 100 ms, then to a mean under 50 ms. The
holding image is kept armed on its own display plane at alpha 0, parked at the
top zpos. A panic mutes the audio and sets that plane's alpha to full (one
display commit), then tears the rest down underneath (DESIGN §12.9).

| Measure (Pi 4, 1080p60, 10 ms plane poll) | Before | First cut (zpos + alpha) | Now (parked, alpha only) |
|---|---|---|---|
| Panic request → holding image up | 326–359 ms | 54–80 ms (8 runs) | **mean 26 ms**, median 21, max 44 (20 runs) |
| Inside the service (panic call → commit done) | — | 22–33 ms | mean 9 ms, max 28 |
| Panic request → HDMI audio stream closed | — | 69–93 ms | 20–49 ms (muted first) |

The "now" figures poll only the armed plane. The first-cut figures polled six
planes, which competed with the teardown commits and read about 30 ms late.

Checked:
- Two panics 0.5 s apart both used the armed image (re-arm about 230 ms).
- A cue started after a panic replaces the image.
- A cleared setting panics to black.
- A new image is armed within a second of the setting changing.
- A file modified on disk falls back to the cold load (477 ms) and re-arms.

### O8 — Flaky gsp test

`TestStateVersionBumpsOnPlaybackOperations` hung once. It has not reproduced.

## Questions: behaviour not specified in the .md files (answered 2026-09-29 — see Round 2)

The answers will go into DESIGN.md.

- **Q1:** What frame rate and fade quality are required for 1080p30 and 1080p60 on the Pi 4? (See O1.)
- **Q2:** The GO tooltip says "Space / Enter", but Enter does not fire GO. Which is intended?
- **Q3:** Should the inline cell editor cancel on Escape? Today it stays open and saves on blur (D4).
- **Q4:** Is there an upper bound for PreWait, PostWait and duration? `99999999` is accepted today (D5).
- **Q5:** The default ESC fade is 1000ms in §6.9, but the baseline uses 500ms. Confirm the default.
- **Q6:** Should the context-menu contents shown today be the specified ones?
  - Cue: Colour, Fade-stop others, Auto-continue, New group, Delete. §5.4 lists only colour and delete.
  - Group: Inspector, Collapse, Colour, New group, Delete group.
  - Blank sheet: New group.
  - Pool tile: Add, Refresh thumbnail, Analyse, Set as holding image, Add to test patterns, Delete (with a confirm dialog).
- **Q7:** Should these current behaviours be documented as intended?
  - F8 jumps to the next broken cue.
  - Escape closes context menus.
  - Double-clicking a pool tile adds a cue.
  - Duplicate cue numbers get " (2)" on append import.
  - Success toasts appear.
  - Invalid times return 400.
  - Show import writes the `show_imported` audit event.
  - The Settings tabs beyond §5.6.
- **Q8:** For a same-name upload, should the file be replaced (today), renamed or refused?
- **Q9:** When media is deleted, should it also be unpinned from test patterns and cleared as the holding image (today)?
- **Q10:** Show mode hides the pool, inspector and Tests button, but the transport stays live. Is that intended?

## Changes made in round 2 (uncommitted)

- `ctp/ctp.go`, `ctp/groups.go` and `ctp/export.go`: cue-number uniqueness (D20); `GetCue` no longer logs "no rows".
- `routes/api.go` and `routes/groups.go`: `rejectCueNum` (200 plus the `cueNumRejected` event).
- `routes/upload.go`: `/upload/check`, `onConflict`, `X-Upload-Result`.
- `public/src/ui.js`: conflict dialog, upload summary, cue-number tooltip, F8 removed.
- `public/src/dropzone.js`: single file picker, conflict choice, JSON errors.
- `public/src/dnd.js`: conflict choice.
- `templates/cueeditcol.html`: Escape cancels.
- `templates/mediainfo.html` and `templates/settingsModal.html`: Space only, no F8.
- `public/css/comp/dropzone.css` and `cuesheet.css`: dialog, tooltip, empty-cell target.
- Tests: `TestCueNumbersStayUnique`, `TestUploadNameConflictChoices`; the existing replace test now sends `onConflict=replace`.
- `DESIGN.md`: §2 frame rate; §5.3, §5.4, §5.6, §5.7; new §5.10 Show / Edit mode and §5.11 Feedback.

## Changes made in round 1 (uncommitted)

- `routes/origin.go`, `logs/logs.go` and `logs/logs_test.go`: the 421 now explains itself, and a new log code `RTE-E229` was added.
- `gsp/gsp.go`: `brightWorker` (D19), `blankWall` calls (D17), Hold for stills (D18a), `IsStill`.
- `gsp/blank.go` and `gsp/blank_test.go`: new.
- `routes/api.go`: shared `fadeStop` (D16), Hold on direct play (D18a).
- `routes/playback_test.go`: `TestDirectPlayImageHolds`.
- `templates/mediainfo.html`: NOW bar falls back to the filename.
- `templates/testModal.html`: the picker is rebuilt on open (D18b).
- `DESIGN.md` §6.9: holding-image notes, fade-out and blanking notes.
- `AGENTS.md`: proxy and `allowed_hosts` notes.
- **Test server config (not in git):** `allowed_hosts` now includes `cutepi-test.drevilish.com`.

## State left behind

- **Test server:** baseline restored (6 cues, group, `escFadeMs=1000`, no custom test-pattern pins). Test uploads (`t_clip.mp4`, `t_tone.wav`) and their thumbnails were removed. The service runs the latest build. The HyperDeck (9993), OSC UDP and OSC TCP (53000) listeners are **on**, so the Companion connections work; turn them off in Settings › Network if not wanted.
- **Dev server:** `/tmp/shot` harness removed. Playwright stays installed, as allowed.
