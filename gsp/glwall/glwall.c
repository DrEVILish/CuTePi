/* GPU compositor wall: the C side (DESIGN §6.1.1, TEST_REPORT "GPU upload
 * routes"). Everything per frame runs here, never through Go objects, so no
 * decoder buffer waits on a garbage collection.
 *
 * Shape:
 *   wall pipeline (always running):
 *     16x16 live black ─┐
 *     layer appsrc → glupload → glcolorconvert → queue ─┤→ glvideomixerelement
 *     …                                                 │     → appsink
 *   The mixer renders straight into a ring of linear dumb buffers on the
 *   HDMI card (imported into its GL context as EGLImages through a buffer
 *   pool offered in its allocation query): no copy pass and no driver-owned
 *   1080p render target, which keeps Mesa's v3d driver out of its
 *   fresh-buffer-per-frame mode. A presenter thread puts each finished
 *   buffer on the wall's KMS plane (the service stays DRM master; the panic
 *   plane stays above) once its native fence has signalled.
 *   Cue frames: a pump thread per layer pulls from the cue's appsink,
 *   re-stamps the buffer from the cue's running time onto the wall clock
 *   (both pipelines share the system clock) and pushes it into the layer's
 *   appsrc. Hardware and ISP frames are DMABufs the GPU samples directly.
 */
#include "glwall.h"

#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <GLES2/gl2ext.h>
#include <drm_fourcc.h>
#include <fcntl.h>
#include <gst/allocators/gstdmabuf.h>
#include <gst/app/gstappsink.h>
#include <gst/app/gstappsrc.h>
#include <gst/base/gstbasesink.h>
#include <gst/gl/gl.h>
#include <gst/gl/gstglbufferpool.h>
#include <gst/gl/gstglfuncs.h>
#include <gst/gl/gstglmemory.h>
#include <gst/video/video.h>
#include <math.h>
#include <poll.h>
#include <pthread.h>
#include <sched.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/syscall.h>
#include <stdio.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include <xf86drm.h>
#include <xf86drmMode.h>

/* Eight scan-out buffers. The pool preallocates nothing (min 0): when a
 * layer attaches, the mixer renegotiates its allocation and the pool is
 * cycled, and a min of RING made every reactivation want all eight while some
 * were still out (TEST_REPORT, GPU wall step 2: the mixer then waited in
 * acquire_buffer for good). Each slot's state is tracked explicitly: a slot
 * wrapped by a live GstBuffer is "allocated"; the slot on screen is never
 * handed out again until another frame has replaced it, whatever the pool
 * does, so the mixer never renders into the picture being scanned out. */
#define RING 8
#define MIXER_LATENCY_NS (33 * GST_MSECOND)
/* Frame time to latch before the presenter has measured it (measured
 * 2026-10-03: frames leave the wall's sink 87 ms after their time, then wait
 * for the next vblank). */
#define DEFAULT_DELAY_NS (100 * GST_MSECOND)

typedef struct {
  uint32_t handle, pitch, fb;
  int dmafd;
  EGLImageKHR img;
  GLuint tex;
} ring_buf;

/* A finished frame on its way to the screen: it owns its sample (and so its
 * ring buffer) and its fence; nothing per slot is ever overwritten. */
/* What one output frame showed of each layer, snapshotted as the mixer
 * selects its inputs: the opacity it is about to blend with and which cue
 * frame (by PTS). Counted only once that output frame is on screen. */
typedef struct {
  guint64 layer_id;
  double alpha;
  GstClockTime frame_pts;
  int has_frame;
} layer_snap;

typedef struct {
  GstClockTime pts; /* output frame */
  int n;
  layer_snap layers[];
} frame_snap;

typedef struct {
  int idx, fence;
  GstSample *sample;
  frame_snap *snap;
  gint64 fenced; /* monotonic µs when the fence was taken */
} frame_item;

static struct {
  int open;
  int fd; /* the service's DRM fd (not ours to close) */
  uint32_t crtc, plane;
  int w, h, hz;
  ring_buf ring[RING];
  int slot_alloc[RING]; /* wrapped by a live GstBuffer */
  int onscreen;         /* slot being scanned out, -1 none */
  GMutex slot_lock;
  GCond slot_cond;
  int ring_imported;
  GstVideoInfo ring_vinfo;
  GstBufferPool *pool;
  GstElement *wall, *mixer, *wout;
  GstGLContext *ctx;
  EGLDisplay dpy;
  int use_fence;
  GAsyncQueue *ready; /* frame_item*, in presentation order */
  GThread *wall_thread, *present_thread;
  volatile int quit;
  guint64 mixed, presented;
  /* pool diagnostics (/api/debug/glwall) */
  guint64 allocs, frees, exhausted, set_configs, alloc_queries, activations, skipped;
  /* presenter timing, microseconds: waiting for the GPU's fence, and the
   * SetPlane commit (returns once the frame is latched at a vblank) */
  guint64 fence_us, fence_max_us, flip_us, flip_max_us, flips_long;
  /* GPU time (fence taken -> signalled, µs) and how late each mixed frame
   * was handed out by the wall's sink against its due time (µs) */
  guint64 gpu_us, gpu_max_us, late_us, late_max_us;
  guint64 unsnapped; /* frames presented with no mixer snapshot */
  gint64 delay_ns;    /* frame time -> latched on screen, moving average */
  GMutex lock;     /* layers, layer_ids */
  GList *layers;   /* glwall_layer*, attached */
  guint64 layer_ids;
  GMutex snap_lock;
  GQueue snaps;    /* frame_snap*, mixed but not yet pulled */
} W;

struct glwall_layer {
  GstElement *appsink, *cue; /* the cue pipeline (parent of its appsink) */
  GstElement *src, *upload, *convert, *caps, *queue;
  GstPad *mixpad;
  GThread *pump;
  volatile int quit;
  guint64 pulled, pushed, steps; /* steps: opacity changes requested */
  double alpha;
  guint64 id;
  /* On screen (written by the presenter): output frames that showed a new
   * cue frame, and output frames whose opacity differed from the previous
   * one shown. These are what a viewer sees, unlike pushed and steps. */
  guint64 shown_frames, shown_steps;
  double shown_alpha;
  GstClockTime shown_pts;
  int shown_any;
  /* Frames that reached the pump too late for their output frame: shown on
   * arrival instead (the mixer would drop them and freeze the picture), and
   * the decoder told through QoS so it skips ahead to catch up. */
  guint64 late;
  gint64 lag_ns; /* how late the last frame reached the pump (0: on time) */
  GstClockTime qos_at; /* wall clock time of the last QoS event sent */
  int still;           /* caps say framerate 0/1: one frame, held */
  /* A fade evaluated per output frame (under W.lock): alpha at clock time T
   * is from + (to - from) * shape((T - start) / dur). */
  struct { int active, curve; double from, to; GstClockTime start, dur; } ramp;
  GstClockTime last_pts; /* last PTS pushed (wall running time) */
  struct lin_set *lin;   /* RGBA frames copied into linear buffers (lin_*) */
};

/* ---- linear upload slots for RGBA layers ----------------------------------- */

/* A moving layer whose frames arrive in system memory as RGBA (the alpha
 * route) was uploaded by glupload on the GL thread, and V3D tiles such an
 * upload on the CPU: 7 ms a 1080p frame (tools/gpu-wall/linearsource), during
 * which the mixer cannot render, so the whole wall missed refreshes beside an
 * alpha video. Instead the layer's pump copies each frame (4.4 ms, a plain
 * memcpy, off the GL thread) into one of LIN_SLOTS linear dumb buffers on the
 * display card, imported once as AB24 textures; the copy is wrapped as GL
 * memory, so glupload passes it through. The GPU converts the linear buffer
 * to its tiled layout per frame (a TFU job, ~2.4 ms of GPU time) instead of
 * the CPU. A slot is free again when the mixer releases the buffer. Stills
 * keep glupload: they are uploaded once, while a linear texture costs the TFU
 * job on every output frame. */
#define LIN_SLOTS 4

