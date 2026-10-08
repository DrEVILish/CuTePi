/* wpedmabuf: GL texture frames (wpevideosrc, GLMemory RGBA) to linear dmabufs a vc4 plane can scan out.
 *
 * V3D renders WebKit's pages into UIF-tiled textures, which the Pi 4 display controller cannot scan out
 * (vc4 RGB planes take LINEAR or VC4_T_TILED only), and gldownload refuses to export tiled images at all.
 * Reading each 8 MB frame back on the CPU (gldownload to system memory) caps a 1080p page at 33-50 fps.
 * This element does one GPU blit per frame into a small ring of contiguous (CMA dma-heap) linear buffers,
 * imported once as EGLImage render targets, and hands them downstream as DMA_DRM AB24 dmabufs: no CPU
 * touches the pixels. A ring slot is reused only when the buffer that carried it has been released
 * (kmssink drops the previous frame when the next one is on screen). */
#include <gst/gst.h>
#include <gst/base/gstbasetransform.h>
#include <gst/video/video.h>
#include <gst/allocators/gstdmabuf.h>
#include <gst/gl/gl.h>
#include <gst/gl/egl/gstgldisplay_egl.h>
#define GL_GLEXT_PROTOTYPES
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES3/gl3.h>
#include <GLES2/gl2ext.h>
#include <linux/dma-heap.h>
#include <drm_fourcc.h>
#include <fcntl.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include <string.h>

#define WPEDMABUF_SLOTS 4

GST_DEBUG_CATEGORY_STATIC (wpedmabuf_debug);
#define GST_CAT_DEFAULT wpedmabuf_debug

typedef struct _GstWpeDmabuf GstWpeDmabuf;
typedef struct _GstWpeDmabufClass GstWpeDmabufClass;

typedef struct {
  GstWpeDmabuf *self;
  int fd;
  GstMemory *mem;               /* dmabuf memory, kept for the element's life: kmssink caches its FB on it */
  EGLImageKHR image;
  GLuint tex, fbo;
  gboolean busy;
} Slot;

struct _GstWpeDmabuf {
  GstBaseTransform parent;
  GstVideoInfo info;
  GstAllocator *alloc;
  GstGLContext *ctx;            /* the context the slots were made in (upstream's) */
  Slot slots[WPEDMABUF_SLOTS];
  gboolean ready, failed, flushing;
  guint stride;
  gsize size;
  GMutex lock;
  GCond cond;
  /* blit job */
  GstGLMemory *in;
  Slot *out;
};
struct _GstWpeDmabufClass { GstBaseTransformClass parent; };

GType gst_wpe_dmabuf_get_type (void);
static GQuark slot_quark;
static PFNEGLCREATEIMAGEKHRPROC create_image;
static PFNEGLDESTROYIMAGEKHRPROC destroy_image;
G_DEFINE_TYPE (GstWpeDmabuf, gst_wpe_dmabuf, GST_TYPE_BASE_TRANSFORM);

static GstStaticPadTemplate sink_tmpl = GST_STATIC_PAD_TEMPLATE ("sink", GST_PAD_SINK, GST_PAD_ALWAYS,
    GST_STATIC_CAPS ("video/x-raw(memory:GLMemory),format=RGBA,texture-target=2D"));
static GstStaticPadTemplate src_tmpl = GST_STATIC_PAD_TEMPLATE ("src", GST_PAD_SRC, GST_PAD_ALWAYS,
    GST_STATIC_CAPS ("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=AB24"));

static GstCaps *
wd_transform_caps (GstBaseTransform * bt, GstPadDirection dir, GstCaps * caps, GstCaps * filter)
{
  GstCaps *out = gst_caps_new_empty ();
  for (guint i = 0; i < gst_caps_get_size (caps); i++) {
    GstStructure *s = gst_structure_copy (gst_caps_get_structure (caps, i));
    if (dir == GST_PAD_SINK) {
      gst_structure_set (s, "format", G_TYPE_STRING, "DMA_DRM", "drm-format", G_TYPE_STRING, "AB24", NULL);
      gst_structure_remove_field (s, "texture-target");
      gst_caps_append_structure_full (out, s, gst_caps_features_new_single_static_str (GST_CAPS_FEATURE_MEMORY_DMABUF));
    } else {
      gst_structure_set (s, "format", G_TYPE_STRING, "RGBA", "texture-target", G_TYPE_STRING, "2D", NULL);
      gst_structure_remove_field (s, "drm-format");
      gst_caps_append_structure_full (out, s, gst_caps_features_new_single_static_str (GST_CAPS_FEATURE_MEMORY_GL_MEMORY));
    }
  }
  if (filter) {
    GstCaps *t = gst_caps_intersect_full (filter, out, GST_CAPS_INTERSECT_FIRST);
    gst_caps_unref (out);
    out = t;
  }
  return out;
}

