/* planewall: the plane wall's presenter and its sink (DESIGN §6.1.4).
 *
 * One presenter thread owns every overlay plane CuTePi shows cues on. Each
 * display refresh it makes ONE atomic commit carrying, for every plane that
 * changed, its newest frame and its opacity, z-order, rotation, source crop
 * and on-screen rectangle. kmssink did the same with a blocking legacy
 * SetPlane per frame per plane plus a commit per alpha write, all sharing the
 * CRTC's 60 commits a second: two video layers got ~30 frames each and fades
 * stepped 30 times a second (TEST_REPORT O1). The display controller itself
 * takes 8 full-screen layers with new pictures and opacities every refresh
 * (PERFORMANCE_PROPOSALS.md).
 *
 * cutepiplanesink (a GstVideoSink, clock-synced) hands each due frame to its
 * plane: decoder DMABufs become framebuffers directly (cached per memory, no
 * copy); system-memory frames are copied into a ring of scanout buffers.
 * Plane properties written by the service (alpha, zpos, rotation, blend
 * mode) go to planewall_set() and land in the next commit. The presenter
 * commits only when something changed; a blocking commit returns at the
 * vblank that latched it, which paces it. A frame is released once the
 * commit that replaced it has returned (it is off the screen then). */
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <sched.h>
#include <stdint.h>
#include <string.h>
#include <sys/mman.h>
#include <xf86drm.h>
#include <xf86drmMode.h>
#include <drm_fourcc.h>
#include <gst/gst.h>
#include <gst/allocators/gstdmabuf.h>
#include <gst/allocators/gstfdmemory.h>
#include <gst/video/video.h>
#include <gst/video/gstvideosink.h>
#include <gst/video/videooverlay.h>

#include "planewall.h"

GST_DEBUG_CATEGORY_STATIC (planewall_debug);
#define GST_CAT_DEFAULT planewall_debug

#define MAXP 16
#define RING 4                  /* scanout buffers per plane for copied frames */
#define IVN 2048                /* commit intervals kept for percentiles */

/* A frame ready to be shown: the buffer (kept referenced while it may be on
 * screen), its framebuffer and how it sits on the plane. */
typedef struct {
  GstBuffer *buf;               /* decoder frame (NULL for a copied frame) */
  int ring;                     /* ring slot of a copied frame, else -1 */
  uint32_t fb;
  uint32_t sx, sy, sw, sh;      /* source rectangle, pixels */
  int cx, cy, cw, ch;           /* on-screen rectangle */
  int yuv;
  uint64_t enc, range;          /* COLOR_ENCODING / COLOR_RANGE enum values */
} frame;

typedef struct {
  uint32_t fb;
  uint8_t *map;
  size_t size;
  uint32_t handle;
  int busy;                     /* queued or on screen */
} ring_buf;

typedef struct {
  uint32_t id;
  int used;
  /* property ids */
  uint32_t p_fb, p_crtc, p_sx, p_sy, p_sw, p_sh, p_cx, p_cy, p_cw, p_ch;
  uint32_t p_alpha, p_zpos, p_rot, p_blend, p_enc, p_range;
  uint64_t enc601, enc709, enc2020, rlimited, rfull;
  /* wanted state (under P.lock) */
  uint64_t alpha, zpos, rot, blend;
  int props_dirty;
  int attached;                 /* a sink feeds it */
  frame *pending;               /* newest frame not yet committed */
  /* presenter-owned */
  frame *shown;                 /* on screen (or committed) */
  int on;                       /* plane enabled on the CRTC */
  /* copied frames */
  ring_buf ring[RING];
  int ring_n;
  GstVideoInfo ring_info;
  /* formats it scans out: fourcc + modifier */
  uint32_t *fmt;
  uint64_t *mod;
  int nfmt;
  /* stats */
  uint64_t shown_frames, dropped;
  uint64_t steps;               /* commits that changed its opacity on screen */
  uint64_t alpha_shown;         /* alpha of the last commit that carried it */
  int first_seen;
} plane_st;

static struct {
  int open, fd, w, h, hz;
  uint32_t crtc;
  GMutex lock;
  GCond cond, done;
  int dirty, quit;
  uint64_t gen;                 /* commits done (detach waits on it) */
  GThread *th;
  plane_st pl[MAXP];
  int npl;
  uint64_t commits, fails;
  int last_errno;
  double iv[IVN];
  int niv;
  gint64 last_us;
} P;

static GQuark fb_quark;

static plane_st *plane_of (uint32_t id) {
  for (int i = 0; i < P.npl; i++)
    if (P.pl[i].id == id)
      return &P.pl[i];
  return NULL;
}

static uint32_t prop_id (uint32_t obj, const char *name, const char *e1, uint64_t *v1, const char *e2,
    uint64_t *v2, const char *e3, uint64_t *v3) {
  drmModeObjectProperties *pp = drmModeObjectGetProperties (P.fd, obj, DRM_MODE_OBJECT_PLANE);
  uint32_t id = 0;
  for (uint32_t i = 0; pp && i < pp->count_props; i++) {
    drmModePropertyRes *r = drmModeGetProperty (P.fd, pp->props[i]);
    if (r && !strcmp (r->name, name)) {
      id = r->prop_id;
      for (int e = 0; e < r->count_enums; e++) {
        if (e1 && strstr (r->enums[e].name, e1)) *v1 = r->enums[e].value;
        if (e2 && strstr (r->enums[e].name, e2)) *v2 = r->enums[e].value;
        if (e3 && strstr (r->enums[e].name, e3)) *v3 = r->enums[e].value;
      }
    }
    drmModeFreeProperty (r);
  }
  drmModeFreeObjectProperties (pp);
  return id;
}