typedef struct lin_set {
  int w, h, refs; /* refs: the layer + each buffer out */
  struct { uint32_t handle, pitch; int dmafd; unsigned char *map; size_t size; EGLImageKHR img; GLuint tex; int busy; } s[LIN_SLOTS];
  GstVideoInfo vinfo;
  GMutex lock;
  GCond cond;
} lin_set;

static PFNEGLCREATESYNCKHRPROC p_mksync;
static PFNEGLDESTROYSYNCKHRPROC p_rmsync;
static PFNEGLDUPNATIVEFENCEFDANDROIDPROC p_dupfence;
static PFNEGLCREATEIMAGEKHRPROC p_mkimg;
static PFNEGLDESTROYIMAGEKHRPROC p_rmimg;
static PFNGLEGLIMAGETARGETTEXTURE2DOESPROC p_bindimg;

/* The wall's own threads (the GL thread, the mixer's output thread, the pull
 * and presenter threads, the layer pumps) run ahead of the cue decoders: a
 * software decoder keeps all four cores busy, and starved wall threads made
 * the whole wall miss refreshes (ProRes 4444: presented 49 a second, with 20
 * skips every half second), every layer with it. Nice -10 gives them about
 * ten times a decoder thread's share without real-time scheduling's risk of
 * locking the system up. Per thread (Linux), and only once per thread. */
#define WALL_NICE (-10)

/* Real-time scheduling (default): every wall thread runs SCHED_FIFO 10, so
 * no decoder or other work can delay the wall by a time slice. Measured on the
 * Pi 4 (TEST_REPORT "GPU wall: real-time threads"): H.264 1080p60 shown at
 * 59.6-60.1 in 8 of 8 runs against 56.5-58.9 at nice -10, HEVC 60, and the
 * web UI still answering within 0.4 s through a 10-minute soak with a
 * software decoder busy. Priority 10 stays below the kernel's interrupt
 * threads (FIFO 50), and the kernel's RT throttling (sched_rt_runtime_us)
 * keeps a runaway thread from locking the system. Unprivileged it needs
 * RLIMIT_RTPRIO (the unit's LimitRTPRIO); without it, or with
 * CUTEPI_WALL_RT=nice, the wall falls back to nice -10 (LimitNICE).
 * CUTEPI_WALL_RT=presenter puts only the presenter on SCHED_FIFO. */
#define WALL_RT_PRIO 10

static void wall_thread_priority_rt(int presenter) {
  static __thread int done;
  if (done) return;
  done = 1;
  const char *rt = g_getenv("CUTEPI_WALL_RT");
  if (!rt || !*rt) rt = "all";
  if (g_str_equal(rt, "all") || (presenter && g_str_equal(rt, "presenter"))) {
    struct sched_param sp = { .sched_priority = WALL_RT_PRIO };
    if (pthread_setschedparam(pthread_self(), SCHED_FIFO, &sp) == 0) {
      GST_INFO("glwall: thread on SCHED_FIFO %d", WALL_RT_PRIO);
      return;
    }
    g_printerr("glwall: SCHED_FIFO: %s (falling back to nice %d)\n", g_strerror(errno), WALL_NICE);
  }
  /* Printed, not a GStreamer debug line: losing the priority silently (an
   * unprivileged service without LimitNICE) costs the wall refreshes. */
  if (setpriority(PRIO_PROCESS, (id_t)syscall(SYS_gettid), WALL_NICE) != 0)
    g_printerr("glwall: thread nice %d: %s (cutepi.service needs LimitNICE=%d)\n", WALL_NICE, g_strerror(errno), WALL_NICE);
}

static void wall_thread_priority(void) { wall_thread_priority_rt(0); }

/* ---- ring: dumb buffers on the display card ------------------------------ */

static int ring_create(char **err) {
  for (int i = 0; i < RING; i++) {
    struct drm_mode_create_dumb cd = { .width = W.w, .height = W.h, .bpp = 32 };
    if (drmIoctl(W.fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) { *err = g_strdup_printf("create dumb buffer: %s", g_strerror(errno)); return -1; }
    W.ring[i].handle = cd.handle; W.ring[i].pitch = cd.pitch;
    uint32_t h[4] = { cd.handle }, p[4] = { cd.pitch }, o[4] = { 0 };
    if (drmModeAddFB2(W.fd, W.w, W.h, DRM_FORMAT_XBGR8888, h, p, o, &W.ring[i].fb, 0)) { *err = g_strdup_printf("addfb2: %s", g_strerror(errno)); return -1; }
    if (drmPrimeHandleToFD(W.fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &W.ring[i].dmafd)) { *err = g_strdup_printf("prime export: %s", g_strerror(errno)); return -1; }
  }
  return 0;
}

static void ring_destroy(void) {
  for (int i = 0; i < RING; i++) {
    ring_buf *r = &W.ring[i];
    if (r->fb) drmModeRmFB(W.fd, r->fb);
    if (r->dmafd > 0) close(r->dmafd);
    if (r->handle) { struct drm_mode_destroy_dumb dd = { .handle = r->handle }; drmIoctl(W.fd, DRM_IOCTL_MODE_DESTROY_DUMB, &dd); }
    memset(r, 0, sizeof *r);
  }
}

/* On the wall's GL thread: import the ring as textures (once). */
static void gl_setup(GstGLContext *ctx, gpointer d) {
  wall_thread_priority(); /* the GL thread: mixing, uploads, fences */
  W.dpy = (EGLDisplay)gst_gl_display_get_handle(ctx->display);
  p_mksync = (void *)eglGetProcAddress("eglCreateSyncKHR");
  p_rmsync = (void *)eglGetProcAddress("eglDestroySyncKHR");
  p_dupfence = (void *)eglGetProcAddress("eglDupNativeFenceFDANDROID");
  p_mkimg = (void *)eglGetProcAddress("eglCreateImageKHR");
  p_rmimg = (void *)eglGetProcAddress("eglDestroyImageKHR");
  p_bindimg = (void *)eglGetProcAddress("glEGLImageTargetTexture2DOES");
  const char *ext = eglQueryString(W.dpy, EGL_EXTENSIONS);
  W.use_fence = p_mksync && p_dupfence && ext && strstr(ext, "EGL_ANDROID_native_fence_sync");
  for (int i = 0; i < RING; i++) {
    ring_buf *r = &W.ring[i];
    EGLint ia[] = { EGL_WIDTH, W.w, EGL_HEIGHT, W.h, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
      EGL_DMA_BUF_PLANE0_FD_EXT, r->dmafd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0, EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint)r->pitch,
      EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, 0, EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, 0, EGL_NONE };
    r->img = p_mkimg(W.dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, ia);
    if (r->img == EGL_NO_IMAGE_KHR) { GST_ERROR("glwall: ring import failed (0x%x)", eglGetError()); continue; }
    glGenTextures(1, &r->tex); glBindTexture(GL_TEXTURE_2D, r->tex); p_bindimg(GL_TEXTURE_2D, r->img);
    glBindTexture(GL_TEXTURE_2D, 0);
  }
  W.ring_imported = 1;
}

/* On the GL thread: a native fence after the mixer's render of this frame. */
static void gl_fence(GstGLContext *ctx, gpointer d) {
  frame_item *f = d;
  f->fence = -1;
  if (W.use_fence) {
    EGLint at[] = { EGL_SYNC_NATIVE_FENCE_FD_ANDROID, EGL_NO_NATIVE_FENCE_FD_ANDROID, EGL_NONE };
    EGLSyncKHR sy = p_mksync(W.dpy, EGL_SYNC_NATIVE_FENCE_ANDROID, at);
    glFlush();
    f->fence = p_dupfence(W.dpy, sy);
    p_rmsync(W.dpy, sy);
    f->fenced = g_get_monotonic_time();
  } else {
    glFinish();
  }
}

static void frame_item_free(frame_item *f) {
  if (!f) return;
  g_free(f->snap);
  if (f->fence >= 0) close(f->fence);
  if (f->sample) gst_sample_unref(f->sample);
  g_free(f);
}

/* ---- the ring as the mixer's buffer pool ----------------------------------- */

