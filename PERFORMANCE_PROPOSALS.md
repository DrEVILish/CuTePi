# CuTePi performance: three proposals (2026-10-07)

Written after the performance round in TEST_REPORT ("Performance round, 2026-10-07"). Every claim marked
**measured** was measured on the test Pi 4 (Model B Rev 1.5, 1080p60 HDMI) that day. **Pi 5** statements are
inference: there is no Pi 5 to measure yet, and each proposal's first step on a Pi 5 is to measure it.

The three proposals are complementary stages, not alternatives. Each can start without the others; together they
add up to the "hardware-decoded frame goes straight to the screen, nothing in between" end state.

| | Proposal | What it attacks | Extremity |
|---|---|---|---|
| 1 | **Atomic plane engine** — one CuTePi presenter drives the display controller directly; no GPU on the frame path | the display path | high |
| 2 | **Show compiler** — every cue's media converted ahead of time to the form each Pi plays best; short clips held as decoded frames in RAM | the media | high |
| 3 | **Real-time appliance** — real-time kernel, isolated cores, overclock with active cooling, stripped read-only OS | the system | extreme |

## What the measurements say (the starting point)

| Finding | Evidence |
|---|---|
| The display controller is far from its limit. | **Measured:** one atomic commit per refresh drove **8 full-screen 1080p NV12 layers**, each with a new picture *and* a new opacity every refresh, at 60.0 commits/s, p99 16.74 ms, zero failures. The plane wall's "60 commits a second shared by every plane and fade step" (O1 (a)) is a limit of `kmssink`'s blocking legacy calls, not the hardware. |
| HEVC frames can go straight to a plane. | **Measured:** the planes accept framebuffers in the HEVC decoder's own SAND128 layout (1, 2 and 4 layers at 60). Today HEVC on the plane wall is untiled on the CPU at ~1 fps, because GStreamer cannot describe SAND128 to `kmssink` (O1 (c)). |
| Scheduling, not bandwidth, was costing H.264 on the GPU wall. | **Measured:** wall threads on SCHED_FIFO: H.264 1080p60 59.6–60.1 in 8 of 8 runs (nice -10: 56.5–58.9, 0 of 8). Adopted as the GL wall's default. |
| Software decoding is fast; moving its frames is the cost. | **Measured:** `avdec_h264` (4 threads) 106–120 fps against the hardware decoder's 73–75 fps. Software codecs are slow on the walls because their frames are copied into display/GPU memory. |
| The hardware decoders have very different headroom. | **Measured:** H.264 ~75 fps (one 1080p60 stream, ~38+38 for two); HEVC ~125 fps (90+90 for two). |
| On-device transcoding is slow on a Pi 4. | **Measured:** x265 ultrafast 1080p: ~15 fps (a 10-minute 60 fps clip ≈ 40 minutes). |
| Sound cannot be shared without a mixer. | **Measured:** the HDMI ALSA device takes one stream; `dmix` cannot produce its format. |

Your two points: the H.264 decoder **already** receives clean access units (decodebin's `h264parse`,
`alignment=au`, byte-stream). And resolution mismatch costs nothing in CuTePi's paths: the display controller scales
planes in hardware for free, and the GPU wall scales in the same shader pass that composites.

---

## Proposal 1 — Atomic plane engine (no GPU on the frame path)

**Idea.** Replace both walls' frame paths (`kmssink` per cue on the plane wall; the GL mixer, ring and presenter on
the GPU wall) with one CuTePi presenter, in C, that owns the display and issues **one atomic commit per refresh**
carrying every layer's new framebuffer, opacity, z-order, position and crop. Decoded frames are imported straight
from the decoders as framebuffers: zero copies, zero GPU passes. The display controller (HVS) does scaling,
blending, opacity, crop, 180° and mirroring in hardware.

