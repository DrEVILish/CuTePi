# GPU wall measurement harnesses

Small C programs behind the GPU-wall figures in TEST_REPORT ("GPU upload routes for software-decoded video"). They
are measurement tools, not part of the service.

Build on the Pi (the GStreamer development headers are already there for cgo):

```
gcc -O2 -o mixbench mixbench.c $(pkg-config --cflags --libs gstreamer-1.0)
gcc -O2 -o bridgebench bridgebench.c $(pkg-config --cflags --libs gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0 gstreamer-gl-1.0) -lpthread
```

GL environment:

- Headless, leaving the display to the running service: `GST_GL_API=gles2 GST_GL_PLATFORM=egl GST_GL_WINDOW=surfaceless`
  (uses the render node, never `/dev/dri/card*`).
- On HDMI: `GST_GL_API=gles2 GST_GL_PLATFORM=egl GST_GL_WINDOW=gbm GST_GL_GBM_DRM_DEVICE=/dev/dri/card1`. This needs
  DRM master: stop `cutepi` first and start it again afterwards.

## lineartarget

`lineartarget [card]`: renders into a linear dumb buffer from the render node. Creates a 1920x1080 dumb buffer on the
HDMI card (default `/dev/dri/card1`; no DRM master needed, the service can keep running), exports it as a DMABuf,
imports it as an AB24 linear EGLImage (EGL surfaceless), checks the framebuffer is complete and that a clear reads back
through the mapping, then times 600 full-screen textured draws with `glFinish` each. `-loop N` draws N frames
round-robin into three imported ring buffers with `glFlush` and a fence per buffer (as the presenter does), paced at
60 Hz, for counting V3D buffer allocations from outside.

```
gcc -O2 -o lineartarget lineartarget.c $(pkg-config --cflags --libs libdrm egl glesv2)
```

## linearsource

`linearsource [card] [frames]`: the cost of getting a 1080p RGBA frame from system memory to V3D each frame: (a)
`glTexSubImage2D` into an ordinary texture (what `glupload` does; V3D tiles it on the CPU in the GL thread), against
(b) a `memcpy` into a mapped linear dumb buffer imported as an AB24 EGLImage texture. Prints CPU time per upload or
copy and the time of a full-screen draw sampling it (with `glFinish`), and reads the drawn pixel back to check the new
frame is the one sampled. Headless; no DRM master needed. Count V3D TFU jobs and buffer creations from outside with
kprobes on `v3d_submit_tfu_ioctl` and `v3d_create_bo_ioctl`.

```
gcc -O2 -o linearsource linearsource.c $(pkg-config --cflags --libs libdrm egl glesv2)
```

## mixbench

`mixbench "<branch 0>" ["<branch 1>" ...]`: each branch is a gst-launch description ending in an element named
`out<i>`; its src pad is linked to a `glvideomixerelement` request pad without the parse-time caps check
(`gst_parse_launch` refuses to link a mixer pad behind a pinned DMA_DRM capsfilter). Prints mixer output frames per
second (headless, unsynced).

## bridgebench

`bridgebench [options] "<cue chain 0>" ["<cue chain 1>" ...]`: each cue chain (source .. decoder, ending in caps such as
`video/x-raw(memory:DMABuf)` for hardware decoders) runs in its own pipeline into an appsink; a C thread per layer
pulls, shallow-copies and pushes into the wall pipeline's appsrc → `glupload` → `glcolorconvert` → queue →
`glvideomixerelement`. The appsink answers the decoder's allocation query (video meta, pool hint); layers attach
after preroll with the preroll sample's caps. Counts frames pulled, pushed and mixed.

Options:

- `-pool N`: pool-size hint in the allocation answer (default 8).
- `-qmax N`: appsrc queue depth (default 3).
- `-display`: real time: a live `gltestsrc` black pad paces the mixer at 60 fps, all pipelines share one clock and base
  time, output to `glimagesink sync=true` (or `-sink`).
- `-sink "<desc>"`: the display-mode sink, for example `"fakesink sync=true"` or
  `"glimagesink sync=true qos=false max-lateness=-1"`.
- `-cuesync`: cue appsinks sync to the clock (default: push ahead, bounded by the appsrc queue).
- `-mixlat MS`: mixer latency (default 33).
- `-kms`: our own presenter: the mixer output is copied by one GPU pass into a ring of three linear dumb buffers on
  `/dev/dri/card1` (imported as EGLImages into the wall's GL context) and a presenter thread page-flips them, waiting
  on a native fence per buffer (`-finish` uses `glFinish` instead). Needs DRM master (stop `cutepi`). `-copyonly`
  does the copy pass without a display (no master needed; pacing is a sleep, so its numbers are only indicative).
- `-ring`: the mixer renders straight into the ring: a `GstGLBufferPool` subclass wrapping the ring textures is
  offered in the mixer's allocation query (probe on its src pad); no copy pass. Implies `-kms`; with `-copyonly` it
  runs headless (no DRM master). `-tiny` replaces the 1080p `gltestsrc` pacing source with a 16×16 `videotestsrc`
  scaled by its mixer pad (no driver-owned 1080p texture in the per-frame path).
- `-copy`: the pump shallow-copies buffers as the spike did (default: ref and make writable).
- `-target 2D|external-oes`: pin the upload texture target (do not: it breaks the direct import; left for experiments).
- `-colorimetry bt709`: set on a layer's appsrc caps when the source caps carry none (the ISP's do not), as the bridge
  will; without it `glcolorconvert` refuses an external-OES YUV layer.
- The exit report also counts, per layer, pushed buffers whose memory is not a DMABuf (a route gone CPU-side).
- ISP route that works: `... ! v4l2convert capture-io-mode=dmabuf ! video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=YU12`
  with `-pool 16 -colorimetry bt709` (pool 32 makes v4l2convert copy to system memory; NV12 imports as 2D and costs
  CPU).
- At exit the harness prints the negotiated upload format and target per layer, the number of distinct memory
  objects seen per layer, and each thread's user and system CPU time.
- `-split`: the wall ends in an appsink and a separate presenter pipeline (appsrc → `-sink`) is given the wall's GL
  display and context to share. Crashes in `gbm_surface_lock_front_buffer` on GBM: GStreamer's GBM window does not
  support a second presenting context on the same display.

Count presented frames on HDMI from outside with ftrace: a kprobe on `drm_mode_page_flip_ioctl` (what `glimagesink`
calls per frame on GBM), `trace_clock=mono`; remove the probe afterwards.