typedef struct { GstGLBufferPool parent; } RingPool;
typedef struct { GstGLBufferPoolClass parent; } RingPoolClass;
static GType ring_pool_get_type(void);
G_DEFINE_TYPE(RingPool, ring_pool, GST_TYPE_GL_BUFFER_POOL)

static int ring_index_of(GstBuffer *b) {
  GstMemory *m = gst_buffer_peek_memory(b, 0);
  if (!gst_is_gl_memory(m)) return -1;
  guint t = gst_gl_memory_get_texture_id((GstGLMemory *)m);
  for (int i = 0; i < RING; i++) if (W.ring[i].tex == t) return i;
  return -1;
}

/* A free slot (not wrapped, not on screen), waiting up to 200 ms for one. */
static int slot_take(void) {
  gint64 until = g_get_monotonic_time() + 200 * G_TIME_SPAN_MILLISECOND;
  g_mutex_lock(&W.slot_lock);
  for (;;) {
    for (int i = 0; i < RING; i++)
      if (!W.slot_alloc[i] && i != W.onscreen) { W.slot_alloc[i] = 1; g_mutex_unlock(&W.slot_lock); return i; }
    if (!g_cond_wait_until(&W.slot_cond, &W.slot_lock, until)) break;
  }
  g_mutex_unlock(&W.slot_lock);
  return -1;
}

static void slot_release(int i) {
  if (i < 0) return;
  g_mutex_lock(&W.slot_lock);
  W.slot_alloc[i] = 0;
  g_cond_broadcast(&W.slot_cond);
  g_mutex_unlock(&W.slot_lock);
}

static GstFlowReturn ring_pool_alloc(GstBufferPool *pool, GstBuffer **out, GstBufferPoolAcquireParams *ap) {
  int i = slot_take();
  if (i < 0) {
    W.exhausted++;
    g_printerr("glwall: ring pool exhausted (allocs %" G_GUINT64_FORMAT ", frees %" G_GUINT64_FORMAT ")\n", W.allocs, W.frees);
    return GST_FLOW_ERROR;
  }
  GstGLContext *ctx = GST_GL_BUFFER_POOL(pool)->context;
  GstGLVideoAllocationParams *params = gst_gl_video_allocation_params_new_wrapped_gl_handle(ctx, NULL, &W.ring_vinfo, 0, NULL,
    GST_GL_TEXTURE_TARGET_2D, GST_GL_RGBA, GUINT_TO_POINTER(W.ring[i].tex), NULL, NULL);
  GstBuffer *b = gst_buffer_new(); gpointer wd[1] = { GUINT_TO_POINTER(W.ring[i].tex) };
  if (!gst_gl_memory_setup_buffer(gst_gl_memory_allocator_get_default(ctx), b, params, NULL, wd, 1)) {
    gst_gl_allocation_params_free((GstGLAllocationParams *)params); gst_buffer_unref(b); slot_release(i);
    g_printerr("glwall: wrapping ring texture %d failed\n", i);
    return GST_FLOW_ERROR;
  }
  gst_gl_allocation_params_free((GstGLAllocationParams *)params);
  W.allocs++;
  *out = b;
  return GST_FLOW_OK;
}

static void ring_pool_free(GstBufferPool *pool, GstBuffer *b) {
  slot_release(ring_index_of(b));
  W.frees++;
  GST_BUFFER_POOL_CLASS(ring_pool_parent_class)->free_buffer(pool, b);
}

/* Whatever the mixer asks, the pool preallocates nothing (see RING). */
static gboolean ring_pool_set_config(GstBufferPool *pool, GstStructure *cfg) {
  GstCaps *caps; guint size, min, max;
  if (gst_buffer_pool_config_get_params(cfg, &caps, &size, &min, &max))
    gst_buffer_pool_config_set_params(cfg, caps, size, 0, RING);
  W.set_configs++;
  return GST_BUFFER_POOL_CLASS(ring_pool_parent_class)->set_config(pool, cfg);
}

static gboolean ring_pool_start(GstBufferPool *pool) {
  W.activations++;
  return GST_BUFFER_POOL_CLASS(ring_pool_parent_class)->start(pool);
}

static void ring_pool_class_init(RingPoolClass *k) {
  GstBufferPoolClass *bp = (GstBufferPoolClass *)k;
  bp->alloc_buffer = ring_pool_alloc;
  bp->free_buffer = ring_pool_free;
  bp->set_config = ring_pool_set_config;
  bp->start = ring_pool_start;
}
static void ring_pool_init(RingPool *p) {}

/* The mixer's allocation query (it runs again whenever a layer attaches):
 * answer with the one ring pool, importing the ring on the first call. */