**How each source gets on a plane:**
- H.264 (Pi 4 hardware): its DMABufs (YU12/NV12 linear) imported as framebuffers directly.
- HEVC (both Pis): imported with the SAND128 modifier ourselves (**measured:** the planes accept it), bypassing the
  GStreamer gap that costs 1 fps today.
- Software codecs (both; on the Pi 5 this includes H.264): the decoder writes into buffers allocated from the
  display's DMA heap, so the decoded frame *is* the scanout buffer. Where a decoder cannot write into foreign
  buffers, one copy into a ring of scanout buffers (still no GPU).
- Stills: one framebuffer, held; nothing is redrawn (proposal item 4 for free).
- 90/270° rotation and alpha video that needs format conversion: the GPU or the ISP renders those layers only,
  into a framebuffer the presenter puts on a plane like any other.

**Frame timing.** The presenter picks, for each layer, the newest frame due by the next vblank (pipeline clock →
vblank time), so A/V sync stays the GStreamer clock's; sound keeps its current path with the measured display delay.

**Expected, Pi 4:**
- Fades step every refresh (60 steps/s, today 30) on any layer, with no frame cost.
- Two or more video layers at full rate up to the decoders' limits (today two layers share 60 commits: ~27 fps
  each). HEVC + HEVC 1080p60 layered at 60 (decoder: 90+90).
- HEVC on planes at full rate (today ~1 fps on the plane wall).
- The H.264 decoder keeps all its memory bandwidth (no full-screen RGBA composite written and read every refresh).
- It is the natural engine for the new **Active Cues / Layer** stack: one layer = one plane, stacking = zpos, changed
  in the same commit as everything else.

**Pi 5 (inference):** same vc4 KMS driver and atomic API; the HEVC decoder outputs SAND128 too. No H.264 decoder,
so H.264 is software-decoded (the A76 cores should manage 1080p60 with room to spare; measure) into scanout buffers.

**Risks and limits:** plane count (16 on this Pi 4) bounds the layers; the HVS has a pixel-rate budget (many
downscaled layers can underrun: measure the worst show); per-pixel effects beyond opacity need the GPU path; this
replaces `kmssink` and the GL wall's presenter, so it is a large C component to own.

**Effort:** high (weeks). **First proof (days):** the `atomic_planes` test with a real decoder feeding it: H.264 +
HEVC SAND128 layers at 60 with fades, frame counts from the kernel trace.

---

## Proposal 2 — Show compiler (media in its best form, ahead of time)

**Idea.** Treat a show like a build: when media is imported (or when the operator presses "Prepare show"), convert
each cue's file into the form *this* Pi plays best, keep the original, and play the prepared copy.

- **One mezzanine per Pi:** HEVC Main 8-bit, at the display's resolution and frame rate (nothing scaled or
  rate-converted at play time), closed GOPs with a keyframe every 0.5–1 s (instant trim, loop and seek), capped
  bitrate. Both Pis decode it in hardware with large headroom (**measured** Pi 4: 121–128 fps for one stream, 90+90
  for two); on the Pi 5 it is the only hardware-decoded codec at all.
