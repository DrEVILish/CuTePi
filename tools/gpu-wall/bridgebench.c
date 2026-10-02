/* bridgebench [-pool N] [-qmax N] "<cue 0 chain>" ["<cue 1 chain>" ...]
 * Each cue chain is a gst-launch description (source .. decoder); it runs in its
 * own pipeline ending in an appsink. The wall pipeline has one appsrc per cue ->
 * glupload -> glcolorconvert -> queue -> glvideomixerelement -> fakesink.
 * A C thread per layer pulls samples and pushes the buffers (shallow copy, no
 * pixel copy). Counts frames pulled, pushed and mixed. Headless throughput. */
#include <gst/gst.h>
#include <gst/app/gstappsink.h>
#include <gst/app/gstappsrc.h>
#include <gst/allocators/gstdmabuf.h>
#include <gst/video/video.h>
#include <gst/gl/gl.h>
#include <gst/gl/gstglfuncs.h>
#include <gst/gl/gstglbufferpool.h>
#include <gst/gl/gstglmemory.h>
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <GLES2/gl2ext.h>
#include <xf86drm.h>
#include <xf86drmMode.h>
#include <drm_fourcc.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <poll.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#define MAXL 4
static int pool_min = 8, copy_mode = 0; static const char *target = NULL; static const char *colorimetry = NULL; static GstElement *g_wall;
static guint64 mixed = 0, pulled[MAXL], pushed[MAXL];
static GHashTable *mems_seen[MAXL]; static guint64 nondma[MAXL]; static char nondma_type[MAXL][64]; /* distinct GstMemory objects per layer: pool reuse vs fresh import each frame */
typedef struct { GstElement *sink; GstElement *src; int i; } Layer;

/* ---- -kms: our own presenter (DESIGN §6.1.1 "Display", revised) ----------
 * Ring of linear dumb buffers on the HDMI card, imported into the wall's GL
 * context as AB24 EGLImages. The GL thread draws each mixed frame into a free
 * ring buffer; a presenter thread page-flips it and waits for the flip, so
 * the GL thread never waits for vblank. */