static GstPadProbeReturn mixer_allocq(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
  GstQuery *q = GST_PAD_PROBE_INFO_QUERY(info);
  if (GST_QUERY_TYPE(q) != GST_QUERY_ALLOCATION) return GST_PAD_PROBE_OK;
  GstCaps *caps; gst_query_parse_allocation(q, &caps, NULL);
  if (!caps || !gst_video_info_from_caps(&W.ring_vinfo, caps)) return GST_PAD_PROBE_OK;
  GstGLContext *ctx = NULL; g_object_get(W.mixer, "context", &ctx, NULL);
  if (!ctx) return GST_PAD_PROBE_OK;
  W.alloc_queries++;
  if (!W.ring_imported) gst_gl_context_thread_add(ctx, gl_setup, NULL);
  if (!W.ctx) W.ctx = gst_object_ref(ctx);
  if (!W.pool) {
    W.pool = g_object_new(ring_pool_get_type(), NULL);
    GST_GL_BUFFER_POOL(W.pool)->context = gst_object_ref(ctx);
    GstStructure *cfg = gst_buffer_pool_get_config(W.pool);
    gst_buffer_pool_config_set_params(cfg, caps, W.ring_vinfo.size, 0, RING);
    gst_buffer_pool_config_add_option(cfg, GST_BUFFER_POOL_OPTION_VIDEO_META);
    if (!gst_buffer_pool_set_config(W.pool, cfg)) g_printerr("glwall: ring pool config refused\n");
  }
  gst_query_add_allocation_meta(q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_pool(q, W.pool, W.ring_vinfo.size, 0, RING);
  gst_object_unref(ctx);
  return GST_PAD_PROBE_HANDLED;
}

/* ---- what each output frame shows ----------------------------------------- */

/* The fade envelope, as gsp's fadeShape (linear, smooth, log, exp). */
static double ramp_shape(int curve, double t) {
  if (t <= 0) return 0;
  if (t >= 1) return 1;
  switch (curve) {
  case GLWALL_CURVE_SMOOTH: return t * t * (3 - 2 * t);
  case GLWALL_CURVE_LOG: return log10(1 + 9 * t);
  case GLWALL_CURVE_EXP: return (exp(3 * t) - 1) / (exp(3) - 1);
  default: return t;
  }
}

/* ramp_shape for tests (it must match gsp's fadeShape). */
double glwall_ramp_shape(int curve, double t) { return ramp_shape(curve, t); }

/* Under W.lock, before the mixer renders the output frame due at clock time
 * T: a layer with a fade gets the opacity for exactly that frame, so a fade
 * changes on every refresh whatever the CPU is doing (a Go timer writing
 * levels missed refreshes under load). */
static void ramp_apply(glwall_layer *l, GstClockTime T) {
  if (!l->ramp.active || !GST_CLOCK_TIME_IS_VALID(T)) return;
  double t = l->ramp.dur ? ((double)(gint64)(T - l->ramp.start)) / (double)l->ramp.dur : 1;
  if (T < l->ramp.start) t = 0;
  double a = l->ramp.from + (l->ramp.to - l->ramp.from) * ramp_shape(l->ramp.curve, t);
  if (t >= 1) { a = l->ramp.to; l->ramp.active = 0; }
  if (a != l->alpha) { l->alpha = a; l->steps++; }
  g_object_set(l->mixpad, "alpha", a, NULL);
}

/* On the mixer's thread, just before it renders an output frame: snapshot
 * each layer's opacity and current cue frame (the values this render uses). */
static void mixer_selected(GstElement *agg, GstSegment *seg, guint64 pts, guint64 dts, guint64 dur, GstStructure *info, gpointer d) {
  wall_thread_priority(); /* the mixer's output thread */
  GstClockTime T = GST_CLOCK_TIME_NONE, base = gst_element_get_base_time(W.wall);
  if (seg && GST_CLOCK_TIME_IS_VALID(pts) && GST_CLOCK_TIME_IS_VALID(base)) {
    GstClockTime rt = gst_segment_to_running_time(seg, GST_FORMAT_TIME, pts);
    if (GST_CLOCK_TIME_IS_VALID(rt)) T = base + rt;
  }
  g_mutex_lock(&W.lock);
  int n = g_list_length(W.layers);
  frame_snap *fs = g_malloc0(sizeof *fs + n * sizeof(layer_snap));
  fs->pts = pts;
  for (GList *it = W.layers; it; it = it->next) {
    glwall_layer *l = it->data;
    ramp_apply(l, T);
    layer_snap *ls = &fs->layers[fs->n++];
    ls->layer_id = l->id;
    g_object_get(l->mixpad, "alpha", &ls->alpha, NULL);
    ls->frame_pts = GST_CLOCK_TIME_NONE;
    GstSample *s = gst_aggregator_peek_next_sample(GST_AGGREGATOR(agg), GST_AGGREGATOR_PAD(l->mixpad));
    if (s) {
      GstBuffer *b = gst_sample_get_buffer(s);
      if (b) { ls->frame_pts = GST_BUFFER_PTS(b); ls->has_frame = 1; }
      gst_sample_unref(s);
    }
  }
  g_mutex_unlock(&W.lock);
  g_mutex_lock(&W.snap_lock);
  g_queue_push_tail(&W.snaps, fs);
  while (g_queue_get_length(&W.snaps) > 2 * RING) g_free(g_queue_pop_head(&W.snaps));
  g_mutex_unlock(&W.snap_lock);
}

/* The snapshot for the output frame with this PTS (older ones are dropped:
 * their frames never reached the wall's sink). */
static frame_snap *snap_take(GstClockTime pts) {
  frame_snap *found = NULL;
  g_mutex_lock(&W.snap_lock);
  frame_snap *fs;
  while ((fs = g_queue_peek_head(&W.snaps)) != NULL && (!GST_CLOCK_TIME_IS_VALID(pts) || fs->pts <= pts)) {
    g_queue_pop_head(&W.snaps);
    if (fs->pts == pts) { found = fs; break; }
    g_free(fs);
  }
  g_mutex_unlock(&W.snap_lock);
  return found;
}

/* The presenter, once a frame is on screen: credit each layer still attached. */
static void snap_count(frame_snap *fs) {
  if (!fs) return;
  g_mutex_lock(&W.lock);
  for (int i = 0; i < fs->n; i++) {
    layer_snap *ls = &fs->layers[i];
    for (GList *it = W.layers; it; it = it->next) {
      glwall_layer *l = it->data;
      if (l->id != ls->layer_id) continue;
      if (ls->alpha != l->shown_alpha) { if (l->shown_alpha >= 0) l->shown_steps++; l->shown_alpha = ls->alpha; }
      /* A still can arrive without a timestamp: its first frame counts. */
      if (ls->has_frame && (!l->shown_any || (GST_CLOCK_TIME_IS_VALID(ls->frame_pts) && ls->frame_pts != l->shown_pts))) {
        l->shown_frames++; l->shown_pts = ls->frame_pts; l->shown_any = 1;
      }
      break;
    }
  }
  g_mutex_unlock(&W.lock);
}

/* ---- wall and presenter threads -------------------------------------------- */

/* Pulls each mixed frame (in a ring buffer) as it falls due, fences it and
 * hands it to the presenter. The frame item owns the sample. */
static gpointer wall_thread(gpointer d) {
  wall_thread_priority();
  while (!W.quit) {
    GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(W.wout), 100 * GST_MSECOND);
    if (!s) continue;
    int idx = ring_index_of(gst_sample_get_buffer(s));
    if (idx < 0) { GST_WARNING("glwall: mixed frame not in the ring"); gst_sample_unref(s); continue; }
    frame_item *f = g_new0(frame_item, 1);
    f->idx = idx; f->sample = s; f->fence = -1;
    f->snap = snap_take(GST_BUFFER_PTS(gst_sample_get_buffer(s)));
    {
      GstClock *clk = gst_element_get_clock(W.wall);
      GstClockTime pts = GST_BUFFER_PTS(gst_sample_get_buffer(s)), base = gst_element_get_base_time(W.wall);
      if (clk && GST_CLOCK_TIME_IS_VALID(pts)) {
        gint64 late = ((gint64)gst_clock_get_time(clk) - (gint64)(base + pts)) / 1000;
        if (late > 0) { W.late_us += late; if ((guint64)late > W.late_max_us) W.late_max_us = late; }
      }
      if (clk) gst_object_unref(clk);
    }
    gst_gl_context_thread_add(W.ctx, gl_fence, f);
    W.mixed++;
    g_async_queue_push(W.ready, f);
  }
  return NULL;
}

/* Puts each finished frame on the plane. The frame on screen keeps its
 * sample (so its buffer stays out of the pool) until the next one has
 * replaced it; its slot is marked on screen so no alloc can hand it out. */
static gpointer present_thread(gpointer d) {
  wall_thread_priority_rt(1);
  frame_item *shown = NULL;
  for (;;) {
    frame_item *f = g_async_queue_timeout_pop(W.ready, 100000);
    if (W.quit) { frame_item_free(f); break; }
    if (!f) continue;
    /* Behind (a backlog formed, e.g. while the wall started): go straight to
     * the newest finished frame. A standing queue is pure latency (each
     * queued frame delays the picture a refresh against the sound). */
    frame_item *newer;
    while ((newer = g_async_queue_try_pop(W.ready)) != NULL) {
      frame_item_free(f);
      f = newer;
      W.skipped++;
    }
    gint64 t0 = g_get_monotonic_time();
    if (f->fence >= 0) { struct pollfd ff = { .fd = f->fence, .events = POLLIN }; poll(&ff, 1, 1000); close(f->fence); f->fence = -1; }
    gint64 t1 = g_get_monotonic_time();
    if (f->fenced) { gint64 g = t1 - f->fenced; if (t1 - t0 > 50) { W.gpu_us += g; if ((guint64)g > W.gpu_max_us) W.gpu_max_us = g; } }
    ring_buf *r = &W.ring[f->idx];
    /* Legacy SetPlane: one commit, returns when the frame is latched. */
    if (drmModeSetPlane(W.fd, W.plane, W.crtc, r->fb, 0, 0, 0, W.w, W.h, 0, 0, (uint32_t)W.w << 16, (uint32_t)W.h << 16)) {
      GST_WARNING("glwall: setplane: %s", g_strerror(errno));
      frame_item_free(f);
      continue;
    }
    gint64 t2 = g_get_monotonic_time();
    W.fence_us += t1 - t0; if ((guint64)(t1 - t0) > W.fence_max_us) W.fence_max_us = t1 - t0;
    W.flip_us += t2 - t1; if ((guint64)(t2 - t1) > W.flip_max_us) W.flip_max_us = t2 - t1;
    if (t2 - t1 > 1000000 / W.hz + 2000) W.flips_long++;
    W.presented++;
    if (!f->snap) W.unsnapped++;
    {
      /* How long after its time a frame reaches the screen: SetPlane has
       * just returned at the vblank that latched it (both clocks are
       * CLOCK_MONOTONIC). Cue sound is delayed by this (glwall_align_audio). */
      GstBuffer *fb = gst_sample_get_buffer(f->sample);
      GstClockTime base = gst_element_get_base_time(W.wall);
      if (fb && GST_CLOCK_TIME_IS_VALID(GST_BUFFER_PTS(fb)) && GST_CLOCK_TIME_IS_VALID(base)) {
        gint64 d = t2 * 1000 - (gint64)(base + GST_BUFFER_PTS(fb));
        if (d > 0 && d < GST_SECOND) W.delay_ns = W.delay_ns ? (W.delay_ns * 31 + d) / 32 : d;
      }
    }
    snap_count(f->snap);
    g_mutex_lock(&W.slot_lock);
    W.onscreen = f->idx;
    g_cond_broadcast(&W.slot_cond);
    g_mutex_unlock(&W.slot_lock);
    frame_item_free(shown); /* the previous frame has left the screen */
    shown = f;
  }
  frame_item_free(shown);
  return NULL;
}