static gboolean
wd_set_caps (GstBaseTransform * bt, GstCaps * in, GstCaps * out)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) bt;
  if (!gst_video_info_from_caps (&self->info, in))
    return FALSE;
  return TRUE;
}

static void
slots_free_gl (GstGLContext * ctx, GstWpeDmabuf * self)
{
  EGLDisplay dpy = (EGLDisplay) gst_gl_display_get_handle (ctx->display);
  for (int i = 0; i < WPEDMABUF_SLOTS; i++) {
    Slot *s = &self->slots[i];
    if (s->fbo) glDeleteFramebuffers (1, &s->fbo);
    if (s->tex) glDeleteTextures (1, &s->tex);
    if (s->image) destroy_image (dpy, s->image);
    s->fbo = s->tex = 0;
    s->image = NULL;
  }
}

/* GL thread: allocate the ring (CMA dma-heap, linear AB24) and make each slot a render target. */
static void
slots_init_gl (GstGLContext * ctx, GstWpeDmabuf * self)
{
  EGLDisplay dpy = (EGLDisplay) gst_gl_display_get_handle (ctx->display);
  PFNGLEGLIMAGETARGETTEXTURE2DOESPROC target2d =
      (PFNGLEGLIMAGETARGETTEXTURE2DOESPROC) gst_gl_context_get_proc_address (ctx, "glEGLImageTargetTexture2DOES");
  int w = GST_VIDEO_INFO_WIDTH (&self->info), h = GST_VIDEO_INFO_HEIGHT (&self->info);
  int heap = open ("/dev/dma_heap/linux,cma", O_RDWR | O_CLOEXEC);
  self->failed = TRUE;
  if (heap < 0 || !target2d) {
    GST_ERROR_OBJECT (self, "no CMA dma-heap or glEGLImageTargetTexture2DOES");
    if (heap >= 0) close (heap);
    return;
  }
  self->stride = GST_ROUND_UP_64 (w * 4);
  self->size = (gsize) self->stride * h;
  for (int i = 0; i < WPEDMABUF_SLOTS; i++) {
    Slot *s = &self->slots[i];
    struct dma_heap_allocation_data a = {.len = self->size,.fd_flags = O_RDWR | O_CLOEXEC };
    if (ioctl (heap, DMA_HEAP_IOCTL_ALLOC, &a) < 0) {
      GST_ERROR_OBJECT (self, "dma-heap allocation of %zu bytes failed", self->size);
      goto out;
    }
    s->self = self;
    s->fd = a.fd;
    s->mem = gst_dmabuf_allocator_alloc (self->alloc, s->fd, self->size);  /* owns the fd */
    EGLint attrs[] = {
      EGL_WIDTH, w, EGL_HEIGHT, h, EGL_LINUX_DRM_FOURCC_EXT, DRM_FORMAT_ABGR8888,
      EGL_DMA_BUF_PLANE0_FD_EXT, s->fd, EGL_DMA_BUF_PLANE0_OFFSET_EXT, 0,
      EGL_DMA_BUF_PLANE0_PITCH_EXT, (EGLint) self->stride,
      EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, (EGLint) (DRM_FORMAT_MOD_LINEAR & 0xffffffff),
      EGL_DMA_BUF_PLANE0_MODIFIER_HI_EXT, (EGLint) (DRM_FORMAT_MOD_LINEAR >> 32),
      EGL_NONE
    };
    s->image = create_image (dpy, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, NULL, attrs);
    if (s->image == EGL_NO_IMAGE_KHR) {
      GST_ERROR_OBJECT (self, "EGL import of the linear buffer failed: 0x%x", eglGetError ());
      goto out;
    }
    glGenTextures (1, &s->tex);
    glBindTexture (GL_TEXTURE_2D, s->tex);
    target2d (GL_TEXTURE_2D, s->image);
    glGenFramebuffers (1, &s->fbo);
    glBindFramebuffer (GL_FRAMEBUFFER, s->fbo);
    glFramebufferTexture2D (GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, s->tex, 0);
    if (glCheckFramebufferStatus (GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
      GST_ERROR_OBJECT (self, "linear buffer is not a complete render target");
      goto out;
    }
  }
  glBindFramebuffer (GL_FRAMEBUFFER, 0);
  glBindTexture (GL_TEXTURE_2D, 0);
  self->failed = FALSE;
  GST_INFO_OBJECT (self, "%d linear %dx%d AB24 scanout buffers, stride %u", WPEDMABUF_SLOTS, w, h, self->stride);
out:
  close (heap);
}

/* GL thread: copy the frame's texture into the slot (GPU blit; detiles UIF into linear) and wait for it. */
static void
blit_gl (GstGLContext * ctx, GstWpeDmabuf * self)
{
  int w = GST_VIDEO_INFO_WIDTH (&self->info), h = GST_VIDEO_INFO_HEIGHT (&self->info);
  GLuint rfbo;
  glGenFramebuffers (1, &rfbo);
  glBindFramebuffer (GL_READ_FRAMEBUFFER, rfbo);
  glFramebufferTexture2D (GL_READ_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D,
      gst_gl_memory_get_texture_id (self->in), 0);
  glBindFramebuffer (GL_DRAW_FRAMEBUFFER, self->out->fbo);
  glBlitFramebuffer (0, 0, w, h, 0, 0, w, h, GL_COLOR_BUFFER_BIT, GL_NEAREST);
  glBindFramebuffer (GL_FRAMEBUFFER, 0);
  glDeleteFramebuffers (1, &rfbo);
  /* The display reads the buffer as soon as kmssink commits it: finish the copy first. */
  glFinish ();
}

static void
slot_released (gpointer data)
{
  Slot *s = data;
  GstWpeDmabuf *self = s->self;
  g_mutex_lock (&self->lock);
  s->busy = FALSE;
  g_cond_signal (&self->cond);
  g_mutex_unlock (&self->lock);
  gst_object_unref (self);
}


static GstFlowReturn
wd_prepare_output_buffer (GstBaseTransform * bt, GstBuffer * inbuf, GstBuffer ** outbuf)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) bt;
  GstMemory *m = gst_buffer_peek_memory (inbuf, 0);
  if (!gst_is_gl_memory (m)) {
    GST_ELEMENT_ERROR (self, STREAM, FORMAT, ("input is not GL memory"), (NULL));
    return GST_FLOW_ERROR;
  }
  GstGLMemory *glm = (GstGLMemory *) m;
  GstGLContext *ctx = glm->mem.context;
  if (!self->ready) {
    self->ctx = gst_object_ref (ctx);
    gst_gl_context_thread_add (ctx, (GstGLContextThreadFunc) slots_init_gl, self);
    self->ready = TRUE;
  }
  if (self->failed) {
    GST_ELEMENT_ERROR (self, RESOURCE, FAILED, ("no linear scanout buffers"), (NULL));
    return GST_FLOW_ERROR;
  }
  GstGLSyncMeta *sync = gst_buffer_get_gl_sync_meta (inbuf);
  if (sync)
    gst_gl_sync_meta_wait (sync, ctx);

  Slot *slot = NULL;
  gint64 end = g_get_monotonic_time () + G_TIME_SPAN_SECOND;
  g_mutex_lock (&self->lock);
  while (!self->flushing) {
    for (int i = 0; i < WPEDMABUF_SLOTS && !slot; i++)
      if (!self->slots[i].busy)
        slot = &self->slots[i];
    if (slot || !g_cond_wait_until (&self->cond, &self->lock, end))
      break;
  }
  if (slot)
    slot->busy = TRUE;
  g_mutex_unlock (&self->lock);
  if (!slot)
    return self->flushing ? GST_FLOW_FLUSHING : GST_FLOW_ERROR;

  self->in = glm;
  self->out = slot;
  gst_gl_context_thread_add (ctx, (GstGLContextThreadFunc) blit_gl, self);

  GstBuffer *out = gst_buffer_new ();
  gst_buffer_append_memory (out, gst_memory_ref (slot->mem));
  gsize offset[GST_VIDEO_MAX_PLANES] = { 0 };
  gint stride[GST_VIDEO_MAX_PLANES] = { (gint) self->stride };
  gst_buffer_add_video_meta_full (out, GST_VIDEO_FRAME_FLAG_NONE, GST_VIDEO_FORMAT_RGBA,
      GST_VIDEO_INFO_WIDTH (&self->info), GST_VIDEO_INFO_HEIGHT (&self->info), 1, offset, stride);
  gst_buffer_copy_into (out, inbuf, GST_BUFFER_COPY_METADATA & ~GST_BUFFER_COPY_META, 0, -1);
  gst_mini_object_set_qdata (GST_MINI_OBJECT (out), slot_quark, slot, slot_released);
  gst_object_ref (self);
  *outbuf = out;
  return GST_FLOW_OK;
}