#define RING 4
typedef struct { uint32_t handle, pitch, fb; int dmafd; EGLImageKHR img; GLuint tex, fbo; int fence; } RingBuf;
static int use_fence = 1, copy_only = 0; static EGLDisplay gl_dpy;
static PFNEGLCREATESYNCKHRPROC mksync; static PFNEGLDESTROYSYNCKHRPROC rmsync; static PFNEGLDUPNATIVEFENCEFDANDROIDPROC dupfence;
static RingBuf ring[RING];
static int kfd = -1; static uint32_t crtc_id, conn_id; static drmModeModeInfo mode;
static GAsyncQueue *ready_q, *free_q;   /* ints (index+1) */
static GLuint prog; static GLint loc_t;
static guint64 presented = 0, gl_draws = 0;
static GstBuffer *cur_buf; static GLuint cur_tex; static int cur_idx;
static GLuint mkshader(GLenum type, const char *src) {
  GLuint sh = glCreateShader(type); glShaderSource(sh, 1, &src, NULL); glCompileShader(sh); return sh;
}
static int kms_open(void) {
  kfd = open("/dev/dri/card1", O_RDWR | O_CLOEXEC);
  if (kfd < 0 || (!copy_only && drmSetMaster(kfd))) { perror("card1 master"); return -1; }
  drmModeRes *r = drmModeGetResources(kfd);
  for (int i = 0; i < r->count_connectors; i++) {
    drmModeConnector *c = drmModeGetConnector(kfd, r->connectors[i]);
    if (c->connection == DRM_MODE_CONNECTED && c->count_modes) {
      conn_id = c->connector_id;
      for (int m = 0; m < c->count_modes; m++) if (c->modes[m].hdisplay == 1920 && c->modes[m].vdisplay == 1080 && c->modes[m].vrefresh == 60) { mode = c->modes[m]; break; }
      if (!mode.hdisplay) mode = c->modes[0];
      drmModeEncoder *e = drmModeGetEncoder(kfd, c->encoder_id);
      crtc_id = e ? e->crtc_id : r->crtcs[0];
      break;
    }
  }
  if (!conn_id && !copy_only) { fprintf(stderr, "no connected connector\n"); return -1; }
  for (int i = 0; i < RING; i++) {
    struct drm_mode_create_dumb cd = { .width = 1920, .height = 1080, .bpp = 32 };
    if (drmIoctl(kfd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) { perror("dumb"); return -1; }
    ring[i].handle = cd.handle; ring[i].pitch = cd.pitch;
    uint32_t h[4] = { cd.handle }, pt[4] = { cd.pitch }, of[4] = { 0 };
    if (drmModeAddFB2(kfd, 1920, 1080, DRM_FORMAT_XBGR8888, h, pt, of, &ring[i].fb, 0)) { perror("addfb2"); return -1; }
    if (drmPrimeHandleToFD(kfd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &ring[i].dmafd)) { perror("prime"); return -1; }
  }
  return 0;
}
/* On the wall's GL thread: import the ring and build the copy shader. */
static void gl_setup(GstGLContext *ctx, gpointer d) {
  EGLDisplay dpy = (EGLDisplay)gst_gl_display_get_handle(ctx->display); gl_dpy = dpy;
  mksync = (void *)eglGetProcAddress("eglCreateSyncKHR"); rmsync = (void *)eglGetProcAddress("eglDestroySyncKHR");
  dupfence = (void *)eglGetProcAddress("eglDupNativeFenceFDANDROID");
  if (use_fence && (!mksync || !dupfence || !strstr(eglQueryString(dpy, EGL_EXTENSIONS), "EGL_ANDROID_native_fence_sync"))) { fprintf(stderr, "no native fence sync: using glFinish\n"); use_fence = 0; }
  PFNEGLCREATEIMAGEKHRPROC mkimg = (void *)eglGetProcAddress("eglCreateImageKHR");
  PFNGLEGLIMAGETARGETTEXTURE2DOESPROC bindimg = (void *)eglGetProcAddress("glEGLImageTargetTexture2DOES");
  for (int i = 0; i < RING; i++) {
    EGLint ia[] = { EGL_WIDTH, 1920, EGL_HEIGHT, 1080, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
      EGL_DMA_BUF_PLANE0_FD_EXT, ring[i].dmafd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0, EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint)ring[i].pitch,
      EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, 0, EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, 0, EGL_NONE };
    ring[i].img = mkimg(dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, ia);
    if (ring[i].img == EGL_NO_IMAGE_KHR) { fprintf(stderr, "ring import failed 0x%x\n", eglGetError()); exit(3); }
    glGenTextures(1, &ring[i].tex); glBindTexture(GL_TEXTURE_2D, ring[i].tex); bindimg(GL_TEXTURE_2D, ring[i].img);
    glGenFramebuffers(1, &ring[i].fbo); glBindFramebuffer(GL_FRAMEBUFFER, ring[i].fbo);
    glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, ring[i].tex, 0);
    if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) { fprintf(stderr, "ring fbo incomplete\n"); exit(3); }
  }
  prog = glCreateProgram();
  glAttachShader(prog, mkshader(GL_VERTEX_SHADER, "attribute vec2 p; varying vec2 uv; void main(){ uv = p*0.5+0.5; gl_Position = vec4(p,0.0,1.0); }"));
  glAttachShader(prog, mkshader(GL_FRAGMENT_SHADER, "precision mediump float; varying vec2 uv; uniform sampler2D t; void main(){ gl_FragColor = texture2D(t, uv); }"));
  glBindAttribLocation(prog, 0, "p"); glLinkProgram(prog); loc_t = glGetUniformLocation(prog, "t");
  glBindFramebuffer(GL_FRAMEBUFFER, 0);
}
/* On the wall's GL thread: copy one mixed frame into ring[cur_idx]. */
static void gl_draw(GstGLContext *ctx, gpointer d) {
  static const GLfloat quad[] = { -1, -1, 1, -1, -1, 1, 1, 1 };
  glBindFramebuffer(GL_FRAMEBUFFER, ring[cur_idx].fbo); glViewport(0, 0, 1920, 1080);
  glUseProgram(prog); glActiveTexture(GL_TEXTURE0); glBindTexture(GL_TEXTURE_2D, cur_tex); glUniform1i(loc_t, 0);
  glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, quad); glEnableVertexAttribArray(0);
  glDrawArrays(GL_TRIANGLE_STRIP, 0, 4);
  ring[cur_idx].fence = -1;
  if (use_fence) {
    /* The GL thread moves on; the presenter waits for this fence before flipping. */
    EGLint at[] = { EGL_SYNC_NATIVE_FENCE_FD_ANDROID, EGL_NO_NATIVE_FENCE_FD_ANDROID, EGL_NONE };
    EGLSyncKHR sy = mksync(gl_dpy, EGL_SYNC_NATIVE_FENCE_ANDROID, at);
    glFlush();
    ring[cur_idx].fence = dupfence(gl_dpy, sy);
    rmsync(gl_dpy, sy);
  } else glFinish();
  glDisableVertexAttribArray(0); glUseProgram(0); glBindFramebuffer(GL_FRAMEBUFFER, 0);
  gl_draws++;
}
static void flip_done(int fd, unsigned int seq, unsigned int s, unsigned int us, void *data) { *(int *)data = 1; }
static gpointer present_thread(gpointer d) {
  int shown = -1;
  drmEventContext ev = { .version = 2, .page_flip_handler = flip_done };
  for (;;) {
    int idx = GPOINTER_TO_INT(g_async_queue_pop(ready_q)) - 1;
    if (idx < 0) return NULL;
    /* only the newest finished frame is shown; older ones go straight back */
    int more; while ((more = GPOINTER_TO_INT(g_async_queue_try_pop(ready_q))) > 0) { g_async_queue_push(free_q, GINT_TO_POINTER(idx + 1)); idx = more - 1; }
    if (ring[idx].fence >= 0) { struct pollfd ff = { .fd = ring[idx].fence, .events = POLLIN }; poll(&ff, 1, 1000); close(ring[idx].fence); ring[idx].fence = -1; }
    if (copy_only) { /* GPU copy done; pace like a 60 Hz flip, no display */
      g_usleep(16667); presented++;
      if (shown >= 0) g_async_queue_push(free_q, GINT_TO_POINTER(shown + 1));
      shown = idx; continue;
    }
    int done = 0;
    if (drmModePageFlip(kfd, crtc_id, ring[idx].fb, DRM_MODE_PAGE_FLIP_EVENT, &done)) { perror("pageflip"); g_async_queue_push(free_q, GINT_TO_POINTER(idx + 1)); continue; }
    struct pollfd pf = { .fd = kfd, .events = POLLIN };
    while (!done) { if (poll(&pf, 1, 100) > 0) drmHandleEvent(kfd, &ev); }
    presented++;
    if (shown >= 0) g_async_queue_push(free_q, GINT_TO_POINTER(shown + 1));
    shown = idx;
  }
}