/* ---- open / close ---------------------------------------------------------- */

int glwall_is_open(void) { return W.open; }

int glwall_open(int drm_fd, uint32_t crtc_id, uint32_t plane_id, int width, int height, int refresh_hz, char **err) {
  *err = NULL;
  if (W.open) return 0;
  memset(&W, 0, sizeof W);
  W.onscreen = -1;
  W.fd = drm_fd; W.crtc = crtc_id; W.plane = plane_id; W.w = width; W.h = height; W.hz = refresh_hz > 0 ? refresh_hz : 60;
  g_mutex_init(&W.lock);
  g_mutex_init(&W.snap_lock);
  g_queue_init(&W.snaps);
  g_mutex_init(&W.slot_lock);
  g_cond_init(&W.slot_cond);
  if (ring_create(err)) { ring_destroy(); return -1; }
  gchar *desc = g_strdup_printf(
    "videotestsrc is-live=true pattern=black ! video/x-raw,width=16,height=16,framerate=%d/1 ! glupload ! glcolorconvert ! "
    "video/x-raw(memory:GLMemory),format=RGBA ! m.sink_0 "
    "glvideomixerelement name=m background=black latency=%" G_GUINT64_FORMAT " sink_0::width=%d sink_0::height=%d ! "
    "video/x-raw(memory:GLMemory),width=%d,height=%d,framerate=%d/1,format=RGBA ! "
    "appsink name=wout sync=true max-buffers=1 drop=false enable-last-sample=false",
    W.hz, (guint64)MIXER_LATENCY_NS, W.w, W.h, W.w, W.h, W.hz);
  GError *gerr = NULL;
  W.wall = gst_parse_launch(desc, &gerr);
  g_free(desc);
  if (!W.wall || gerr) { *err = g_strdup_printf("wall pipeline: %s", gerr ? gerr->message : "parse failed"); if (gerr) g_error_free(gerr); ring_destroy(); return -1; }
  W.mixer = gst_bin_get_by_name(GST_BIN(W.wall), "m");
  W.wout = gst_bin_get_by_name(GST_BIN(W.wall), "wout");
  GstPad *msrc = gst_element_get_static_pad(W.mixer, "src");
  gst_pad_add_probe(msrc, GST_PAD_PROBE_TYPE_QUERY_DOWNSTREAM, mixer_allocq, NULL, NULL);
  gst_object_unref(msrc);
  g_object_set(W.mixer, "emit-signals", TRUE, NULL);
  g_signal_connect(W.mixer, "samples-selected", G_CALLBACK(mixer_selected), NULL);
  W.ready = g_async_queue_new_full((GDestroyNotify)frame_item_free);
  /* The wall runs on the system clock with a base time every cue can be
   * mapped onto (the pump re-stamps frames from cue to wall base time). */
  GstClock *clk = gst_system_clock_obtain();
  gst_pipeline_use_clock(GST_PIPELINE(W.wall), clk);
  gst_object_unref(clk);
  if (gst_element_set_state(W.wall, GST_STATE_PLAYING) == GST_STATE_CHANGE_FAILURE) {
    *err = g_strdup("wall pipeline would not start");
    gst_element_set_state(W.wall, GST_STATE_NULL); gst_object_unref(W.wall); W.wall = NULL; ring_destroy(); return -1;
  }
  W.present_thread = g_thread_new("glwall-present", present_thread, NULL);
  W.wall_thread = g_thread_new("glwall-pull", wall_thread, NULL);
  W.open = 1;
  return 0;
}

void glwall_close(void) {
  if (!W.open) return;
  W.quit = 1;
  gst_element_set_state(W.wall, GST_STATE_NULL);
  if (W.wall_thread) g_thread_join(W.wall_thread);
  if (W.present_thread) g_thread_join(W.present_thread);
  drmModeSetPlane(W.fd, W.plane, W.crtc, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0); /* plane off */
  if (W.pool) { gst_buffer_pool_set_active(W.pool, FALSE); gst_object_unref(W.pool); }
  if (W.ctx) gst_object_unref(W.ctx);
  if (W.mixer) gst_object_unref(W.mixer);
  if (W.wout) gst_object_unref(W.wout);
  gst_object_unref(W.wall);
  g_async_queue_unref(W.ready);
  g_queue_clear_full(&W.snaps, g_free);
  ring_destroy();
  W.open = 0;
}

void glwall_stats(uint64_t *mixed, uint64_t *presented) { *mixed = W.mixed; *presented = W.presented; }
void glwall_layer_stats(glwall_layer *l, uint64_t *pulled, uint64_t *pushed, uint64_t *steps) {
  *pulled = l ? l->pulled : 0; *pushed = l ? l->pushed : 0; *steps = l ? l->steps : 0;
}
void glwall_layer_shown(glwall_layer *l, uint64_t *frames, uint64_t *steps, uint64_t *late, int64_t *lag_ns) {
  *frames = l ? l->shown_frames : 0; *steps = l ? l->shown_steps : 0; *late = l ? l->late : 0; *lag_ns = l ? l->lag_ns : 0;
}

void glwall_pool_stats(glwall_pool_stats_t *st) {
  memset(st, 0, sizeof *st);
  st->allocs = W.allocs; st->frees = W.frees; st->exhausted = W.exhausted;
  st->set_configs = W.set_configs; st->alloc_queries = W.alloc_queries; st->activations = W.activations;
  g_mutex_lock(&W.slot_lock);
  for (int i = 0; i < RING; i++) st->allocated += W.slot_alloc[i] ? 1 : 0;
  st->onscreen = W.onscreen;
  g_mutex_unlock(&W.slot_lock);
  st->queued = W.ready ? g_async_queue_length(W.ready) : 0;
  st->skipped = W.skipped;
  st->fence_us = W.fence_us; st->fence_max_us = W.fence_max_us; st->flip_us = W.flip_us;
  st->flip_max_us = W.flip_max_us; st->flips_long = W.flips_long;
  st->unsnapped = W.unsnapped;
  st->gpu_us = W.gpu_us; st->gpu_max_us = W.gpu_max_us; st->late_us = W.late_us; st->late_max_us = W.late_max_us;
  W.fence_max_us = W.flip_max_us = W.gpu_max_us = W.late_max_us = 0; /* maxima since the last read */
}

/* ---- layers ----------------------------------------------------------------- */

/* The cue's decoder asks downstream (the appsink) about allocation: hardware
 * decoders refuse to negotiate without video meta and size their pools from
 * the answer; the ISP copies to system memory above 31. */