static GstFlowReturn
wd_transform (GstBaseTransform * bt, GstBuffer * in, GstBuffer * out)
{
  return GST_FLOW_OK;
}

static gboolean
wd_start (GstBaseTransform * bt)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) bt;
  self->alloc = gst_dmabuf_allocator_new ();
  self->ready = self->failed = self->flushing = FALSE;
  return TRUE;
}

static gboolean
wd_stop (GstBaseTransform * bt)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) bt;
  g_mutex_lock (&self->lock);
  self->flushing = TRUE;
  g_cond_broadcast (&self->cond);
  g_mutex_unlock (&self->lock);
  if (self->ctx) {
    gst_gl_context_thread_add (self->ctx, (GstGLContextThreadFunc) slots_free_gl, self);
    gst_clear_object (&self->ctx);
  }
  /* A slot still on screen keeps its memory (and fd) alive through the buffer's reference. */
  for (int i = 0; i < WPEDMABUF_SLOTS; i++) {
    Slot *s = &self->slots[i];
    if (s->mem) {
      gst_memory_unref (s->mem);
      s->mem = NULL;
    }
    s->fd = 0;
  }
  gst_clear_object (&self->alloc);
  self->ready = FALSE;
  return TRUE;
}

static gboolean
wd_sink_event (GstBaseTransform * bt, GstEvent * ev)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) bt;
  if (GST_EVENT_TYPE (ev) == GST_EVENT_FLUSH_START) {
    g_mutex_lock (&self->lock);
    self->flushing = TRUE;
    g_cond_broadcast (&self->cond);
    g_mutex_unlock (&self->lock);
  } else if (GST_EVENT_TYPE (ev) == GST_EVENT_FLUSH_STOP) {
    g_mutex_lock (&self->lock);
    self->flushing = FALSE;
    g_mutex_unlock (&self->lock);
  }
  return GST_BASE_TRANSFORM_CLASS (gst_wpe_dmabuf_parent_class)->sink_event (bt, ev);
}