/* ---- -ring: the mixer renders straight into the ring ----------------------
 * A GstGLBufferPool whose buffers wrap the ring textures (EGLImages over the
 * linear dumb buffers). Offered to the mixer in its allocation query, so its
 * output lands in scan-out memory with no copy pass and no driver-owned
 * 1080p render target (TEST_REPORT "The fresh-buffer mode"). */
typedef struct { GstGLBufferPool parent; } RingPool;
typedef struct { GstGLBufferPoolClass parent; } RingPoolClass;
static GType ring_pool_get_type(void);
G_DEFINE_TYPE(RingPool, ring_pool, GST_TYPE_GL_BUFFER_POOL)
static int ring_used[RING]; static GstVideoInfo ring_vinfo; static GstSample *ring_sample[RING];
static GstBufferPool *ring_pool_obj; static gboolean ring_imported;
static GstFlowReturn ring_pool_alloc(GstBufferPool *pool, GstBuffer **out, GstBufferPoolAcquireParams *ap) {
  int i; for (i = 0; i < RING && ring_used[i]; i++);
  if (i >= RING) { fprintf(stderr, "ring pool: no more buffers\n"); return GST_FLOW_ERROR; }
  GstGLContext *ctx = GST_GL_BUFFER_POOL(pool)->context;
  GstGLVideoAllocationParams *params = gst_gl_video_allocation_params_new_wrapped_gl_handle(ctx, NULL, &ring_vinfo, 0, NULL,
    GST_GL_TEXTURE_TARGET_2D, GST_GL_RGBA, GUINT_TO_POINTER(ring[i].tex), NULL, NULL);
  GstBuffer *b = gst_buffer_new(); gpointer wd[1] = { GUINT_TO_POINTER(ring[i].tex) };
  if (!gst_gl_memory_setup_buffer(gst_gl_memory_allocator_get_default(ctx), b, params, NULL, wd, 1)) {
    fprintf(stderr, "ring pool: setup_buffer failed\n"); return GST_FLOW_ERROR;
  }
  gst_gl_allocation_params_free((GstGLAllocationParams *)params);
  ring_used[i] = 1; *out = b; return GST_FLOW_OK;
}
static int ring_index_of(GstBuffer *b);
static void ring_pool_free(GstBufferPool *pool, GstBuffer *b) {
  int i = ring_index_of(b); if (i >= 0) ring_used[i] = 0;
  GST_BUFFER_POOL_CLASS(ring_pool_parent_class)->free_buffer(pool, b);
}
static void ring_pool_class_init(RingPoolClass *k) { ((GstBufferPoolClass *)k)->alloc_buffer = ring_pool_alloc; ((GstBufferPoolClass *)k)->free_buffer = ring_pool_free; }
static void ring_pool_init(RingPool *p) {}
static int ring_index_of(GstBuffer *b) {
  GstGLMemory *gm = (GstGLMemory *)gst_buffer_peek_memory(b, 0); guint t = gst_gl_memory_get_texture_id(gm);
  for (int i = 0; i < RING; i++) if (ring[i].tex == t) return i;
  return -1;
}
/* Mixer src pad: answer its allocation query with the ring pool. */
static GstPadProbeReturn ring_allocq(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
  GstQuery *q = GST_PAD_PROBE_INFO_QUERY(info);
  if (GST_QUERY_TYPE(q) != GST_QUERY_ALLOCATION) return GST_PAD_PROBE_OK;
  GstCaps *caps; gst_query_parse_allocation(q, &caps, NULL);
  if (!caps || !gst_video_info_from_caps(&ring_vinfo, caps)) return GST_PAD_PROBE_OK;
  GstGLContext *ctx = NULL; g_object_get(d, "context", &ctx, NULL);
  if (!ctx) { fprintf(stderr, "ring: mixer has no GL context yet\n"); return GST_PAD_PROBE_OK; }
  if (!ring_imported) { gst_gl_context_thread_add(ctx, gl_setup, NULL); ring_imported = TRUE; } /* import the ring once */
  if (!ring_pool_obj) {
    ring_pool_obj = g_object_new(ring_pool_get_type(), NULL);
    GST_GL_BUFFER_POOL(ring_pool_obj)->context = gst_object_ref(ctx);
    GstStructure *cfg = gst_buffer_pool_get_config(ring_pool_obj);
    gst_buffer_pool_config_set_params(cfg, caps, ring_vinfo.size, RING, RING);
    gst_buffer_pool_config_add_option(cfg, GST_BUFFER_POOL_OPTION_VIDEO_META);
    if (!gst_buffer_pool_set_config(ring_pool_obj, cfg)) fprintf(stderr, "ring: pool set_config refused\n");
  }
  gst_query_add_allocation_meta(q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_pool(q, ring_pool_obj, ring_vinfo.size, RING, RING);
  fprintf(stderr, "ring: pool offered to the mixer (%d buffers)\n", RING);
  gst_object_unref(ctx);
  return GST_PAD_PROBE_HANDLED;
}
/* On the GL thread: a native fence after the mixer's render of ring[cur_idx]. */
static void gl_fence(GstGLContext *ctx, gpointer d) {
  ring[cur_idx].fence = -1;
  if (use_fence) {
    EGLint at[] = { EGL_SYNC_NATIVE_FENCE_FD_ANDROID, EGL_NO_NATIVE_FENCE_FD_ANDROID, EGL_NONE };
    EGLSyncKHR sy = mksync(gl_dpy, EGL_SYNC_NATIVE_FENCE_ANDROID, at); glFlush();
    ring[cur_idx].fence = dupfence(gl_dpy, sy); rmsync(gl_dpy, sy);
  } else glFinish();
}
static gpointer ring_present_thread(gpointer d) {
  int shown = -1; drmEventContext ev = { .version = 2, .page_flip_handler = flip_done };
  for (;;) {
    int idx = GPOINTER_TO_INT(g_async_queue_pop(ready_q)) - 1;
    if (idx < 0) return NULL;
    if (ring[idx].fence >= 0) { struct pollfd ff = { .fd = ring[idx].fence, .events = POLLIN }; poll(&ff, 1, 1000); close(ring[idx].fence); ring[idx].fence = -1; }
    int done = 0;
    if (!copy_only && drmModePageFlip(kfd, crtc_id, ring[idx].fb, DRM_MODE_PAGE_FLIP_EVENT, &done)) { perror("pageflip"); gst_sample_unref(ring_sample[idx]); ring_sample[idx] = NULL; continue; }
    if (copy_only) g_usleep(16667); else { struct pollfd pf = { .fd = kfd, .events = POLLIN }; while (!done) { if (poll(&pf, 1, 100) > 0) drmHandleEvent(kfd, &ev); } }
    presented++;
    if (shown >= 0) { gst_sample_unref(ring_sample[shown]); ring_sample[shown] = NULL; } /* back to the pool */
    shown = idx;
  }
}
static int ring_run(GstElement *wall, GstElement *wout, double secs) {
  if (!copy_only && drmModeSetCrtc(kfd, crtc_id, ring[0].fb, 0, 0, &conn_id, 1, &mode)) { perror("setcrtc"); return 2; }
  ready_q = g_async_queue_new();
  g_thread_new("present", ring_present_thread, NULL);
  gint64 end = g_get_monotonic_time() + (gint64)(secs * G_USEC_PER_SEC);
  GstGLContext *wctx = NULL;
  while (g_get_monotonic_time() < end) {
    GstSample *ws = gst_app_sink_try_pull_sample(GST_APP_SINK(wout), 100 * GST_MSECOND);
    if (!ws) continue;
    GstBuffer *b = gst_sample_get_buffer(ws); int idx = ring_index_of(b);
    if (idx < 0) { static int warned; if (!warned++) fprintf(stderr, "ring: mixer output is not a ring buffer (pool not used)\n"); gst_sample_unref(ws); continue; }
    if (!wctx) wctx = ((GstGLBaseMemory *)gst_buffer_peek_memory(b, 0))->context;
    cur_idx = idx; gst_gl_context_thread_add(wctx, gl_fence, NULL);
    gl_draws++;
    ring_sample[idx] = ws; /* held until the buffer has left the screen */
    g_async_queue_push(ready_q, GINT_TO_POINTER(idx + 1));
  }
  return 0;
}

static int kms_run(GstElement *wall, GstElement *wout, double secs) {
  GstSample *first = gst_app_sink_pull_sample(GST_APP_SINK(wout));
  if (!first) { fprintf(stderr, "kms: no first sample\n"); return 2; }
  GstGLContext *wctx = ((GstGLBaseMemory *)gst_buffer_peek_memory(gst_sample_get_buffer(first), 0))->context;
  gst_gl_context_thread_add(wctx, gl_setup, NULL);
  if (!copy_only && drmModeSetCrtc(kfd, crtc_id, ring[0].fb, 0, 0, &conn_id, 1, &mode)) { perror("setcrtc"); return 2; }
  ready_q = g_async_queue_new(); free_q = g_async_queue_new();
  for (int i = 1; i < RING; i++) g_async_queue_push(free_q, GINT_TO_POINTER(i + 1));
  g_thread_new("present", present_thread, NULL);
  gst_sample_unref(first);
  gint64 end = g_get_monotonic_time() + (gint64)(secs * G_USEC_PER_SEC);
  while (g_get_monotonic_time() < end) {
    GstSample *ws = gst_app_sink_try_pull_sample(GST_APP_SINK(wout), 100 * GST_MSECOND);
    if (!ws) continue;
    int idx = GPOINTER_TO_INT(g_async_queue_timeout_pop(free_q, 100000)) - 1;
    if (idx < 0) { gst_sample_unref(ws); continue; } /* no free buffer: drop this mixed frame */
    GstGLMemory *gm = (GstGLMemory *)gst_buffer_peek_memory(gst_sample_get_buffer(ws), 0);
    cur_tex = gst_gl_memory_get_texture_id(gm); cur_idx = idx;
    gst_gl_context_thread_add(wctx, gl_draw, NULL);
    gst_sample_unref(ws);
    g_async_queue_push(ready_q, GINT_TO_POINTER(idx + 1));
  }
  return 0;
}

/* CPU time per thread at exit (utime+stime from /proc/self/task/<tid>/stat). */
static void thread_cpu_report(void) {
  GDir *d = g_dir_open("/proc/self/task", 0, NULL); const char *t;
  if (!d) return;
  for (int i = 0; i < MAXL && mems_seen[i]; i++) fprintf(stderr, "layer %d: %u distinct memory objects over %llu frames; %llu pushed buffers not dmabuf%s%s\n", i, g_hash_table_size(mems_seen[i]), (unsigned long long)pulled[i], (unsigned long long)nondma[i], nondma[i] ? ", first: " : "", nondma[i] ? nondma_type[i] : "");
  for (int i = 0; i < MAXL; i++) {
    char nm[8]; snprintf(nm, sizeof nm, "up%d", i);
    GstElement *up = g_wall ? gst_bin_get_by_name(GST_BIN(g_wall), nm) : NULL;
    if (!up) break;
    GstPad *sp = gst_element_get_static_pad(up, "src"); GstCaps *c = gst_pad_get_current_caps(sp);
    if (c) { GstStructure *st = gst_caps_get_structure(c, 0); fprintf(stderr, "layer %d upload: format %s target %s\n", i, gst_structure_get_string(st, "format"), gst_structure_get_string(st, "texture-target")); }
  }
  if (g_wall) { GstElement *mx = gst_bin_get_by_name(GST_BIN(g_wall), "m");
    for (int i = 0; mx && i < MAXL + 1; i++) { char pn[12]; snprintf(pn, sizeof pn, "sink_%d", i);
      GstPad *sp = gst_element_get_static_pad(mx, pn); if (!sp) break; GstCaps *c = gst_pad_get_current_caps(sp);
      if (c) { GstStructure *st = gst_caps_get_structure(c, 0); fprintf(stderr, "mixer %s: %s %s\n", pn, gst_structure_get_string(st, "format"), gst_structure_get_string(st, "texture-target")); } } }
  fprintf(stderr, "thread cpu:");
  while ((t = g_dir_read_name(d))) {
    gchar *path = g_strdup_printf("/proc/self/task/%s/stat", t), *buf = NULL;
    if (g_file_get_contents(path, &buf, NULL, NULL)) {
      char *rp = strrchr(buf, ')'); char comm[64] = ""; sscanf(buf, "%*d (%63[^)])", comm);
      unsigned long ut = 0, st = 0; if (rp) sscanf(rp + 2, "%*c %*d %*d %*d %*d %*d %*u %*u %*u %*u %*u %lu %lu", &ut, &st);
      if (ut + st >= 5) fprintf(stderr, " %s=%.1fu+%.1fs", comm, ut / 100.0, st / 100.0);
      g_free(buf);
    }
    g_free(path);
  }
  g_dir_close(d); fprintf(stderr, "\n");
}
static GstPadProbeReturn mixcount(GstPad *p, GstPadProbeInfo *i, gpointer d) { mixed++; return GST_PAD_PROBE_OK; }
/* Answer the decoder's allocation query: video meta + a pool-size hint. */
static GstPadProbeReturn allocq(GstPad *pad, GstPadProbeInfo *info, gpointer d) {
  GstQuery *q = GST_PAD_PROBE_INFO_QUERY(info);
  if (GST_QUERY_TYPE(q) != GST_QUERY_ALLOCATION) return GST_PAD_PROBE_OK;
  GstCaps *caps; gboolean need; gst_query_parse_allocation(q, &caps, &need);
  GstVideoInfo vi; guint size = 0;
  if (caps && gst_video_info_from_caps(&vi, caps)) size = vi.size;
  gst_query_add_allocation_meta(q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_pool(q, NULL, size, pool_min, 0);
  return GST_PAD_PROBE_HANDLED;
}
static gpointer pump(gpointer data) {
  Layer *l = data;
  for (;;) {
    GstSample *s = gst_app_sink_pull_sample(GST_APP_SINK(l->sink));
    if (!s) { gst_app_src_end_of_stream(GST_APP_SRC(l->src)); return NULL; }
    pulled[l->i]++;
    GstBuffer *orig = gst_sample_get_buffer(s), *b;
    if (copy_mode) b = gst_buffer_copy(orig); /* the spike's "shallow" copy */
    else { b = gst_buffer_ref(orig); }
    gst_sample_unref(s);
    if (!copy_mode) b = gst_buffer_make_writable(b); /* sole ref now: no copy, re-stampable */
    { GstMemory *m0 = gst_buffer_peek_memory(b, 0);
      if (!gst_is_dmabuf_memory(m0)) { if (!nondma[l->i]++) snprintf(nondma_type[l->i], 64, "%s (n_mem %u, size %zu)", m0->allocator->mem_type, gst_buffer_n_memory(b), gst_buffer_get_size(b)); } }
    if (!mems_seen[l->i]) mems_seen[l->i] = g_hash_table_new(NULL, NULL);
    g_hash_table_add(mems_seen[l->i], gst_buffer_peek_memory(b, 0));
    if (pulled[l->i] <= 2) {
      GstMemory *mo = gst_buffer_peek_memory(orig, 0), *mb = gst_buffer_peek_memory(b, 0);
      fprintf(stderr, "layer %d frame %llu: src mem %s dmabuf=%d noshare=%d -> pushed mem %s dmabuf=%d same=%d\n", l->i,
        (unsigned long long)pulled[l->i], mo->allocator->mem_type, gst_is_dmabuf_memory(mo), !!GST_MEMORY_FLAG_IS_SET(mo, GST_MEMORY_FLAG_NO_SHARE),
        mb->allocator->mem_type, gst_is_dmabuf_memory(mb), mo == mb);
    }
    if (gst_app_src_push_buffer(GST_APP_SRC(l->src), b) != GST_FLOW_OK) return NULL;
    pushed[l->i]++;
  }
}
int main(int argc, char **argv) {
  gst_init(&argc, &argv);
  int qmax = 3, a = 1, display = 0, cuesync = 0, split = 0, kms = 0, ringmode = 0, tiny = 0; long mixlat = 33000000; const char *dsink = "glimagesink sync=true";
  while (a < argc && argv[a][0] == '-') {
    if (!strcmp(argv[a], "-pool")) pool_min = atoi(argv[++a]);
    else if (!strcmp(argv[a], "-qmax")) qmax = atoi(argv[++a]);
    else if (!strcmp(argv[a], "-display")) display = 1;
    else if (!strcmp(argv[a], "-cuesync")) cuesync = 1;
    else if (!strcmp(argv[a], "-sink")) dsink = argv[++a];
    else if (!strcmp(argv[a], "-split")) split = 1;
    else if (!strcmp(argv[a], "-kms")) { kms = 1; display = 1; }
    else if (!strcmp(argv[a], "-ring")) { kms = 1; display = 1; ringmode = 1; }
    else if (!strcmp(argv[a], "-tiny")) tiny = 1;
    else if (!strcmp(argv[a], "-finish")) use_fence = 0;
    else if (!strcmp(argv[a], "-copy")) copy_mode = 1;
    else if (!strcmp(argv[a], "-target")) target = argv[++a];
    else if (!strcmp(argv[a], "-colorimetry")) colorimetry = argv[++a];
    else if (!strcmp(argv[a], "-copyonly")) { kms = 1; display = 1; copy_only = 1; }
    else if (!strcmp(argv[a], "-mixlat")) mixlat = atol(argv[++a]) * 1000000L;
    a++;
  }
  int n = argc - a; if (n < 1 || n > MAXL) { fprintf(stderr, "1..%d cue chains\n", MAXL); return 2; }
  /* -display: real time on HDMI. A live GPU black source paces the mixer at
   * 60 fps (bridge rule), glimagesink presents, every pipeline shares one
   * clock and base time so cue timestamps are wall timestamps. */
  GString *w = display ? g_string_new(NULL) : NULL;
  if (display) g_string_printf(w, "%s ! m.sink_0 "
      "glvideomixerelement name=m background=black latency=%ld sink_0::width=1920 sink_0::height=1080 ! "
      "video/x-raw(memory:GLMemory),width=1920,height=1080,framerate=60/1,format=RGBA ! "
      "identity name=wsink ! %s",
      tiny ? "videotestsrc is-live=true pattern=black ! video/x-raw,width=16,height=16,framerate=60/1 ! glupload ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA"
           : "gltestsrc is-live=true pattern=black ! video/x-raw(memory:GLMemory),width=1920,height=1080,framerate=60/1",
      mixlat, (split || kms) ? (kms ? (ringmode ? "appsink name=wout sync=true max-buffers=1 drop=false enable-last-sample=false" : "appsink name=wout sync=true max-buffers=2 drop=false") : "appsink name=wout sync=false max-buffers=2 drop=false") : dsink);
  else w = g_string_new(
      "glvideomixerelement name=m background=black ! "
      "video/x-raw(memory:GLMemory),width=1920,height=1080,format=RGBA ! fakesink name=wsink sync=false");
  for (int i = 0; i < n; i++) {
    g_string_append_printf(w, " appsrc name=src%d format=time max-buffers=%d block=true ! glupload name=up%d ! ", i, qmax, i);
    if (target) g_string_append_printf(w, "video/x-raw(memory:GLMemory),texture-target=%s ! ", target);
    g_string_append_printf(w, "glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA ! queue max-size-buffers=2 name=out%d", i);
  }
  GError *err = NULL;
  GstElement *wall = gst_parse_launch(w->str, &err);
  if (!wall || err) { fprintf(stderr, "wall: %s\n", err ? err->message : "parse failed"); return 2; }
  g_wall = wall;
  GstElement *m = gst_bin_get_by_name(GST_BIN(wall), "m");
  if (ringmode) {
    if (kms_open()) return 2;
    gst_pad_add_probe(gst_element_get_static_pad(m, "src"), GST_PAD_PROBE_TYPE_QUERY_DOWNSTREAM, ring_allocq, m, NULL);
  }
  Layer L[MAXL]; GstElement *cue[MAXL];
  for (int i = 0; i < n; i++) {
    char nm[16]; snprintf(nm, sizeof nm, "out%d", i);
    /* (with -display the black source holds sink_0; cue pads follow) */
    GstPad *src = gst_element_get_static_pad(gst_bin_get_by_name(GST_BIN(wall), nm), "src");
    gst_pad_link_full(src, gst_element_request_pad_simple(m, "sink_%u"), GST_PAD_LINK_CHECK_NOTHING);
    GString *c = g_string_new(argv[a + i]);
    g_string_append_printf(c, " ! appsink name=sink sync=%s max-buffers=2 drop=false enable-last-sample=false", (display && cuesync) ? "true" : "false");
    cue[i] = gst_parse_launch(c->str, &err);
    if (!cue[i] || err) { fprintf(stderr, "cue %d: %s\n", i, err ? err->message : "parse failed"); return 2; }
    L[i].sink = gst_bin_get_by_name(GST_BIN(cue[i]), "sink");
    snprintf(nm, sizeof nm, "src%d", i);
    L[i].src = gst_bin_get_by_name(GST_BIN(wall), nm); L[i].i = i;
    gst_pad_add_probe(gst_element_get_static_pad(L[i].sink, "sink"), GST_PAD_PROBE_TYPE_QUERY_DOWNSTREAM, allocq, NULL, NULL);
  }
  gst_pad_add_probe(gst_element_get_static_pad(gst_bin_get_by_name(GST_BIN(wall), "wsink"), "sink"),
    GST_PAD_PROBE_TYPE_BUFFER, mixcount, NULL, NULL);
  /* Bridge rule: a layer joins only once its caps are known. Preroll each cue,
   * take the caps from its preroll sample, then start the wall. */
  for (int i = 0; i < n; i++) {
    gst_element_set_state(cue[i], GST_STATE_PAUSED);
    if (gst_element_get_state(cue[i], NULL, NULL, 10 * GST_SECOND) == GST_STATE_CHANGE_FAILURE) { fprintf(stderr, "cue %d preroll failed\n", i); return 2; }
    GstSample *ps = gst_app_sink_pull_preroll(GST_APP_SINK(L[i].sink));
    if (!ps) { fprintf(stderr, "cue %d no preroll sample\n", i); return 2; }
    GstCaps *lc = gst_caps_copy(gst_sample_get_caps(ps));
    /* A YUV layer needs its colour matrix for the GPU conversion (the ISP's
     * caps carry none; the H.264 decoder's say bt709): the bridge supplies it. */
    if (colorimetry && !gst_structure_has_field(gst_caps_get_structure(lc, 0), "colorimetry"))
      gst_caps_set_simple(lc, "colorimetry", G_TYPE_STRING, colorimetry, NULL);
    fprintf(stderr, "layer %d caps: %s\n", i, gst_caps_to_string(lc));
    gst_app_src_set_caps(GST_APP_SRC(L[i].src), lc); gst_caps_unref(lc);
    gst_sample_unref(ps);
  }
  gint64 t0 = g_get_monotonic_time();
  if (display) {
    GstClock *clk = gst_system_clock_obtain();
    GstClockTime base = gst_clock_get_time(clk) + 200 * GST_MSECOND;
    gst_pipeline_use_clock(GST_PIPELINE(wall), clk);
    gst_element_set_start_time(wall, GST_CLOCK_TIME_NONE); gst_element_set_base_time(wall, base);
    for (int i = 0; i < n; i++) {
      gst_pipeline_use_clock(GST_PIPELINE(cue[i]), clk);
      gst_element_set_start_time(cue[i], GST_CLOCK_TIME_NONE); gst_element_set_base_time(cue[i], base);
    }
  }
  gst_element_set_state(wall, GST_STATE_PLAYING);
  for (int i = 0; i < n; i++) { gst_element_set_state(cue[i], GST_STATE_PLAYING); g_thread_new("pump", pump, &L[i]); }
  if (kms) {
    if (!ringmode && kms_open()) return 2;
    int rc = ringmode ? ring_run(wall, gst_bin_get_by_name(GST_BIN(wall), "wout"), 9.0) : kms_run(wall, gst_bin_get_by_name(GST_BIN(wall), "wout"), 9.0);
    double s = (g_get_monotonic_time() - t0) / 1e6;
    printf("kms: mixer out %llu, drawn %llu, presented %llu in %.2f s;", (unsigned long long)mixed, (unsigned long long)gl_draws, (unsigned long long)presented, s);
    for (int i = 0; i < n; i++) printf(" layer %d pulled %llu pushed %llu;", i, (unsigned long long)pulled[i], (unsigned long long)pushed[i]);
    printf("\n"); thread_cpu_report(); fflush(stdout); fflush(stderr); _exit(rc);
  }
  if (display && split) {
    /* Presenter: its own pipeline, appsrc -> glimagesink. Given the wall's GL
     * display and context as the one to share with, glimagesink makes its own
     * context (own thread), so a present waiting for vblank no longer blocks
     * the wall's upload and mixing. */
    GstElement *wout = gst_bin_get_by_name(GST_BIN(wall), "wout");
    fprintf(stderr, "split: waiting for first mixed frame\n");
    GstSample *first = gst_app_sink_pull_sample(GST_APP_SINK(wout));
    if (!first) { fprintf(stderr, "split: no first sample\n"); return 2; }
    GstMemory *mem = gst_buffer_peek_memory(gst_sample_get_buffer(first), 0);
    if (!gst_is_gl_memory(mem)) { fprintf(stderr, "wall output is not GL memory\n"); return 2; }
    GstGLContext *wctx = ((GstGLBaseMemory *)mem)->context;
    gchar *pdesc = g_strdup_printf("appsrc name=psrc format=time is-live=true max-buffers=2 block=true ! %s", dsink);
    GstElement *pres = gst_parse_launch(pdesc, &err);
    if (!pres) { fprintf(stderr, "presenter: %s\n", err->message); return 2; }
    fprintf(stderr, "split: wall ctx %p display %p\n", (void *)wctx, wctx ? (void *)wctx->display : NULL);
    GstContext *dc = gst_context_new(GST_GL_DISPLAY_CONTEXT_TYPE, TRUE);
    gst_context_set_gl_display(dc, wctx->display);
    GstContext *ac = gst_context_new("gst.gl.app_context", TRUE);
    gst_structure_set(gst_context_writable_structure(ac), "context", GST_TYPE_GL_CONTEXT, wctx, NULL);
    gst_element_set_context(pres, dc); gst_element_set_context(pres, ac);
    GstElement *psrc = gst_bin_get_by_name(GST_BIN(pres), "psrc");
    gst_app_src_set_caps(GST_APP_SRC(psrc), gst_sample_get_caps(first));
    GstClock *clk = gst_system_clock_obtain();
    gst_pipeline_use_clock(GST_PIPELINE(pres), clk);
    gst_element_set_start_time(pres, GST_CLOCK_TIME_NONE); gst_element_set_base_time(pres, gst_element_get_base_time(wall));
    gst_element_set_state(pres, GST_STATE_PLAYING);
    gst_app_src_push_buffer(GST_APP_SRC(psrc), gst_buffer_ref(gst_sample_get_buffer(first)));
    gst_sample_unref(first);
    gint64 end = g_get_monotonic_time() + 9 * G_USEC_PER_SEC;
    while (g_get_monotonic_time() < end) {
      GstSample *ws = gst_app_sink_try_pull_sample(GST_APP_SINK(wout), 100 * GST_MSECOND);
      if (!ws) continue;
      gst_app_src_push_buffer(GST_APP_SRC(psrc), gst_buffer_ref(gst_sample_get_buffer(ws)));
      gst_sample_unref(ws);
    }
    GstMessage *pm = gst_bus_pop_filtered(gst_element_get_bus(pres), GST_MESSAGE_ERROR);
    if (pm) { GError *e; gst_message_parse_error(pm, &e, NULL); fprintf(stderr, "presenter error: %s\n", e->message); }
    double s = (g_get_monotonic_time() - t0) / 1e6;
    printf("split: mixer out %llu in %.2f s = %.1f fps;", (unsigned long long)mixed, s, mixed / s);
    for (int i = 0; i < n; i++) printf(" layer %d pulled %llu pushed %llu;", i, (unsigned long long)pulled[i], (unsigned long long)pushed[i]);
    printf("\n"); thread_cpu_report(); fflush(stdout); fflush(stderr); _exit(0);
  }
  if (display) { /* the live black source never ends: run for the clip length */
    g_usleep(9 * G_USEC_PER_SEC);
    double s = (g_get_monotonic_time() - t0) / 1e6;
    printf("display: mixer out %llu in %.2f s = %.1f fps;", (unsigned long long)mixed, s, mixed / s);
    for (int i = 0; i < n; i++) printf(" layer %d pulled %llu pushed %llu;", i, (unsigned long long)pulled[i], (unsigned long long)pushed[i]);
    printf("\n"); thread_cpu_report(); fflush(stdout); _exit(0);
  }
  GstMessage *msg = gst_bus_timed_pop_filtered(gst_element_get_bus(wall), 120 * GST_SECOND, GST_MESSAGE_EOS | GST_MESSAGE_ERROR);
  double s = (g_get_monotonic_time() - t0) / 1e6;
  if (msg && GST_MESSAGE_TYPE(msg) == GST_MESSAGE_ERROR) { GError *e; gst_message_parse_error(msg, &e, NULL); fprintf(stderr, "wall error: %s\n", e->message); }
  for (int i = 0; i < n; i++) {
    GstMessage *cm = gst_bus_pop_filtered(gst_element_get_bus(cue[i]), GST_MESSAGE_ERROR);
    if (cm) { GError *e; gst_message_parse_error(cm, &e, NULL); fprintf(stderr, "cue %d error: %s\n", i, e->message); }
  }
  printf("mixed %llu in %.2f s = %.1f fps;", (unsigned long long)mixed, s, mixed / s);
  for (int i = 0; i < n; i++) printf(" layer %d pulled %llu pushed %llu;", i, (unsigned long long)pulled[i], (unsigned long long)pushed[i]);
  printf("\n");
  fflush(stdout); fflush(stderr);
  _exit(0);
}