- **Alpha video** (ProRes 4444, QuickTime Animation, PNG video; 1–9 fps today, CPU-bound): encoded as one HEVC
  stream with the colour on top and the alpha matte below (1920×2160, inside the HEVC decoder's 4K range), recombined
  in one GPU pass or by the ISP. Expected: full rate.
- **RAM frame cache** for stings, logos and loops: decoded once into NV12 frames held in memory. Zero decode at play
  time, frame-exact loops, first frame on screen on the next refresh. 1 s of 1080p60 NV12 ≈ 187 MB, so the budget is
  seconds on a 4 GB Pi 4 and tens of seconds on an 8–16 GB Pi 5.
- **Encoding:** on the Pi it is slow (**measured** x265 ultrafast ≈ 15 fps on the Pi 4; the Pi 5 has no hardware
  encoder either, perhaps 2–3× faster in software: inference). So: background encoding while the wall is idle (as the
  live-page cache keeper does), and optionally a LAN encode worker (any x86 box, e.g. the dev server: x265 or a GPU
  encoder) that CuTePi sends jobs to.

**Expected:** every cue hardware-decoded at full rate on both Pis; two full-screen 1080p60 layers; alpha video at
60; GO latency for cached clips of one refresh.

**Risks:** storage (a second copy of every file), encode time on large libraries, generational quality loss
(mitigate with high bitrate), and operator expectations (the original always kept, the copy invisible).

**Effort:** medium-high. **First proof (days):** convert the codec-support set and the alpha set with an offline
script, rerun `support.py`, and compare against today's README tables.

---

## Proposal 3 — Real-time appliance (the system as part of the player)

**Idea.** Stop treating the OS as a general-purpose Linux box: make it a dedicated playback appliance.

- **Real-time kernel:** PREEMPT_RT (mainline since 6.12; the Pi kernel is 6.18) built for the Pi, so the presenter and
  audio threads are never delayed by kernel work. **Measured** today: moving the GPU wall's threads to SCHED_FIFO alone
  took H.264 from 0 of 8 runs at full rate to 8 of 8; PREEMPT_RT removes the kernel-side latency that remains.
- **Isolated cores:** `isolcpus`/`nohz_full`/`rcu_nocbs` on one core for the presenter and audio, display and codec
  interrupts pinned to it, everything else (web UI, Go GC, SQLite, WebKit) kept off it.
- **Clocks and cooling:** an active-cooled case and an overclock: Pi 4 `arm_freq` ~2000, `v3d_freq` ~750, and the
  H.264/HEVC/ISP block clocks (`h264_freq`, `hevc_freq`, `isp_freq`) raised, which is the one lever that moves the
  H.264 decoder's ~75 fps ceiling; Pi 5 `arm_freq` ~2800–3000, `v3d_freq` ~960 (inference). Measured with the same
  harness before adoption, with a thermal soak.
- **Stripped image:** a CuTePi OS image (Raspberry Pi OS Lite base or Buildroot): read-only root, logs to RAM,
  nothing but the service and NetworkManager running, no getty on the HDMI console, boot straight to the wall.
- **Memory:** `mlockall` for the presenter, a larger CMA pool sized for the layer count, Go's GC tuned (GOMEMLIMIT)
  so it never runs on the presenter's core.

**Expected:** p99.9 frame intervals at one refresh under load; decoder headroom raised by the overclock; faster,
predictable boot; fewer moving parts on show day.

**Risks:** a custom kernel and image to maintain per Raspberry Pi OS release; overclocking is hardware-dependent and
needs cooling and a soak per board (and voids nothing only while `over_voltage` is not forced); real-time mistakes can
starve the system (mitigated by RT throttling, kept on).

**Effort:** medium for kernel and image, low for clocks. **First proof (a day):** `h264_freq`/`v3d_freq` raised on the
test Pi with active cooling, then the decode benchmark and `support.py`; a PREEMPT_RT build measured with the soak's
p99.9.

---

## The "worth doing" list, where it stands

1. **p99 frame intervals and soak runs in `support.py`:** done (`--soak`, intervals on the plane wall); used for this
   round.
2. **Real-time wall threads:** done and adopted as the GL wall's default (8 of 8 runs at full rate for H.264).
3. **Finish the GPU wall:** possible again now H.264 passes; but the plane wall vs GPU wall vs Proposal 1 is one
   decision, best taken once.
4. **Don't redraw unchanged pictures:** not started; Proposal 1 makes it unnecessary.
5. **System settings:** CPU governor measured at 1–3 % (not adopted); clocks are Proposal 3.
6. **Recommend HEVC for 1080p60:** not started; Proposal 2 makes it automatic.

Suggested order: Proposal 1's first proof (it also carries the new layer stack), then Proposal 2's offline proof
(cheap, independent), then Proposal 3's clock test.