static GstPadProbeReturn sink_allocq(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
  GstQuery *q = GST_PAD_PROBE_INFO_QUERY(info);
  if (GST_QUERY_TYPE(q) != GST_QUERY_ALLOCATION) return GST_PAD_PROBE_OK;
  GstCaps *caps; gst_query_parse_allocation(q, &caps, NULL);
  GstVideoInfo vi; guint size = 0;
  if (caps && gst_video_info_from_caps(&vi, caps)) size = vi.size;
  gst_query_add_allocation_meta(q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_pool(q, NULL, size, GPOINTER_TO_INT(d), 0);
  return GST_PAD_PROBE_HANDLED;
}

void glwall_prepare_sink(GstElement *appsink, int pool_buffers) {
  GstPad *p = gst_element_get_static_pad(appsink, "sink");
  gst_pad_add_probe(p, GST_PAD_PROBE_TYPE_QUERY_DOWNSTREAM, sink_allocq, GINT_TO_POINTER(pool_buffers), NULL);
  gst_object_unref(p);
}

/* A frame later than the mixer will wait for (its latency, less the time to
 * upload it) would be dropped by the mixer, freezing the layer on its last
 * frame for as long as the decoder stays behind. It is stamped to show on
 * the next output frame instead, and the decoder is sent the QoS event a
 * display sink would send, so it drops frames until it has caught up with
 * the clock (and the sound) again. */
#define LATE_NS (MIXER_LATENCY_NS - 8 * GST_MSECOND)
#define QOS_EVERY (250 * GST_MSECOND)
/* QoS helps only a decoder that can get ahead again: one that cannot skip
 * decoding (intra-only codecs such as DNxHR or ProRes decode every frame and
 * only drop late ones afterwards) and runs at about real time never catches
 * up, and then drops nearly every frame (DNxHR HQ: 1-2 shown a second, lag
 * growing to 4 s). Beyond this lag the decoder is left alone and each frame
 * is shown as it comes: the picture moves at the decoder's speed. */
#define QOS_MAX_LAG (500 * GST_MSECOND)

static void pump_lateness(glwall_layer *l, GstBuffer *b, const GstSegment *seg, GstClockTime cue_pts) {
  GstClockTime pts = GST_BUFFER_PTS(b), wall_base = gst_element_get_base_time(W.wall);
  if (!GST_CLOCK_TIME_IS_VALID(pts) || !GST_CLOCK_TIME_IS_VALID(wall_base)) return;
  GstClock *clk = gst_element_get_clock(W.wall);
  if (!clk) return;
  GstClockTime now = gst_clock_get_time(clk);
  gst_object_unref(clk);
  if (now <= wall_base) return;
  GstClockTime now_rt = now - wall_base;
  l->lag_ns = now_rt > pts ? (gint64)(now_rt - pts) : 0;
  if (pts + LATE_NS < now_rt) {
    GstClockTimeDiff diff = (GstClockTimeDiff)(now_rt - pts);
    GST_BUFFER_PTS(b) = now_rt;
    l->late++;
    GstClockTime rt = seg ? gst_segment_to_running_time(seg, GST_FORMAT_TIME, cue_pts) : GST_CLOCK_TIME_NONE;
    /* GstVideoDecoder skips up to twice the reported lateness ahead, and
     * the converters behind it drop on the same event: report half, at most
     * every QOS_EVERY, so the decoder aims at the clock instead of past it
     * (a full report every frame made it drop nearly everything). */
    if (GST_CLOCK_TIME_IS_VALID(rt) && diff <= (GstClockTimeDiff)QOS_MAX_LAG &&
        (!GST_CLOCK_TIME_IS_VALID(l->qos_at) || now - l->qos_at >= QOS_EVERY)) {
      gst_element_send_event(l->appsink, gst_event_new_qos(GST_QOS_TYPE_UNDERFLOW, 1.0, diff / 2, rt));
      l->qos_at = now;
    }
  }
  /* Never step back: a late frame stamped "now" may be followed by one
   * whose own time is earlier still. */
  if (GST_CLOCK_TIME_IS_VALID(l->last_pts) && GST_BUFFER_PTS(b) <= l->last_pts) GST_BUFFER_PTS(b) = l->last_pts + 1;
  l->last_pts = GST_BUFFER_PTS(b);
}

static void lin_gl_import(GstGLContext *ctx, gpointer d) {
  lin_set *ls = d;
  for (int i = 0; i < LIN_SLOTS; i++) {
    EGLint ia[] = { EGL_WIDTH, ls->w, EGL_HEIGHT, ls->h, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
      EGL_DMA_BUF_PLANE0_FD_EXT, ls->s[i].dmafd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0, EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint)ls->s[i].pitch,
      EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, 0, EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, 0, EGL_NONE };
    ls->s[i].img = p_mkimg(W.dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, ia);
    if (ls->s[i].img == EGL_NO_IMAGE_KHR) continue;
    glGenTextures(1, &ls->s[i].tex); glBindTexture(GL_TEXTURE_2D, ls->s[i].tex); p_bindimg(GL_TEXTURE_2D, ls->s[i].img);
    glBindTexture(GL_TEXTURE_2D, 0);
  }
}

static void lin_gl_release(GstGLContext *ctx, gpointer d) {
  lin_set *ls = d;
  for (int i = 0; i < LIN_SLOTS; i++) {
    if (ls->s[i].tex) glDeleteTextures(1, &ls->s[i].tex);
    if (ls->s[i].img && ls->s[i].img != EGL_NO_IMAGE_KHR && p_rmimg) p_rmimg(W.dpy, ls->s[i].img);
  }
}

static void lin_unref(lin_set *ls) {
  g_mutex_lock(&ls->lock);
  int last = --ls->refs == 0;
  g_mutex_unlock(&ls->lock);
  if (!last) return;
  if (W.ctx) gst_gl_context_thread_add(W.ctx, lin_gl_release, ls);
  for (int i = 0; i < LIN_SLOTS; i++) {
    if (ls->s[i].map) munmap(ls->s[i].map, ls->s[i].size);
    if (ls->s[i].dmafd > 0) close(ls->s[i].dmafd);
    if (ls->s[i].handle) { struct drm_mode_destroy_dumb dd = { .handle = ls->s[i].handle }; drmIoctl(W.fd, DRM_IOCTL_MODE_DESTROY_DUMB, &dd); }
  }
  g_mutex_clear(&ls->lock); g_cond_clear(&ls->cond);
  g_free(ls);
}