/* The fourcc + modifier pairs a plane scans out (IN_FORMATS, else linear). */
static void plane_formats (plane_st * s) {
  drmModePlane *p = drmModeGetPlane (P.fd, s->id);
  drmModeObjectProperties *pp = drmModeObjectGetProperties (P.fd, s->id, DRM_MODE_OBJECT_PLANE);
  uint64_t blob = 0;
  for (uint32_t i = 0; pp && i < pp->count_props; i++) {
    drmModePropertyRes *r = drmModeGetProperty (P.fd, pp->props[i]);
    if (r && !strcmp (r->name, "IN_FORMATS")) blob = pp->prop_values[i];
    drmModeFreeProperty (r);
  }
  drmModeFreeObjectProperties (pp);
  GArray *f = g_array_new (FALSE, FALSE, sizeof (uint32_t)), *m = g_array_new (FALSE, FALSE, sizeof (uint64_t));
  drmModePropertyBlobRes *b = blob ? drmModeGetPropertyBlob (P.fd, blob) : NULL;
  if (b) {
    drmModeFormatModifierIterator it = { 0 };
    while (drmModeFormatModifierBlobIterNext (b, &it)) {
      g_array_append_val (f, it.fmt);
      g_array_append_val (m, it.mod);
    }
    drmModeFreePropertyBlob (b);
  } else if (p) {
    uint64_t lin = DRM_FORMAT_MOD_LINEAR;
    for (uint32_t i = 0; i < p->count_formats; i++) {
      g_array_append_val (f, p->formats[i]);
      g_array_append_val (m, lin);
    }
  }
  drmModeFreePlane (p);
  s->nfmt = f->len;
  s->fmt = (uint32_t *) g_array_free (f, FALSE);
  s->mod = (uint64_t *) g_array_free (m, FALSE);
}

static int plane_takes (plane_st * s, uint32_t fourcc, uint64_t mod) {
  for (int i = 0; i < s->nfmt; i++)
    if (s->fmt[i] == fourcc && s->mod[i] == mod)
      return 1;
  return 0;
}

/* ---- frames ------------------------------------------------------------- */

static void frame_free (plane_st * s, frame * f) {
  if (!f) return;
  if (f->buf) gst_buffer_unref (f->buf);
  if (f->ring >= 0 && s) s->ring[f->ring].busy = 0;
  g_free (f);
}

typedef struct { uint32_t fb; int w, h; uint32_t fourcc; uint64_t mod; } fb_cache;

static void fb_cache_free (gpointer d) {
  fb_cache *c = d;
  /* The memory is gone, so no frame of it is held (and on screen). */
  if (c->fb && P.fd >= 0) drmModeRmFB (P.fd, c->fb);
  g_free (c);
}

/* A framebuffer for a dmabuf frame, cached on its first memory (decoder pools
 * recycle buffers: one framebuffer per pool slot). */
static uint32_t dmabuf_fb (GstBuffer * b, const GstVideoInfo * vi, uint32_t fourcc, uint64_t mod) {
  GstMemory *m0 = gst_buffer_peek_memory (b, 0);
  int w = GST_VIDEO_INFO_WIDTH (vi), h = GST_VIDEO_INFO_HEIGHT (vi);
  fb_cache *c = gst_mini_object_get_qdata (GST_MINI_OBJECT (m0), fb_quark);
  if (c && c->w == w && c->h == h && c->fourcc == fourcc && c->mod == mod)
    return c->fb;
  GstVideoMeta *vm = gst_buffer_get_video_meta (b);
  int np = vm ? (int) vm->n_planes : (int) GST_VIDEO_INFO_N_PLANES (vi);
  uint32_t handles[4] = { 0 }, pitches[4] = { 0 }, offsets[4] = { 0 };
  uint64_t mods[4] = { 0 };
  for (int p = 0; p < np && p < 4; p++) {
    gsize off = vm ? vm->offset[p] : GST_VIDEO_INFO_PLANE_OFFSET (vi, p);
    guint idx, len;
    gsize skip;
    if (!gst_buffer_find_memory (b, off, 1, &idx, &len, &skip)) return 0;
    GstMemory *m = gst_buffer_peek_memory (b, idx);
    int dfd = gst_is_dmabuf_memory (m) ? gst_dmabuf_memory_get_fd (m) : gst_is_fd_memory (m) ? gst_fd_memory_get_fd (m) : -1;
    if (dfd < 0 || drmPrimeFDToHandle (P.fd, dfd, &handles[p])) return 0;
    offsets[p] = skip + m->offset;
    pitches[p] = vm ? vm->stride[p] : GST_VIDEO_INFO_PLANE_STRIDE (vi, p);
    mods[p] = mod;
  }
  uint32_t fb = 0;
  int r = drmModeAddFB2WithModifiers (P.fd, w, h, fourcc, handles, pitches, offsets, mods, &fb,
      mod != DRM_FORMAT_MOD_LINEAR ? DRM_MODE_FB_MODIFIERS : 0);
  /* The framebuffer holds the buffer objects; our GEM handles can go. */
  for (int p = 0; p < np && p < 4; p++) {
    int dup = 0;
    for (int q = 0; q < p; q++) dup |= handles[q] == handles[p];
    if (handles[p] && !dup) drmCloseBufferHandle (P.fd, handles[p]);
  }
  if (r) {
    GST_WARNING ("addfb2 %" GST_FOURCC_FORMAT " mod %#" G_GINT64_MODIFIER "x %dx%d: %s", GST_FOURCC_ARGS (fourcc), mod, w, h, g_strerror (errno));
    return 0;
  }
  c = g_new0 (fb_cache, 1);
  *c = (fb_cache) { fb, w, h, fourcc, mod };
  gst_mini_object_set_qdata (GST_MINI_OBJECT (m0), fb_quark, c, fb_cache_free);
  return fb;
}

