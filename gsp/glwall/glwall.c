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
#include <gst/gl/gl.h>
#include <gst/gl/gstglbufferpool.h>
#include <gst/gl/gstglfuncs.h>
#include <gst/gl/gstglmemory.h>
#include <gst/video/video.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include <xf86drm.h>
#include <xf86drmMode.h>

#define RING 4
#define MIXER_LATENCY_NS (33 * GST_MSECOND)

typedef struct {
  uint32_t handle, pitch, fb;
  int dmafd, fence;
  EGLImageKHR img;
  GLuint tex;
  GstSample *sample; /* the mixed frame living in this buffer, held until it has left the screen */
} ring_buf;

static struct {
  int open;
  int fd; /* the service's DRM fd (not ours to close) */
  uint32_t crtc, plane;
  int w, h, hz;
  ring_buf ring[RING];
  int ring_used[RING];
  int ring_imported;
  GstVideoInfo ring_vinfo;
  GstBufferPool *pool;
  GstElement *wall, *mixer, *wout;
  GstGLContext *ctx;
  EGLDisplay dpy;
  int use_fence;
  GAsyncQueue *ready; /* ring index + 1, in presentation order */
  GThread *wall_thread, *present_thread;
  volatile int quit;
  guint64 mixed, presented;
  GMutex lock; /* layers list, zorder */
} W;

struct glwall_layer {
  GstElement *appsink, *cue; /* the cue pipeline (parent of its appsink) */
  GstElement *src, *upload, *convert, *caps, *queue;
  GstPad *mixpad;
  GThread *pump;
  volatile int quit;
  guint64 pulled, pushed;
};

static PFNEGLCREATESYNCKHRPROC p_mksync;
static PFNEGLDESTROYSYNCKHRPROC p_rmsync;
static PFNEGLDUPNATIVEFENCEFDANDROIDPROC p_dupfence;
static PFNEGLCREATEIMAGEKHRPROC p_mkimg;
static PFNEGLDESTROYIMAGEKHRPROC p_rmimg;
static PFNGLEGLIMAGETARGETTEXTURE2DOESPROC p_bindimg;

/* ---- ring: dumb buffers on the display card ------------------------------ */