/* The slots for a w x h RGBA layer, or NULL (then glupload uploads). */
static lin_set *lin_new(GstCaps *caps) {
  GstVideoInfo vi;
  if (!W.ctx || !p_mkimg || !gst_video_info_from_caps(&vi, caps) || GST_VIDEO_INFO_FORMAT(&vi) != GST_VIDEO_FORMAT_RGBA) return NULL;
  GstCapsFeatures *f = gst_caps_get_features(caps, 0);
  if (f && !gst_caps_features_is_equal(f, GST_CAPS_FEATURES_MEMORY_SYSTEM_MEMORY)) return NULL;
  lin_set *ls = g_new0(lin_set, 1);
  ls->w = vi.width; ls->h = vi.height; ls->refs = 1;
  g_mutex_init(&ls->lock); g_cond_init(&ls->cond);
  for (int i = 0; i < LIN_SLOTS; i++) {
    struct drm_mode_create_dumb cd = { .width = ls->w, .height = ls->h, .bpp = 32 };
    struct drm_mode_map_dumb md = { 0 };
    if (drmIoctl(W.fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) goto fail;
    ls->s[i].handle = cd.handle; ls->s[i].pitch = cd.pitch; ls->s[i].size = cd.size;
    md.handle = cd.handle;
    if (drmIoctl(W.fd, DRM_IOCTL_MODE_MAP_DUMB, &md)) goto fail;
    ls->s[i].map = mmap(NULL, cd.size, PROT_READ | PROT_WRITE, MAP_SHARED, W.fd, md.offset);
    if (ls->s[i].map == MAP_FAILED) { ls->s[i].map = NULL; goto fail; }
    if (drmPrimeHandleToFD(W.fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &ls->s[i].dmafd)) goto fail;
  }
  gst_gl_context_thread_add(W.ctx, lin_gl_import, ls);
  for (int i = 0; i < LIN_SLOTS; i++) if (!ls->s[i].tex) goto fail;
  /* What the slots are as GL memory: RGBA, 2D, the dumb buffer's pitch. */
  gst_video_info_set_format(&ls->vinfo, GST_VIDEO_FORMAT_RGBA, ls->w, ls->h);
  ls->vinfo.fps_n = vi.fps_n; ls->vinfo.fps_d = vi.fps_d; ls->vinfo.par_n = vi.par_n; ls->vinfo.par_d = vi.par_d;
  return ls;
fail:
  GST_WARNING("glwall: linear upload slots unavailable: %s", g_strerror(errno));
  lin_unref(ls);
  return NULL;
}

typedef struct { lin_set *ls; int i; } lin_ref;

static void lin_slot_done(gpointer d) {
  lin_ref *r = d;
  g_mutex_lock(&r->ls->lock);
  r->ls->s[r->i].busy = 0;
  g_cond_broadcast(&r->ls->cond);
  g_mutex_unlock(&r->ls->lock);
  lin_unref(r->ls);
  g_free(r);
}

/* Copy the frame in b (RGBA, system memory) into a free slot and return a
 * GL-memory buffer wrapping it with b's timing; NULL if no slot came free
 * within 100 ms (the frame is dropped) or the copy failed. */
static GstBuffer *lin_copy(glwall_layer *l, GstBuffer *b, GstCaps *caps) {
  lin_set *ls = l->lin;
  GstVideoInfo vi; GstVideoFrame fr;
  if (!gst_video_info_from_caps(&vi, caps) || vi.width != ls->w || vi.height != ls->h) return NULL;
  gint64 until = g_get_monotonic_time() + 100 * G_TIME_SPAN_MILLISECOND;
  int i = -1;
  g_mutex_lock(&ls->lock);
  while (i < 0) {
    for (int k = 0; k < LIN_SLOTS; k++) if (!ls->s[k].busy) { i = k; break; }
    if (i < 0 && !g_cond_wait_until(&ls->cond, &ls->lock, until)) break;
  }
  if (i >= 0) { ls->s[i].busy = 1; ls->refs++; }
  g_mutex_unlock(&ls->lock);
  if (i < 0) return NULL;
  lin_ref *r = g_new0(lin_ref, 1); r->ls = ls; r->i = i;
  if (!gst_video_frame_map(&fr, &vi, b, GST_MAP_READ)) { lin_slot_done(r); return NULL; }
  const guint8 *src = GST_VIDEO_FRAME_PLANE_DATA(&fr, 0);
  int sstride = GST_VIDEO_FRAME_PLANE_STRIDE(&fr, 0), row = ls->w * 4;
  for (int y = 0; y < ls->h; y++) memcpy(ls->s[i].map + (size_t)y * ls->s[i].pitch, src + (size_t)y * sstride, row);
  gst_video_frame_unmap(&fr);
  GstGLVideoAllocationParams *params = gst_gl_video_allocation_params_new_wrapped_gl_handle(W.ctx, NULL, &ls->vinfo, 0, NULL,
    GST_GL_TEXTURE_TARGET_2D, GST_GL_RGBA, GUINT_TO_POINTER(ls->s[i].tex), r, lin_slot_done);
  GstBuffer *out = gst_buffer_new();
  gpointer wd[1] = { GUINT_TO_POINTER(ls->s[i].tex) };
  if (!gst_gl_memory_setup_buffer(gst_gl_memory_allocator_get_default(W.ctx), out, params, NULL, wd, 1)) {
    gst_gl_allocation_params_free((GstGLAllocationParams *)params); gst_buffer_unref(out);
    return NULL; /* the params' notify has released the slot */
  }
  gst_gl_allocation_params_free((GstGLAllocationParams *)params);
  gst_buffer_copy_into(out, b, GST_BUFFER_COPY_TIMESTAMPS | GST_BUFFER_COPY_FLAGS, 0, -1);
  return out;
}

/* The mixer needs each frame's start and end: a frame without a duration
 * makes it wait for the next frame to learn where this one ends, and a
 * still never sends one, so the whole wall stopped (a BMP or GIF still, one
 * buffer with no duration at framerate 0/1). A frame without a timestamp is
 * due now; a still lasts until it is replaced; any other frame without a
 * duration lasts one refresh (the mixer repeats it if nothing follows). */
#define STILL_NS (24 * 3600 * GST_SECOND)

static void pump_timing(glwall_layer *l, GstBuffer *b) {
  if (!GST_CLOCK_TIME_IS_VALID(GST_BUFFER_PTS(b))) {
    GstClock *clk = gst_element_get_clock(W.wall);
    GstClockTime wall_base = gst_element_get_base_time(W.wall);
    if (clk) {
      GstClockTime now = gst_clock_get_time(clk);
      if (GST_CLOCK_TIME_IS_VALID(wall_base) && now > wall_base) GST_BUFFER_PTS(b) = now - wall_base;
      gst_object_unref(clk);
    }
  }
  if (!GST_CLOCK_TIME_IS_VALID(GST_BUFFER_DURATION(b)))
    GST_BUFFER_DURATION(b) = l->still ? STILL_NS : GST_SECOND / W.hz;
}

/* Per layer: pull, re-stamp onto the wall clock, push. The shallow
 * make-writable keeps the decoder's memory (one ref), so nothing is copied. */
static gpointer pump(gpointer data) {
  glwall_layer *l = data;
  wall_thread_priority();
  while (!l->quit) {
    GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(l->appsink), 200 * GST_MSECOND);
    if (!s) { if (gst_app_sink_is_eos(GST_APP_SINK(l->appsink))) break; continue; }
    l->pulled++;
    GstBuffer *b;
    if (l->lin) {
      b = lin_copy(l, gst_sample_get_buffer(s), gst_sample_get_caps(s));
      if (!b) { gst_sample_unref(s); continue; }
    } else {
      b = gst_buffer_ref(gst_sample_get_buffer(s));
    }
    GstSegment segc; const GstSegment *seg = NULL;
    if (gst_sample_get_segment(s)) { gst_segment_copy_into(gst_sample_get_segment(s), &segc); seg = &segc; }
    GstClockTime pts = GST_BUFFER_PTS(b);
    gst_sample_unref(s);
    b = gst_buffer_make_writable(b);
    if (seg && GST_CLOCK_TIME_IS_VALID(pts)) {
      GstClockTime rt = gst_segment_to_running_time(seg, GST_FORMAT_TIME, pts);
      GstClockTime cue_base = gst_element_get_base_time(l->cue), wall_base = gst_element_get_base_time(W.wall);
      if (GST_CLOCK_TIME_IS_VALID(rt) && GST_CLOCK_TIME_IS_VALID(cue_base) && GST_CLOCK_TIME_IS_VALID(wall_base))
        GST_BUFFER_PTS(b) = rt + cue_base - wall_base;
    }
    GST_BUFFER_DTS(b) = GST_CLOCK_TIME_NONE;
    pump_timing(l, b);
    pump_lateness(l, b, seg, pts);
    if (gst_app_src_push_buffer(GST_APP_SRC(l->src), b) != GST_FLOW_OK) break;
    l->pushed++;
  }
  return NULL;
}