/* Scanout buffers for copied (system-memory) frames, laid out like vi. */
static void ring_free (plane_st * s) {
  for (int i = 0; i < s->ring_n; i++) {
    ring_buf *r = &s->ring[i];
    if (r->map) munmap (r->map, r->size);
    if (r->fb) drmModeRmFB (P.fd, r->fb);
    if (r->handle) {
      struct drm_mode_destroy_dumb dd = { .handle = r->handle };
      drmIoctl (P.fd, DRM_IOCTL_MODE_DESTROY_DUMB, &dd);
    }
  }
  memset (s->ring, 0, sizeof s->ring);
  s->ring_n = 0;
}

static int ring_alloc (plane_st * s, const GstVideoInfo * vi) {
  uint32_t fourcc = gst_video_dma_drm_fourcc_from_format (GST_VIDEO_INFO_FORMAT (vi));
  if (!fourcc) return -1;
  int w = GST_VIDEO_INFO_WIDTH (vi), h = GST_VIDEO_INFO_HEIGHT (vi);
  /* One dumb buffer per frame, planes packed with the layout GstVideoInfo
   * gives for width w (strides rounded by GStreamer's rules). */
  GstVideoInfo li;
  gst_video_info_set_format (&li, GST_VIDEO_INFO_FORMAT (vi), w, h);
  for (int i = 0; i < RING; i++) {
    ring_buf *r = &s->ring[i];
    /* A flat allocation of at least the frame's size (4096 x n bytes); the
     * framebuffer indexes it with GstVideoInfo's plane offsets and strides. */
    struct drm_mode_create_dumb cd = { .width = 4096, .height = (GST_VIDEO_INFO_SIZE (&li) + 4095) / 4096, .bpp = 8 };
    if (drmIoctl (P.fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) goto fail;
    r->handle = cd.handle;
    r->size = cd.size;
    struct drm_mode_map_dumb md = { .handle = cd.handle };
    if (drmIoctl (P.fd, DRM_IOCTL_MODE_MAP_DUMB, &md)) goto fail;
    r->map = mmap (NULL, cd.size, PROT_READ | PROT_WRITE, MAP_SHARED, P.fd, md.offset);
    if (r->map == MAP_FAILED) { r->map = NULL; goto fail; }
    uint32_t handles[4] = { 0 }, pitches[4] = { 0 }, offsets[4] = { 0 };
    for (guint p = 0; p < GST_VIDEO_INFO_N_PLANES (&li); p++) {
      handles[p] = cd.handle;
      pitches[p] = GST_VIDEO_INFO_PLANE_STRIDE (&li, p);
      offsets[p] = GST_VIDEO_INFO_PLANE_OFFSET (&li, p);
    }
    if (drmModeAddFB2 (P.fd, w, h, fourcc, handles, pitches, offsets, &r->fb, 0)) {
      GST_WARNING ("addfb2 (copy ring) %" GST_FOURCC_FORMAT " %dx%d: %s", GST_FOURCC_ARGS (fourcc), w, h, g_strerror (errno));
      goto fail;
    }
    s->ring_n++;
  }
  s->ring_info = li;
  return 0;
fail:
  s->ring_n++;
  ring_free (s);
  return -1;
}

/* ---- scanout pool ---------------------------------------------------------
 * Offered to upstream in the allocation query for system-memory caps: each
 * buffer is a dumb (scanout) buffer exported as a dmabuf, laid out by
 * GstVideoInfo (GstVideoMeta attached). A decoder or converter that takes it
 * writes the picture straight into scanout memory, and show_frame puts it on
 * the plane as a framebuffer without a copy (as kmssink's pool did: AV1
 * through dav1d 34.8 fps there against 20.5 with a copy per frame). */
typedef struct { GstBufferPool parent; GstVideoInfo info; GstAllocator *dmabuf; } PwPool;
typedef struct { GstBufferPoolClass parent_class; } PwPoolClass;
G_DEFINE_TYPE (PwPool, pw_pool, GST_TYPE_BUFFER_POOL);
static GQuark dumb_quark;

static void dumb_destroy (gpointer d) {
  struct drm_mode_destroy_dumb dd = { .handle = GPOINTER_TO_UINT (d) };
  if (P.fd >= 0) drmIoctl (P.fd, DRM_IOCTL_MODE_DESTROY_DUMB, &dd);
}

static gboolean pw_pool_set_config (GstBufferPool * bp, GstStructure * cfg) {
  PwPool *pool = (PwPool *) bp;
  GstCaps *caps;
  guint size, min, max;
  if (!gst_buffer_pool_config_get_params (cfg, &caps, &size, &min, &max) || !caps) return FALSE;
  if (!gst_video_info_from_caps (&pool->info, caps)) return FALSE;
  gst_buffer_pool_config_set_params (cfg, caps, GST_VIDEO_INFO_SIZE (&pool->info), min, max);
  return GST_BUFFER_POOL_CLASS (pw_pool_parent_class)->set_config (bp, cfg);
}

static GstFlowReturn pw_pool_alloc (GstBufferPool * bp, GstBuffer ** out, GstBufferPoolAcquireParams * ap) {
  PwPool *pool = (PwPool *) bp;
  GstVideoInfo *vi = &pool->info;
  struct drm_mode_create_dumb cd = { .width = 4096, .height = (GST_VIDEO_INFO_SIZE (vi) + 4095) / 4096, .bpp = 8 };
  if (drmIoctl (P.fd, DRM_IOCTL_MODE_CREATE_DUMB, &cd)) return GST_FLOW_ERROR;
  int fd = -1;
  if (drmPrimeHandleToFD (P.fd, cd.handle, DRM_CLOEXEC | DRM_RDWR, &fd)) { dumb_destroy (GUINT_TO_POINTER (cd.handle)); return GST_FLOW_ERROR; }
  GstMemory *m = gst_dmabuf_allocator_alloc (pool->dmabuf, fd, cd.size);
  /* The dumb buffer goes with the memory (after its framebuffer: qdata set
   * later is destroyed first). */
  gst_mini_object_set_qdata (GST_MINI_OBJECT (m), dumb_quark, GUINT_TO_POINTER (cd.handle), dumb_destroy);
  GstBuffer *b = gst_buffer_new ();
  gst_buffer_append_memory (b, m);
  gst_buffer_add_video_meta_full (b, GST_VIDEO_FRAME_FLAG_NONE, GST_VIDEO_INFO_FORMAT (vi), GST_VIDEO_INFO_WIDTH (vi),
      GST_VIDEO_INFO_HEIGHT (vi), GST_VIDEO_INFO_N_PLANES (vi), vi->offset, vi->stride);
  *out = b;
  return GST_FLOW_OK;
}

static void pw_pool_finalize (GObject * o) {
  PwPool *pool = (PwPool *) o;
  gst_clear_object (&pool->dmabuf);
  G_OBJECT_CLASS (pw_pool_parent_class)->finalize (o);
}
static void pw_pool_class_init (PwPoolClass * k) {
  G_OBJECT_CLASS (k)->finalize = pw_pool_finalize;
  GST_BUFFER_POOL_CLASS (k)->set_config = pw_pool_set_config;
  GST_BUFFER_POOL_CLASS (k)->alloc_buffer = pw_pool_alloc;
}
static void pw_pool_init (PwPool * p) { p->dmabuf = gst_dmabuf_allocator_new (); }

/* ---- presenter ----------------------------------------------------------- */

static void add (drmModeAtomicReq * rq, uint32_t obj, uint32_t prop, uint64_t v) {
  if (prop) drmModeAtomicAddProperty (rq, obj, prop, v);
}

static int cmpd (const void *a, const void *b) {
  double x = *(const double *) a, y = *(const double *) b;
  return x < y ? -1 : x > y;
}

static gpointer present (gpointer d) {
  struct sched_param sp = { .sched_priority = 10 };
  if (pthread_setschedparam (pthread_self (), SCHED_FIFO, &sp) != 0)
    g_printerr ("planewall: SCHED_FIFO: %s\n", g_strerror (errno));
  frame *next[MAXP];
  int off[MAXP], inc[MAXP];
  for (;;) {
    g_mutex_lock (&P.lock);
    while (!P.dirty && !P.quit) g_cond_wait (&P.cond, &P.lock);
    if (P.quit) { g_mutex_unlock (&P.lock); break; }
    P.dirty = 0;
    drmModeAtomicReq *rq = drmModeAtomicAlloc ();
    for (int i = 0; i < P.npl; i++) {
      plane_st *s = &P.pl[i];
      next[i] = NULL;
      off[i] = 0;
      inc[i] = 0;
      if (s->pending) { next[i] = s->pending; s->pending = NULL; }
      frame *f = next[i] ? next[i] : s->shown;
      if (!s->attached || !f) {
        if (s->on) { add (rq, s->id, s->p_fb, 0); add (rq, s->id, s->p_crtc, 0); off[i] = 1; }
        s->props_dirty = 0;
        continue;
      }
      if (!next[i] && !s->props_dirty && s->on) continue;
      inc[i] = 1;
      add (rq, s->id, s->p_fb, f->fb);
      add (rq, s->id, s->p_crtc, P.crtc);
      add (rq, s->id, s->p_sx, (uint64_t) f->sx << 16);
      add (rq, s->id, s->p_sy, (uint64_t) f->sy << 16);
      add (rq, s->id, s->p_sw, (uint64_t) f->sw << 16);
      add (rq, s->id, s->p_sh, (uint64_t) f->sh << 16);
      add (rq, s->id, s->p_cx, (uint64_t) (int64_t) f->cx);
      add (rq, s->id, s->p_cy, (uint64_t) (int64_t) f->cy);
      add (rq, s->id, s->p_cw, f->cw);
      add (rq, s->id, s->p_ch, f->ch);
      add (rq, s->id, s->p_alpha, s->alpha);
      add (rq, s->id, s->p_zpos, s->zpos);
      add (rq, s->id, s->p_rot, s->rot);
      add (rq, s->id, s->p_blend, s->blend);
      if (f->yuv) { add (rq, s->id, s->p_enc, f->enc); add (rq, s->id, s->p_range, f->range); }
      s->props_dirty = 0;
    }
    g_mutex_unlock (&P.lock);

    int r = drmModeAtomicGetCursor (rq) ? drmModeAtomicCommit (P.fd, rq, 0, NULL) : 0;
    int err = errno;
    drmModeAtomicFree (rq);
    gint64 t = g_get_monotonic_time ();

    g_mutex_lock (&P.lock);
    if (r) {
      P.fails++;
      if (err != P.last_errno || P.fails < 4)
        g_printerr ("planewall: atomic commit failed: %s\n", g_strerror (err));
      P.last_errno = err;
      /* Don't retry the frames that failed: drop them, keep what was shown. */
      for (int i = 0; i < P.npl; i++) {
        if (next[i]) { frame_free (&P.pl[i], next[i]); P.pl[i].dropped++; }
      }
    } else {
      if (P.last_us && P.niv < IVN) P.iv[P.niv++] = (t - P.last_us) / 1000.0;
      else if (P.last_us) { memmove (P.iv, P.iv + 1, (IVN - 1) * sizeof P.iv[0]); P.iv[IVN - 1] = (t - P.last_us) / 1000.0; }
      P.last_us = t;
      P.commits++;
      for (int i = 0; i < P.npl; i++) {
        plane_st *s = &P.pl[i];
        if (off[i]) { s->on = 0; frame_free (s, s->shown); s->shown = NULL; continue; }
        if (inc[i] && s->on && s->alpha != s->alpha_shown) s->steps++;
        if (inc[i]) s->alpha_shown = s->alpha;
        if (next[i]) {
          /* The frame it replaced has left the screen. */
          frame_free (s, s->shown);
          s->shown = next[i];
          s->shown_frames++;
          s->on = 1;
          s->first_seen = 1;
        } else if (s->attached && s->shown) s->on = 1;
      }
    }
    P.gen++;
    g_cond_broadcast (&P.done);
    g_mutex_unlock (&P.lock);
  }
  return NULL;
}

static void kick_locked (void) {
  P.dirty = 1;
  g_cond_signal (&P.cond);
}

int planewall_open (int drm_fd, uint32_t crtc_id, const uint32_t * planes, int nplanes, int w, int h, int hz, char **err) {
  if (P.open) return 0;
  GST_DEBUG_CATEGORY_INIT (planewall_debug, "cutepiplanewall", 0, "CuTePi plane wall");
  fb_quark = g_quark_from_static_string ("cutepi-planewall-fb");
  dumb_quark = g_quark_from_static_string ("cutepi-planewall-dumb");
  g_mutex_init (&P.lock);
  g_cond_init (&P.cond);
  g_cond_init (&P.done);
  P.fd = drm_fd;
  P.crtc = crtc_id;
  P.w = w; P.h = h; P.hz = hz;
  P.npl = 0;
  for (int i = 0; i < nplanes && i < MAXP; i++) {
    plane_st *s = &P.pl[P.npl++];
    memset (s, 0, sizeof *s);
    s->id = planes[i];
    s->p_fb = prop_id (s->id, "FB_ID", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_crtc = prop_id (s->id, "CRTC_ID", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_sx = prop_id (s->id, "SRC_X", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_sy = prop_id (s->id, "SRC_Y", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_sw = prop_id (s->id, "SRC_W", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_sh = prop_id (s->id, "SRC_H", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_cx = prop_id (s->id, "CRTC_X", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_cy = prop_id (s->id, "CRTC_Y", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_cw = prop_id (s->id, "CRTC_W", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_ch = prop_id (s->id, "CRTC_H", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_alpha = prop_id (s->id, "alpha", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_zpos = prop_id (s->id, "zpos", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_rot = prop_id (s->id, "rotation", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_blend = prop_id (s->id, "pixel blend mode", NULL, NULL, NULL, NULL, NULL, NULL);
    s->p_enc = prop_id (s->id, "COLOR_ENCODING", "601", &s->enc601, "709", &s->enc709, "2020", &s->enc2020);
    s->p_range = prop_id (s->id, "COLOR_RANGE", "limited", &s->rlimited, "full", &s->rfull, NULL, NULL);
    s->alpha = 0;
    s->zpos = 1;
    s->rot = 1;                 /* DRM_MODE_ROTATE_0 */
    plane_formats (s);
    if (!s->p_fb || !s->p_crtc) { *err = g_strdup_printf ("plane %u has no FB_ID/CRTC_ID", s->id); return -1; }
  }
  P.quit = 0;
  P.th = g_thread_new ("planewall", present, NULL);
  P.open = 1;
  return 0;
}

int planewall_is_open (void) { return P.open; }

/* A plane property for the next commit (alpha, zpos, rotation, blend). */
int planewall_set (uint32_t plane, const char *name, uint64_t value) {
  g_mutex_lock (&P.lock);
  plane_st *s = plane_of (plane);
  int ok = s != NULL;
  if (s) {
    if (!strcmp (name, "alpha")) s->alpha = value;
    else if (!strcmp (name, "zpos")) s->zpos = value;
    else if (!strcmp (name, "rotation")) s->rot = value;
    else if (!strcmp (name, "pixel blend mode")) s->blend = value;
    else ok = 0;
    if (ok) { s->props_dirty = 1; kick_locked (); }
  }
  g_mutex_unlock (&P.lock);
  return ok ? 0 : -1;
}

void planewall_stats (planewall_stats_t * st) {
  g_mutex_lock (&P.lock);
  st->commits = P.commits;
  st->fails = P.fails;
  double tmp[IVN];
  int n = P.niv;
  memcpy (tmp, P.iv, n * sizeof tmp[0]);
  g_mutex_unlock (&P.lock);
  qsort (tmp, n, sizeof tmp[0], cmpd);
  st->p50_ms = n ? tmp[n / 2] : 0;
  st->p99_ms = n ? tmp[n * 99 / 100] : 0;
  st->max_ms = n ? tmp[n - 1] : 0;
}

void planewall_plane_stats (uint32_t plane, uint64_t * shown, uint64_t * dropped, uint64_t * steps, int *first, int64_t * t_us) {
  g_mutex_lock (&P.lock);
  plane_st *s = plane_of (plane);
  *shown = s ? s->shown_frames : 0;
  *dropped = s ? s->dropped : 0;
  *steps = s ? s->steps : 0;
  *first = s ? s->first_seen : 0;
  *t_us = P.last_us;            /* the commit these counts are as of */
  g_mutex_unlock (&P.lock);
}

/* ---- cutepiplanesink ---------------------------------------------------- */

#define CUTEPI_TYPE_PLANE_SINK (cutepi_plane_sink_get_type ())
G_DECLARE_FINAL_TYPE (CutepiPlaneSink, cutepi_plane_sink, CUTEPI, PLANE_SINK, GstVideoSink)

struct _CutepiPlaneSink {
  GstVideoSink parent;
  guint plane_id;
  int bx, by, bw, bh;           /* render rectangle (0 size: the whole display) */
  GstVideoInfo vi;
  GstVideoInfoDmaDrm drm;
  int dmabuf, yuv;
  uint64_t enc, range;
};

static void cutepi_plane_sink_overlay_init (GstVideoOverlayInterface * iface);
G_DEFINE_TYPE_WITH_CODE (CutepiPlaneSink, cutepi_plane_sink, GST_TYPE_VIDEO_SINK,
    G_IMPLEMENT_INTERFACE (GST_TYPE_VIDEO_OVERLAY, cutepi_plane_sink_overlay_init));

enum { PROP_0, PROP_PLANE_ID, PROP_LAST };

/* System-memory layouts that can be copied into a scanout buffer (as
 * kmsSysmemCaps in wall.go: the kernel cannot allocate 4:2:2/4:4:4 dumb
 * layouts here). */
#define SYSMEM_FORMATS "{ I420, YV12, NV12, NV21, BGRx, BGRA, RGBx, RGBA, xRGB, ARGB, xBGR, ABGR, RGB16 }"

static GstStaticPadTemplate sink_tmpl = GST_STATIC_PAD_TEMPLATE ("sink", GST_PAD_SINK, GST_PAD_ALWAYS,
    GST_STATIC_CAPS (GST_VIDEO_DMA_DRM_CAPS_MAKE "; " GST_VIDEO_CAPS_MAKE (SYSMEM_FORMATS)));

static void cutepi_plane_sink_set_render_rectangle (GstVideoOverlay * o, gint x, gint y, gint w, gint h) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (o);
  GST_OBJECT_LOCK (self);
  self->bx = x; self->by = y; self->bw = w; self->bh = h;
  GST_OBJECT_UNLOCK (self);
}
static void cutepi_plane_sink_expose (GstVideoOverlay * o) {}
static void cutepi_plane_sink_overlay_init (GstVideoOverlayInterface * iface) {
  iface->set_render_rectangle = cutepi_plane_sink_set_render_rectangle;
  iface->expose = cutepi_plane_sink_expose;
}

static void cutepi_plane_sink_set_property (GObject * o, guint id, const GValue * v, GParamSpec * ps) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (o);
  if (gst_video_overlay_set_property (o, PROP_LAST, id, v)) return;
  if (id == PROP_PLANE_ID) self->plane_id = g_value_get_int (v);
  else G_OBJECT_WARN_INVALID_PROPERTY_ID (o, id, ps);
}
static void cutepi_plane_sink_get_property (GObject * o, guint id, GValue * v, GParamSpec * ps) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (o);
  if (id == PROP_PLANE_ID) g_value_set_int (v, self->plane_id);
  else G_OBJECT_WARN_INVALID_PROPERTY_ID (o, id, ps);
}

/* What this plane takes: its dmabuf fourcc+modifier pairs, and the copyable
 * system-memory layouts. */
static GstCaps *cutepi_plane_sink_get_caps (GstBaseSink * bs, GstCaps * filter) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (bs);
  plane_st *s = P.open ? plane_of (self->plane_id) : NULL;
  GstCaps *caps;
  if (!s) caps = gst_pad_get_pad_template_caps (GST_BASE_SINK_PAD (bs));
  else {
    GValue list = G_VALUE_INIT;
    g_value_init (&list, GST_TYPE_LIST);
    for (int i = 0; i < s->nfmt; i++) {
      gchar *str = gst_video_dma_drm_fourcc_to_string (s->fmt[i], s->mod[i]);
      if (!str) continue;
      GValue v = G_VALUE_INIT;
      g_value_init (&v, G_TYPE_STRING);
      g_value_take_string (&v, str);
      gst_value_list_append_and_take_value (&list, &v);
    }
    caps = gst_caps_new_empty ();
    GstStructure *st = gst_structure_new ("video/x-raw", "format", G_TYPE_STRING, "DMA_DRM",
        "width", GST_TYPE_INT_RANGE, 1, G_MAXINT, "height", GST_TYPE_INT_RANGE, 1, G_MAXINT,
        "framerate", GST_TYPE_FRACTION_RANGE, 0, 1, G_MAXINT, 1, NULL);
    gst_structure_take_value (st, "drm-format", &list);
    gst_caps_append_structure_full (caps, st, gst_caps_features_new_single_static_str (GST_CAPS_FEATURE_MEMORY_DMABUF));
    gst_caps_append (caps, gst_caps_from_string (GST_VIDEO_CAPS_MAKE (SYSMEM_FORMATS)));
  }
  if (filter) {
    GstCaps *c = gst_caps_intersect_full (filter, caps, GST_CAPS_INTERSECT_FIRST);
    gst_caps_unref (caps);
    caps = c;
  }
  return caps;
}

static gboolean cutepi_plane_sink_set_caps (GstBaseSink * bs, GstCaps * caps) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (bs);
  plane_st *s = plane_of (self->plane_id);
  if (!s) { GST_ELEMENT_ERROR (self, RESOURCE, SETTINGS, ("no plane %u on the wall", self->plane_id), (NULL)); return FALSE; }
  self->dmabuf = gst_video_is_dma_drm_caps (caps);
  if (self->dmabuf) {
    if (!gst_video_info_dma_drm_from_caps (&self->drm, caps)) return FALSE;
    if (!gst_video_info_dma_drm_to_video_info (&self->drm, &self->vi)) {
      /* A fourcc GStreamer has no format for: still describe the size. */
      gst_video_info_init (&self->vi);
      self->vi.width = self->drm.vinfo.width;
      self->vi.height = self->drm.vinfo.height;
      self->vi.par_n = self->drm.vinfo.par_n ? self->drm.vinfo.par_n : 1;
      self->vi.par_d = self->drm.vinfo.par_d ? self->drm.vinfo.par_d : 1;
      self->vi.colorimetry = self->drm.vinfo.colorimetry;
      self->vi.finfo = self->drm.vinfo.finfo;
    }
    if (!plane_takes (s, self->drm.drm_fourcc, self->drm.drm_modifier)) {
      GST_ELEMENT_ERROR (self, STREAM, FORMAT, ("plane %u cannot scan out %" GST_FOURCC_FORMAT " %#" G_GINT64_MODIFIER "x",
              self->plane_id, GST_FOURCC_ARGS (self->drm.drm_fourcc), self->drm.drm_modifier), (NULL));
      return FALSE;
    }
  } else {
    if (!gst_video_info_from_caps (&self->vi, caps)) return FALSE;
    g_mutex_lock (&P.lock);
    int need = !s->ring_n || !gst_video_info_is_equal (&s->ring_info, &self->vi);
    g_mutex_unlock (&P.lock);
    if (need) {
      /* Only free copy buffers that are off the screen: detach first. */
      if (s->ring_n) planewall_detach (self->plane_id);
      ring_free (s);
      if (ring_alloc (s, &self->vi)) {
        GST_ELEMENT_ERROR (self, RESOURCE, NO_SPACE_LEFT, ("no scanout buffers for %s %dx%d",
                gst_video_format_to_string (GST_VIDEO_INFO_FORMAT (&self->vi)), self->vi.width, self->vi.height), (NULL));
        return FALSE;
      }
    }
  }
  const GstVideoFormatInfo *fi = self->vi.finfo;
  self->yuv = fi ? GST_VIDEO_FORMAT_INFO_IS_YUV (fi) : 1;
  GstVideoColorMatrix mx = self->vi.colorimetry.matrix;
  if (mx == GST_VIDEO_COLOR_MATRIX_UNKNOWN)
    mx = self->vi.height >= 720 ? GST_VIDEO_COLOR_MATRIX_BT709 : GST_VIDEO_COLOR_MATRIX_BT601;
  self->enc = mx == GST_VIDEO_COLOR_MATRIX_BT2020 ? s->enc2020 : mx == GST_VIDEO_COLOR_MATRIX_BT709 ? s->enc709 : s->enc601;
  self->range = self->vi.colorimetry.range == GST_VIDEO_COLOR_RANGE_0_255 ? s->rfull : s->rlimited;
  return TRUE;
}

static gboolean cutepi_plane_sink_propose_allocation (GstBaseSink * bs, GstQuery * q) {
  gst_query_add_allocation_meta (q, GST_VIDEO_META_API_TYPE, NULL);
  gst_query_add_allocation_meta (q, GST_VIDEO_CROP_META_API_TYPE, NULL);
  GstCaps *caps;
  gboolean need_pool;
  gst_query_parse_allocation (q, &caps, &need_pool);
  GstVideoInfo vi;
  if (!caps || gst_video_is_dma_drm_caps (caps) || !gst_video_info_from_caps (&vi, caps)) return TRUE;
  if (!gst_video_dma_drm_fourcc_from_format (GST_VIDEO_INFO_FORMAT (&vi))) return TRUE;
  if (need_pool) {
    GstBufferPool *pool = g_object_new (pw_pool_get_type (), NULL);
    GstStructure *cfg = gst_buffer_pool_get_config (pool);
    gst_buffer_pool_config_set_params (cfg, caps, GST_VIDEO_INFO_SIZE (&vi), 8, 0);
    gst_buffer_pool_config_add_option (cfg, GST_BUFFER_POOL_OPTION_VIDEO_META);
    if (gst_buffer_pool_set_config (pool, cfg))
      gst_query_add_allocation_pool (q, pool, GST_VIDEO_INFO_SIZE (&vi), 8, 0);
    gst_object_unref (pool);
  }
  return TRUE;
}

/* The frame's on-screen rectangle: the (cropped) picture fitted inside the
 * render rectangle, aspect kept from its pixel aspect ratio (as kmssink). */
static void place (CutepiPlaneSink * self, frame * f) {
  int bx, by, bw, bh;
  GST_OBJECT_LOCK (self);
  bx = self->bx; by = self->by; bw = self->bw; bh = self->bh;
  GST_OBJECT_UNLOCK (self);
  if (bw <= 0 || bh <= 0) { bx = 0; by = 0; bw = P.w; bh = P.h; }
  double par = self->vi.par_d ? (double) self->vi.par_n / self->vi.par_d : 1;
  double da = (double) f->sw * par / f->sh;
  int w = bw, h = bh;
  if (da > (double) bw / bh) h = (int) (bw / da + 0.5);
  else w = (int) (bh * da + 0.5);
  f->cx = bx + (bw - w) / 2;
  f->cy = by + (bh - h) / 2;
  f->cw = w > 0 ? w : 1;
  f->ch = h > 0 ? h : 1;
}

static GstFlowReturn cutepi_plane_sink_show_frame (GstVideoSink * vs, GstBuffer * b) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (vs);
  plane_st *s = plane_of (self->plane_id);
  if (!s) return GST_FLOW_ERROR;
  frame *f = g_new0 (frame, 1);
  f->ring = -1;
  uint32_t sys_fourcc = self->dmabuf ? 0 : gst_video_dma_drm_fourcc_from_format (GST_VIDEO_INFO_FORMAT (&self->vi));
  GstMemory *m0 = gst_buffer_n_memory (b) ? gst_buffer_peek_memory (b, 0) : NULL;
  if (!self->dmabuf && sys_fourcc && m0 && gst_is_dmabuf_memory (m0) && gst_buffer_get_video_meta (b) &&
      (f->fb = dmabuf_fb (b, &self->vi, sys_fourcc, DRM_FORMAT_MOD_LINEAR)) != 0) {
    f->buf = gst_buffer_ref (b);  /* written straight into scanout memory */
  } else if (self->dmabuf) {
    f->fb = dmabuf_fb (b, &self->vi, self->drm.drm_fourcc, self->drm.drm_modifier);
    if (!f->fb) { g_free (f); GST_ELEMENT_ERROR (self, RESOURCE, FAILED, ("cannot put the frame on plane %u", self->plane_id), (NULL)); return GST_FLOW_ERROR; }
    f->buf = gst_buffer_ref (b);
  } else {
    /* Copy into a free scanout buffer (none free: the presenter is behind,
     * this frame is dropped). */
    g_mutex_lock (&P.lock);
    int slot = -1;
    for (int i = 0; i < s->ring_n; i++) if (!s->ring[i].busy) { slot = i; s->ring[i].busy = 1; break; }
    g_mutex_unlock (&P.lock);
    if (slot < 0) { g_free (f); s->dropped++; return GST_FLOW_OK; }
    GstVideoFrame src;
    if (!gst_video_frame_map (&src, &self->vi, b, GST_MAP_READ)) { g_mutex_lock (&P.lock); s->ring[slot].busy = 0; g_mutex_unlock (&P.lock); g_free (f); return GST_FLOW_ERROR; }
    GstVideoFrame dst;
    GstVideoInfo *li = &s->ring_info;
    memset (&dst, 0, sizeof dst);
    dst.info = *li;
    for (guint p = 0; p < GST_VIDEO_INFO_N_PLANES (li); p++) dst.data[p] = s->ring[slot].map + GST_VIDEO_INFO_PLANE_OFFSET (li, p);
    dst.flags = 0;
    gst_video_frame_copy (&dst, &src);
    gst_video_frame_unmap (&src);
    f->ring = slot;
    f->fb = s->ring[slot].fb;
  }
  GstVideoCropMeta *cm = gst_buffer_get_video_crop_meta (b);
  if (cm && cm->width && cm->height) { f->sx = cm->x; f->sy = cm->y; f->sw = cm->width; f->sh = cm->height; }
  else { f->sx = 0; f->sy = 0; f->sw = self->vi.width; f->sh = self->vi.height; }
  f->yuv = self->yuv;
  f->enc = self->enc;
  f->range = self->range;
  place (self, f);
  g_mutex_lock (&P.lock);
  if (s->pending) { frame_free (s, s->pending); s->dropped++; }
  s->pending = f;
  s->attached = 1;
  kick_locked ();
  g_mutex_unlock (&P.lock);
  return GST_FLOW_OK;
}

/* Take the plane off the screen and release its frames: waits for the commit
 * that turns it off. */
void planewall_detach (uint32_t plane) {
  g_mutex_lock (&P.lock);
  plane_st *s = plane_of (plane);
  if (s) {
    s->attached = 0;
    if (s->pending) { frame_free (s, s->pending); s->pending = NULL; }
    if (s->on || s->shown) {
      kick_locked ();
      gint64 until = g_get_monotonic_time () + G_TIME_SPAN_SECOND;
      while ((s->on || s->shown) && P.open && !P.quit)
        if (!g_cond_wait_until (&P.done, &P.lock, until)) break;
    }
  }
  g_mutex_unlock (&P.lock);
}

static gboolean cutepi_plane_sink_stop (GstBaseSink * bs) {
  CutepiPlaneSink *self = CUTEPI_PLANE_SINK (bs);
  /* The decoder's pool wants its buffers back: none stay on the plane. */
  planewall_detach (self->plane_id);
  plane_st *s = plane_of (self->plane_id);
  if (s && s->ring_n) ring_free (s);
  return TRUE;
}

static void cutepi_plane_sink_class_init (CutepiPlaneSinkClass * k) {
  GObjectClass *oc = G_OBJECT_CLASS (k);
  GstElementClass *ec = GST_ELEMENT_CLASS (k);
  GstBaseSinkClass *bc = GST_BASE_SINK_CLASS (k);
  GstVideoSinkClass *vc = GST_VIDEO_SINK_CLASS (k);
  oc->set_property = cutepi_plane_sink_set_property;
  oc->get_property = cutepi_plane_sink_get_property;
  g_object_class_install_property (oc, PROP_PLANE_ID, g_param_spec_int ("plane-id", "Plane ID", "Overlay plane on the plane wall",
          0, G_MAXINT, 0, G_PARAM_READWRITE | G_PARAM_STATIC_STRINGS));
  gst_video_overlay_install_properties (oc, PROP_LAST);
  gst_element_class_add_static_pad_template (ec, &sink_tmpl);
  gst_element_class_set_static_metadata (ec, "CuTePi plane sink", "Sink/Video",
      "Frames on a plane of CuTePi's plane wall (one atomic commit per refresh)", "CuTePi");
  bc->get_caps = cutepi_plane_sink_get_caps;
  bc->set_caps = cutepi_plane_sink_set_caps;
  bc->propose_allocation = cutepi_plane_sink_propose_allocation;
  bc->stop = cutepi_plane_sink_stop;
  vc->show_frame = cutepi_plane_sink_show_frame;
}

static void cutepi_plane_sink_init (CutepiPlaneSink * self) {}

int cutepi_planesink_register (void) {
  return gst_element_register (NULL, "cutepiplanesink", GST_RANK_NONE, CUTEPI_TYPE_PLANE_SINK);
}