static void
wd_finalize (GObject * o)
{
  GstWpeDmabuf *self = (GstWpeDmabuf *) o;
  g_mutex_clear (&self->lock);
  g_cond_clear (&self->cond);
  G_OBJECT_CLASS (gst_wpe_dmabuf_parent_class)->finalize (o);
}

static void
gst_wpe_dmabuf_class_init (GstWpeDmabufClass * k)
{
  GstElementClass *ek = GST_ELEMENT_CLASS (k);
  GstBaseTransformClass *bk = GST_BASE_TRANSFORM_CLASS (k);
  G_OBJECT_CLASS (k)->finalize = wd_finalize;
  gst_element_class_add_static_pad_template (ek, &sink_tmpl);
  gst_element_class_add_static_pad_template (ek, &src_tmpl);
  gst_element_class_set_static_metadata (ek, "GL texture to linear scanout dmabuf", "Filter/Video",
      "GPU-copies GL frames into linear CMA dmabufs for a display plane", "CuTePi");
  bk->transform_caps = wd_transform_caps;
  bk->set_caps = wd_set_caps;
  bk->prepare_output_buffer = wd_prepare_output_buffer;
  bk->transform = wd_transform;
  bk->start = wd_start;
  bk->stop = wd_stop;
  bk->sink_event = wd_sink_event;
  bk->passthrough_on_same_caps = FALSE;
  slot_quark = g_quark_from_static_string ("wpedmabuf-slot");
  create_image = (PFNEGLCREATEIMAGEKHRPROC) eglGetProcAddress ("eglCreateImageKHR");
  destroy_image = (PFNEGLDESTROYIMAGEKHRPROC) eglGetProcAddress ("eglDestroyImageKHR");
  GST_DEBUG_CATEGORY_INIT (wpedmabuf_debug, "wpedmabuf", 0, "wpedmabuf");
}

static void
gst_wpe_dmabuf_init (GstWpeDmabuf * self)
{
  g_mutex_init (&self->lock);
  g_cond_init (&self->cond);
}

/* Registers the element in this process (no plugin file). */
int
scanout_register (void)
{
  return gst_element_register (NULL, "wpedmabuf", GST_RANK_NONE, gst_wpe_dmabuf_get_type ());
}