glwall_layer *glwall_layer_attach(GstElement *appsink, const char *colorimetry, char **err) {
  *err = NULL;
  if (!W.open) { *err = g_strdup("wall not open"); return NULL; }
  GstSample *ps = gst_app_sink_try_pull_preroll(GST_APP_SINK(appsink), 10 * GST_SECOND);
  if (!ps) { *err = g_strdup("cue has no preroll frame"); return NULL; }
  GstCaps *lc = gst_caps_copy(gst_sample_get_caps(ps));
  gst_sample_unref(ps);
  if (colorimetry && *colorimetry && !gst_structure_has_field(gst_caps_get_structure(lc, 0), "colorimetry"))
    gst_caps_set_simple(lc, "colorimetry", G_TYPE_STRING, colorimetry, NULL);
  glwall_layer *l = g_new0(glwall_layer, 1);
  {
    gint fn = 0, fd = 1;
    GstStructure *st = gst_caps_get_structure(lc, 0);
    l->still = gst_structure_get_fraction(st, "framerate", &fn, &fd) && fn == 0;
  }
  if (!l->still && (l->lin = lin_new(lc)) != NULL) {
    /* The layer's frames arrive as GL memory (the slots), not RGBA in RAM. */
    gst_caps_set_features(lc, 0, gst_caps_features_new(GST_CAPS_FEATURE_MEMORY_GL_MEMORY, NULL));
    gst_caps_set_simple(lc, "texture-target", G_TYPE_STRING, "2D", NULL);
  }
  l->appsink = gst_object_ref(appsink);
  l->cue = GST_ELEMENT(gst_element_get_parent(appsink)); /* ref'd; released in free */
  l->src = gst_element_factory_make("appsrc", NULL);
  l->upload = gst_element_factory_make("glupload", NULL);
  l->convert = gst_element_factory_make("glcolorconvert", NULL);
  l->caps = gst_element_factory_make("capsfilter", NULL);
  l->queue = gst_element_factory_make("queue", NULL);
  if (!l->src || !l->upload || !l->convert || !l->caps || !l->queue) { *err = g_strdup("GL elements missing (gstreamer1.0-gl)"); goto fail; }
  g_object_set(l->src, "format", GST_FORMAT_TIME, "max-buffers", 3, "block", TRUE, "is-live", FALSE, "caps", lc, NULL);
  GstCaps *rgba = gst_caps_from_string("video/x-raw(memory:GLMemory),format=RGBA");
  g_object_set(l->caps, "caps", rgba, NULL); gst_caps_unref(rgba);
  g_object_set(l->queue, "max-size-buffers", 2, "max-size-bytes", 0, "max-size-time", (guint64)0, NULL);
  gst_bin_add_many(GST_BIN(W.wall), l->src, l->upload, l->convert, l->caps, l->queue, NULL);
  if (!gst_element_link_many(l->src, l->upload, l->convert, l->caps, l->queue, NULL)) { *err = g_strdup("layer chain would not link"); goto fail; }
  l->mixpad = gst_element_request_pad_simple(W.mixer, "sink_%u");
  if (!l->mixpad) { *err = g_strdup("mixer has no free pad"); goto fail; }
  g_object_set(l->mixpad, "alpha", 0.0, "zorder", (guint)1, "xpos", 0, "ypos", 0, "width", W.w, "height", W.h, NULL);
  GstPad *qsrc = gst_element_get_static_pad(l->queue, "src");
  GstPadLinkReturn lr = gst_pad_link(qsrc, l->mixpad);
  gst_object_unref(qsrc);
  if (lr != GST_PAD_LINK_OK) { *err = g_strdup_printf("layer would not link to the mixer (%d)", lr); goto fail; }
  gst_element_sync_state_with_parent(l->queue);
  gst_element_sync_state_with_parent(l->caps);
  gst_element_sync_state_with_parent(l->convert);
  gst_element_sync_state_with_parent(l->upload);
  gst_element_sync_state_with_parent(l->src);
  gst_caps_unref(lc);
  l->shown_alpha = -1; l->shown_pts = GST_CLOCK_TIME_NONE; l->last_pts = GST_CLOCK_TIME_NONE; l->qos_at = GST_CLOCK_TIME_NONE;
  g_mutex_lock(&W.lock);
  l->id = ++W.layer_ids;
  W.layers = g_list_append(W.layers, l);
  g_mutex_unlock(&W.lock);
  l->pump = g_thread_new("glwall-pump", pump, l);
  return l;
fail:
  gst_caps_unref(lc);
  glwall_layer_free(l);
  return NULL;
}

void glwall_layer_set_alpha(glwall_layer *l, double a) {
  if (!l || !l->mixpad) return;
  g_mutex_lock(&W.lock);
  l->ramp.active = 0; /* a set level ends any fade */
  if (a != l->alpha) { l->alpha = a; l->steps++; }
  g_object_set(l->mixpad, "alpha", a, NULL);
  g_mutex_unlock(&W.lock);
}

void glwall_layer_ramp(glwall_layer *l, double from, double to, uint64_t start_ns, uint64_t dur_ns, int curve) {
  if (!l || !l->mixpad) return;
  g_mutex_lock(&W.lock);
  l->ramp.from = from; l->ramp.to = to; l->ramp.start = start_ns; l->ramp.dur = dur_ns; l->ramp.curve = curve;
  l->ramp.active = 1;
  g_mutex_unlock(&W.lock);
}

/* The picture is latched DELAY after its time and scanned out over the next
 * refresh; the middle of the screen lights half a refresh after the latch. */
int64_t glwall_display_delay(void) {
  gint64 d = W.delay_ns ? W.delay_ns : DEFAULT_DELAY_NS;
  return d + GST_SECOND / (2 * (W.hz ? W.hz : 60));
}

void glwall_align_audio(GstElement *sink) {
  g_object_set(sink, "ts-offset", (gint64)glwall_display_delay(), NULL);
}

/* The largest ts-offset on a pipeline's sinks other than the video appsink
 * (its sound), ns; -1 when it has none. */
int64_t glwall_audio_offset(GstElement *pipeline) {
  GstIterator *it = gst_bin_iterate_sinks(GST_BIN(pipeline));
  GValue v = G_VALUE_INIT;
  gint64 off = -1;
  while (gst_iterator_next(it, &v) == GST_ITERATOR_OK) {
    GstElement *e = g_value_get_object(&v);
    /* autoaudiosink is a bin that passes ts-offset to the sink inside it */
    if (!GST_IS_APP_SINK(e) && g_object_class_find_property(G_OBJECT_GET_CLASS(e), "ts-offset")) {
      gint64 o = 0;
      g_object_get(e, "ts-offset", &o, NULL);
      if (o > off) off = o;
    }
    g_value_reset(&v);
  }
  g_value_unset(&v);
  gst_iterator_free(it);
  return off;
}

uint64_t glwall_now(void) {
  GstClock *clk = gst_system_clock_obtain();
  GstClockTime t = gst_clock_get_time(clk);
  gst_object_unref(clk);
  return t;
}
void glwall_layer_set_crop(glwall_layer *l, int left, int right, int top, int bottom) {
  if (l && l->mixpad) g_object_set(l->mixpad, "crop-left", left, "crop-right", right, "crop-top", top, "crop-bottom", bottom, NULL);
}
void glwall_layer_set_zorder(glwall_layer *l, int z) { if (l && l->mixpad) g_object_set(l->mixpad, "zorder", (guint)z, NULL); }
void glwall_layer_set_rect(glwall_layer *l, int x, int y, int w, int h, int keep_aspect) {
  if (l && l->mixpad) g_object_set(l->mixpad, "xpos", x, "ypos", y, "width", w, "height", h, "sizing-policy", keep_aspect ? 1 : 0, NULL);
}

void glwall_use_system_clock(GstElement *pipeline) {
  GstClock *clk = gst_system_clock_obtain();
  gst_pipeline_use_clock(GST_PIPELINE(pipeline), clk);
  gst_object_unref(clk);
}

void glwall_layer_free(glwall_layer *l) {
  if (!l) return;
  /* Off the list first: the mixer's snapshot reads the pad under W.lock. */
  g_mutex_lock(&W.lock);
  W.layers = g_list_remove(W.layers, l);
  g_mutex_unlock(&W.lock);
  l->quit = 1;
  /* The pump may be blocked pushing into a full appsrc that nothing drains
   * any more (the cue is stopping): NULL makes the push return FLUSHING. */
  if (l->src) gst_element_set_state(l->src, GST_STATE_NULL);
  if (l->pump) g_thread_join(l->pump);
  /* Detach from the mixer first: releasing the pad deactivates it, which
   * wakes a push waiting in it, so the queue's task can stop. Stopping the
   * chain before that waited forever on a frame the mixer still held. */
  if (l->mixpad) {
    GstPad *qsrc = l->queue ? gst_element_get_static_pad(l->queue, "src") : NULL;
    if (qsrc) { gst_pad_unlink(qsrc, l->mixpad); gst_object_unref(qsrc); }
    gst_element_release_request_pad(W.mixer, l->mixpad);
    gst_object_unref(l->mixpad);
    l->mixpad = NULL;
  }
  GstElement *els[] = { l->queue, l->caps, l->convert, l->upload, l->src };
  for (int i = 0; i < 5; i++) if (els[i]) {
    gst_element_set_state(els[i], GST_STATE_NULL);
    if (GST_OBJECT_PARENT(els[i]) == GST_OBJECT(W.wall)) gst_bin_remove(GST_BIN(W.wall), els[i]);
    else gst_object_unref(els[i]);
  }
  if (l->lin) lin_unref(l->lin); /* the slots go when the mixer has released the last one */
  if (l->cue) gst_object_unref(l->cue);
  if (l->appsink) gst_object_unref(l->appsink);
  g_free(l);
}