static int ring_create(char **err) {
  for (int i = 0; i < RING; i++) {
    struct drm_mode_create_dumb cd = { .width = W.w, .height = W.h, .bpp = 32 };
    if (drmIoctl(W.fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) { *err = g_strdup_printf("create dumb buffer: %s", g_strerror(errno)); return -1; }
    W.ring[i].handle = cd.handle; W.ring[i].pitch = cd.pitch; W.ring[i].fence = -1;
    uint32_t h[4] = { cd.handle }, p[4] = { cd.pitch }, o[4] = { 0 };
    if (drmModeAddFB2(W.fd, W.w, W.h, DRM_FORMAT_XBGR8888, h, p, o, &W.ring[i].fb, 0)) { *err = g_strdup_printf("addfb2: %s", g_strerror(errno)); return -1; }
    if (drmPrimeHandleToFD(W.fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &W.ring[i].dmafd)) { *err = g_strdup_printf("prime export: %s", g_strerror(errno)); return -1; }
  }
  return 0;
}

static void ring_destroy(void) {
  for (int i = 0; i < RING; i++) {
    ring_buf *r = &W.ring[i];
    if (r->fence >= 0) close(r->fence);
    if (r->fb) drmModeRmFB(W.fd, r->fb);
    if (r->dmafd > 0) close(r->dmafd);
    if (r->handle) { struct drm_mode_destroy_dumb dd = { .handle = r->handle }; drmIoctl(W.fd, DRM_IOCTL_MODE_DESTROY_DUMB, &dd); }
    memset(r, 0, sizeof *r); r->fence = -1;
  }
}

/* On the wall's GL thread: import the ring as textures (once). */
static void gl_setup(GstGLContext *ctx, gpointer d) {
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

/* On the GL thread: a native fence after the mixer's render into ring[i]. */
static void gl_fence(GstGLContext *ctx, gpointer d) {
  ring_buf *r = &W.ring[GPOINTER_TO_INT(d)];
  if (r->fence >= 0) { close(r->fence); r->fence = -1; }
  if (W.use_fence) {
    EGLint at[] = { EGL_SYNC_NATIVE_FENCE_FD_ANDROID, EGL_NO_NATIVE_FENCE_FD_ANDROID, EGL_NONE };
    EGLSyncKHR sy = p_mksync(W.dpy, EGL_SYNC_NATIVE_FENCE_ANDROID, at);
    glFlush();
    r->fence = p_dupfence(W.dpy, sy);
    p_rmsync(W.dpy, sy);
  } else {
    glFinish();
  }
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

static GstFlowReturn ring_pool_alloc(GstBufferPool *pool, GstBuffer **out, GstBufferPoolAcquireParams *ap) {
  int i; for (i = 0; i < RING && W.ring_used[i]; i++);
  if (i >= RING) { GST_ERROR("glwall: ring pool exhausted"); return GST_FLOW_ERROR; }
  GstGLContext *ctx = GST_GL_BUFFER_POOL(pool)->context;
  GstGLVideoAllocationParams *params = gst_gl_video_allocation_params_new_wrapped_gl_handle(ctx, NULL, &W.ring_vinfo, 0, NULL,
    GST_GL_TEXTURE_TARGET_2D, GST_GL_RGBA, GUINT_TO_POINTER(W.ring[i].tex), NULL, NULL);
  GstBuffer *b = gst_buffer_new(); gpointer wd[1] = { GUINT_TO_POINTER(W.ring[i].tex) };
  if (!gst_gl_memory_setup_buffer(gst_gl_memory_allocator_get_default(ctx), b, params, NULL, wd, 1)) {
    gst_gl_allocation_params_free((GstGLAllocationParams *)params); gst_buffer_unref(b);
    return GST_FLOW_ERROR;
  }
  gst_gl_allocation_params_free((GstGLAllocationParams *)params);
  W.ring_used[i] = 1; *out = b;
  return GST_FLOW_OK;
}

static void ring_pool_free(GstBufferPool *pool, GstBuffer *b) {
  int i = ring_index_of(b); if (i >= 0) W.ring_used[i] = 0;
  GST_BUFFER_POOL_CLASS(ring_pool_parent_class)->free_buffer(pool, b);
}

static void ring_pool_class_init(RingPoolClass *k) {
  ((GstBufferPoolClass *)k)->alloc_buffer = ring_pool_alloc;
  ((GstBufferPoolClass *)k)->free_buffer = ring_pool_free;
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
  if (!W.ring_imported) gst_gl_context_thread_add(ctx, gl_setup, NULL);
  if (!W.ctx) W.ctx = gst_object_ref(ctx);
  if (!W.pool) {
    W.pool = g_object_new(ring_pool_get_type(), NULL);
    GST_GL_BUFFER_POOL(W.pool)->context = gst_object_ref(ctx);
    GstStructure *cfg = gst_buffer_pool_get_config(W.pool);
    gst_buffer_pool_config_set_params(cfg, caps, W.ring_vinfo.size, RING, RING);
    gst_buffer_pool_config_add_option(cfg, GST_BUFFER_POOL_OPTION_VIDEO_META);
    if (!gst_buffer_pool_set_config(W.pool, cfg)) GST_ERROR("glwall: ring pool config refused");
  }
  gst_query_add_allocation_meta(q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_pool(q, W.pool, W.ring_vinfo.size, RING, RING);
  gst_object_unref(ctx);
  return GST_PAD_PROBE_HANDLED;
}

/* ---- wall and presenter threads -------------------------------------------- */

/* Pulls each mixed frame (in a ring buffer) as it falls due, fences it and
 * hands it to the presenter. The sample is held until the buffer has left
 * the screen, which is what keeps the mixer from reusing it. */
static gpointer wall_thread(gpointer d) {
  while (!W.quit) {
    GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(W.wout), 100 * GST_MSECOND);
    if (!s) continue;
    int idx = ring_index_of(gst_sample_get_buffer(s));
    if (idx < 0) { GST_WARNING("glwall: mixed frame not in the ring"); gst_sample_unref(s); continue; }
    gst_gl_context_thread_add(W.ctx, gl_fence, GINT_TO_POINTER(idx));
    W.mixed++;
    W.ring[idx].sample = s;
    g_async_queue_push(W.ready, GINT_TO_POINTER(idx + 1));
  }
  return NULL;
}

static gpointer present_thread(gpointer d) {
  int shown = -1;
  for (;;) {
    gpointer p = g_async_queue_timeout_pop(W.ready, 100000);
    if (W.quit) break;
    if (!p) continue;
    int idx = GPOINTER_TO_INT(p) - 1;
    ring_buf *r = &W.ring[idx];
    if (r->fence >= 0) { struct pollfd ff = { .fd = r->fence, .events = POLLIN }; poll(&ff, 1, 1000); close(r->fence); r->fence = -1; }
    /* Legacy SetPlane: one commit, returns when the frame is latched. */
    if (drmModeSetPlane(W.fd, W.plane, W.crtc, r->fb, 0, 0, 0, W.w, W.h, 0, 0, (uint32_t)W.w << 16, (uint32_t)W.h << 16)) {
      GST_WARNING("glwall: setplane: %s", g_strerror(errno));
    } else {
      W.presented++;
    }
    if (shown >= 0 && W.ring[shown].sample) { gst_sample_unref(W.ring[shown].sample); W.ring[shown].sample = NULL; }
    shown = idx;
  }
  if (shown >= 0 && W.ring[shown].sample) { gst_sample_unref(W.ring[shown].sample); W.ring[shown].sample = NULL; }
  return NULL;
}

/* ---- open / close ---------------------------------------------------------- */

int glwall_is_open(void) { return W.open; }

int glwall_open(int drm_fd, uint32_t crtc_id, uint32_t plane_id, int width, int height, int refresh_hz, char **err) {
  *err = NULL;
  if (W.open) return 0;
  memset(&W, 0, sizeof W);
  for (int i = 0; i < RING; i++) W.ring[i].fence = -1;
  W.fd = drm_fd; W.crtc = crtc_id; W.plane = plane_id; W.w = width; W.h = height; W.hz = refresh_hz > 0 ? refresh_hz : 60;
  g_mutex_init(&W.lock);
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
  W.ready = g_async_queue_new();
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
  for (int i = 0; i < RING; i++) if (W.ring[i].sample) { gst_sample_unref(W.ring[i].sample); W.ring[i].sample = NULL; }
  drmModeSetPlane(W.fd, W.plane, W.crtc, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0); /* plane off */
  if (W.pool) { gst_buffer_pool_set_active(W.pool, FALSE); gst_object_unref(W.pool); }
  if (W.ctx) gst_object_unref(W.ctx);
  if (W.mixer) gst_object_unref(W.mixer);
  if (W.wout) gst_object_unref(W.wout);
  gst_object_unref(W.wall);
  g_async_queue_unref(W.ready);
  ring_destroy();
  W.open = 0;
}

void glwall_stats(uint64_t *mixed, uint64_t *presented) { *mixed = W.mixed; *presented = W.presented; }
void glwall_layer_stats(glwall_layer *l, uint64_t *pulled, uint64_t *pushed) { *pulled = l ? l->pulled : 0; *pushed = l ? l->pushed : 0; }

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

/* Per layer: pull, re-stamp onto the wall clock, push. The shallow
 * make-writable keeps the decoder's memory (one ref), so nothing is copied. */
static gpointer pump(gpointer data) {
  glwall_layer *l = data;
  while (!l->quit) {
    GstSample *s = gst_app_sink_try_pull_sample(GST_APP_SINK(l->appsink), 200 * GST_MSECOND);
    if (!s) { if (gst_app_sink_is_eos(GST_APP_SINK(l->appsink))) break; continue; }
    l->pulled++;
    GstBuffer *b = gst_buffer_ref(gst_sample_get_buffer(s));
    const GstSegment *seg = gst_sample_get_segment(s);
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
  l->pump = g_thread_new("glwall-pump", pump, l);
  return l;
fail:
  gst_caps_unref(lc);
  glwall_layer_free(l);
  return NULL;
}

void glwall_layer_set_alpha(glwall_layer *l, double a) { if (l && l->mixpad) g_object_set(l->mixpad, "alpha", a, NULL); }
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
  l->quit = 1;
  if (l->pump) g_thread_join(l->pump);
  if (l->mixpad) {
    GstPad *qsrc = l->queue ? gst_element_get_static_pad(l->queue, "src") : NULL;
    if (qsrc) { gst_pad_unlink(qsrc, l->mixpad); gst_object_unref(qsrc); }
    gst_element_release_request_pad(W.mixer, l->mixpad);
    gst_object_unref(l->mixpad);
  }
  GstElement *els[] = { l->src, l->upload, l->convert, l->caps, l->queue };
  for (int i = 0; i < 5; i++) if (els[i]) {
    gst_element_set_state(els[i], GST_STATE_NULL);
    if (GST_OBJECT_PARENT(els[i]) == GST_OBJECT(W.wall)) gst_bin_remove(GST_BIN(W.wall), els[i]);
    else gst_object_unref(els[i]);
  }
  if (l->cue) gst_object_unref(l->cue);
  if (l->appsink) gst_object_unref(l->appsink);
  g_free(l);
}
