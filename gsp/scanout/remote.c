/* Out-of-process live pages. WebKit renders in a child of the service (cutepi --livepage-render): wpevideosrc !
 * wpedmabuf ! appsink. The child passes its ring of linear scanout buffers to the service once, as dmabuf file
 * descriptors over a SOCK_SEQPACKET socket (SCM_RIGHTS), then one small message per frame naming the slot. The
 * service wraps the slots as GstBuffers and pushes them from an appsrc into the cue's pipeline (the GPU wall
 * imports them, the KMS wall scans them out), and hands each slot back when its buffer is freed.
 *
 * Why: tearing a WebKit view down inside the service crashed it now and then (GStreamer 1.26 wpe plugin,
 * WPEBackend-fdo release_exported_image on the view being deleted: 1 stop in 15 on the GPU wall). In the child
 * the view is never torn down: when the service closes the socket the child exits at once. */
#include <gst/gst.h>
#include <gst/app/gstappsrc.h>
#include <gst/app/gstappsink.h>
#include <gst/video/video.h>
#include <gst/allocators/gstdmabuf.h>
#include <errno.h>
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#define SLOTS 8

enum { MSG_SLOT = 1, MSG_FRAME, MSG_PROGRESS, MSG_RELEASE, MSG_ERROR };

typedef struct {
  uint32_t type, idx, width, height, stride, size;
  uint64_t pts;
  double progress;
  char text[128];
} Msg;

static int
send_msg (int sock, const Msg * m, int fd)
{
  struct iovec io = { .iov_base = (void *) m, .iov_len = sizeof *m };
  char ctl[CMSG_SPACE (sizeof (int))];
  struct msghdr h = { .msg_iov = &io, .msg_iovlen = 1 };
  if (fd >= 0) {
    memset (ctl, 0, sizeof ctl);
    h.msg_control = ctl;
    h.msg_controllen = sizeof ctl;
    struct cmsghdr *c = CMSG_FIRSTHDR (&h);
    c->cmsg_level = SOL_SOCKET;
    c->cmsg_type = SCM_RIGHTS;
    c->cmsg_len = CMSG_LEN (sizeof (int));
    memcpy (CMSG_DATA (c), &fd, sizeof (int));
  }
  return sendmsg (sock, &h, MSG_NOSIGNAL) == (ssize_t) sizeof *m ? 0 : -1;
}

/* Returns 1 with a message (and *fd when one came along), 0 on EOF, -1 on error. */
static int
recv_msg (int sock, Msg * m, int *fd)
{
  struct iovec io = { .iov_base = m, .iov_len = sizeof *m };
  char ctl[CMSG_SPACE (sizeof (int))];
  struct msghdr h = { .msg_iov = &io, .msg_iovlen = 1, .msg_control = ctl, .msg_controllen = sizeof ctl };
  ssize_t n = recvmsg (sock, &h, MSG_CMSG_CLOEXEC);
  if (n == 0)
    return 0;
  if (n != (ssize_t) sizeof *m)
    return -1;
  if (fd) {
    *fd = -1;
    for (struct cmsghdr * c = CMSG_FIRSTHDR (&h); c; c = CMSG_NXTHDR (&h, c))
      if (c->cmsg_level == SOL_SOCKET && c->cmsg_type == SCM_RIGHTS)
        memcpy (fd, CMSG_DATA (c), sizeof (int));
  }
  return 1;
}

/* ---- child ---- */

int scanout_register (void);

/* Renders url in this process and serves the frames on sock until the service closes it. Returns an exit code;
 * on the service closing the socket it _exit()s without tearing WebKit down. */
int
livepage_child_run (const char *url, int w, int h, int fps, int transparent, int sock)
{
  if (!scanout_register ())
    return 2;
  gchar *desc = g_strdup_printf ("wpevideosrc name=src draw-background=%s ! video/x-raw(memory:GLMemory),"
      "format=RGBA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1 ! wpedmabuf ! "
      "appsink name=k sync=false max-buffers=2 drop=false enable-last-sample=false",
      transparent ? "false" : "true", w, h, fps);
  GError *e = NULL;
  GstElement *p = gst_parse_launch (desc, &e);
  g_free (desc);
  if (!p) {
    Msg m = { .type = MSG_ERROR };
    g_strlcpy (m.text, e ? e->message : "pipeline", sizeof m.text);
    send_msg (sock, &m, -1);
    return 2;
  }
  GstElement *src = gst_bin_get_by_name (GST_BIN (p), "src");
  g_object_set (src, "location", url, NULL);
  GstElement *sink = gst_bin_get_by_name (GST_BIN (p), "k");
  GstBus *bus = gst_element_get_bus (p);
  gst_element_set_state (p, GST_STATE_PLAYING);

  int fds[SLOTS];
  GstSample *held[SLOTS] = { 0 };
  int nslots = 0;
  for (;;) {
    /* Slots handed back, or the service gone. */
    struct pollfd pf = { .fd = sock, .events = POLLIN };
    while (poll (&pf, 1, 0) > 0) {
      Msg m;
      int r = recv_msg (sock, &m, NULL);
      if (r <= 0)
        _exit (0);      /* the cue stopped: leave WebKit as it is (its teardown is what crashes) */
      if (m.type == MSG_RELEASE && m.idx < SLOTS && held[m.idx]) {
        gst_sample_unref (held[m.idx]);
        held[m.idx] = NULL;
      }
    }
    GstMessage *bm;
    while ((bm = gst_bus_pop_filtered (bus, GST_MESSAGE_ELEMENT | GST_MESSAGE_ERROR))) {
      Msg m = { 0 };
      if (GST_MESSAGE_TYPE (bm) == GST_MESSAGE_ERROR) {
        GError *ge = NULL;
        gst_message_parse_error (bm, &ge, NULL);
        m.type = MSG_ERROR;
        g_strlcpy (m.text, ge ? ge->message : "error", sizeof m.text);
        g_clear_error (&ge);
        send_msg (sock, &m, -1);
        _exit (3);
      }
      const GstStructure *st = gst_message_get_structure (bm);
      if (st && gst_structure_has_name (st, "wpe-stats") &&
          gst_structure_get_double (st, "estimated-load-progress", &m.progress)) {
        m.type = MSG_PROGRESS;
        send_msg (sock, &m, -1);
      }
      gst_message_unref (bm);
    }
    GstSample *s = gst_app_sink_try_pull_sample (GST_APP_SINK (sink), 20 * GST_MSECOND);
    if (!s)
      continue;
    GstBuffer *b = gst_sample_get_buffer (s);
    GstMemory *mem = gst_buffer_peek_memory (b, 0);
    int fd = gst_is_dmabuf_memory (mem) ? gst_dmabuf_memory_get_fd (mem) : -1;
    GstVideoMeta *vm = gst_buffer_get_video_meta (b);
    if (fd < 0 || !vm) {
      gst_sample_unref (s);
      continue;
    }
    int idx = -1;
    for (int i = 0; i < nslots; i++)
      if (fds[i] == fd)
        idx = i;
    if (idx < 0) {
      if (nslots == SLOTS) {
        gst_sample_unref (s);
        continue;
      }
      idx = nslots++;
      fds[idx] = fd;
      Msg m = { .type = MSG_SLOT, .idx = idx, .width = vm->width, .height = vm->height, .stride = vm->stride[0],
          .size = gst_memory_get_sizes (mem, NULL, NULL) };
      if (send_msg (sock, &m, fd))
        _exit (0);
    }
    if (held[idx])              /* cannot happen: wpedmabuf reuses a slot only once released */
      gst_sample_unref (held[idx]);
    held[idx] = s;
    Msg m = { .type = MSG_FRAME, .idx = idx, .pts = GST_BUFFER_PTS (b) };
    if (send_msg (sock, &m, -1))
      _exit (0);
  }
}

/* ---- service ---- */

typedef struct {
  gint refs;
  int sock;
  GstElement *appsrc;
  GstAllocator *alloc;
  GstMemory *mem[SLOTS];
  uint32_t width, height, stride;
  GThread *thread;
  gboolean stop;
  gboolean started;             /* the appsrc has been past READY */
} Remote;

typedef struct {
  Remote *r;
  uint32_t idx;
} Release;

static void
remote_unref (Remote * r)
{
  if (!g_atomic_int_dec_and_test (&r->refs))
    return;
  for (int i = 0; i < SLOTS; i++)
    if (r->mem[i])
      gst_memory_unref (r->mem[i]);
  if (r->alloc)
    gst_object_unref (r->alloc);
  gst_object_unref (r->appsrc);
  close (r->sock);
  g_free (r);
}

static void
slot_release (gpointer data)
{
  Release *rel = data;
  Msg m = { .type = MSG_RELEASE, .idx = rel->idx };
  send_msg (rel->r->sock, &m, -1);       /* fails quietly once the child is gone */
  remote_unref (rel->r);
  g_free (rel);
}

static GQuark release_quark;

static gpointer
remote_main (gpointer data)
{
  Remote *r = data;
  gint64 born = g_get_monotonic_time ();
  for (;;) {
    if (g_atomic_int_get (&r->stop))
      break;
    if (!r->started && g_get_monotonic_time () - born > 60 * G_TIME_SPAN_SECOND)
      break;    /* built but never started: do not keep the renderer */
    /* The cue's pipeline went back to NULL (stopped, any path): end the child. */
    GstState cur = GST_STATE (r->appsrc);
    if (cur >= GST_STATE_PAUSED)
      r->started = TRUE;
    else if (r->started && cur == GST_STATE_NULL)
      break;
    struct pollfd pf = { .fd = r->sock, .events = POLLIN };
    if (poll (&pf, 1, 200) <= 0)
      continue;
    Msg m;
    int fd = -1;
    int got = recv_msg (r->sock, &m, &fd);
    if (got <= 0) {
      if (!g_atomic_int_get (&r->stop)) {
        GST_ELEMENT_ERROR (r->appsrc, RESOURCE, READ, ("live page renderer exited"), (NULL));
      }
      break;
    }
    switch (m.type) {
      case MSG_SLOT:
        if (m.idx < SLOTS && fd >= 0) {
          if (r->mem[m.idx])
            gst_memory_unref (r->mem[m.idx]);
          r->mem[m.idx] = gst_dmabuf_allocator_alloc (r->alloc, fd, m.size);       /* owns fd */
          r->width = m.width;
          r->height = m.height;
          r->stride = m.stride;
        } else if (fd >= 0)
          close (fd);
        break;
      case MSG_FRAME:{
        if (m.idx >= SLOTS || !r->mem[m.idx])
          break;
        GstBuffer *b = gst_buffer_new ();
        gst_buffer_append_memory (b, gst_memory_ref (r->mem[m.idx]));
        gsize off[GST_VIDEO_MAX_PLANES] = { 0 };
        gint str[GST_VIDEO_MAX_PLANES] = { (gint) r->stride };
        gst_buffer_add_video_meta_full (b, GST_VIDEO_FRAME_FLAG_NONE, GST_VIDEO_FORMAT_RGBA, r->width, r->height, 1,
            off, str);
        Release *rel = g_new (Release, 1);
        rel->r = r;
        rel->idx = m.idx;
        g_atomic_int_inc (&r->refs);
        gst_mini_object_set_qdata (GST_MINI_OBJECT (b), release_quark, rel, slot_release);
        if (gst_app_src_push_buffer (GST_APP_SRC (r->appsrc), b) == GST_FLOW_FLUSHING && r->started &&
            GST_STATE (r->appsrc) == GST_STATE_NULL)
          goto out;
        break;
      }
      case MSG_PROGRESS:{
        GstStructure *st = gst_structure_new ("wpe-stats", "estimated-load-progress", G_TYPE_DOUBLE, m.progress, NULL);
        gst_element_post_message (r->appsrc, gst_message_new_element (GST_OBJECT (r->appsrc), st));
        break;
      }
      case MSG_ERROR:
        m.text[sizeof m.text - 1] = 0;
        GST_ELEMENT_ERROR (r->appsrc, RESOURCE, FAILED, ("live page: %s", m.text), (NULL));
        goto out;
      default:
        if (fd >= 0)
          close (fd);
    }
  }
out:
  shutdown (r->sock, SHUT_RDWR);        /* the child sees EOF and exits */
  remote_unref (r);
  return NULL;
}

/* Serves the frames arriving on sock into appsrc until the pipeline goes back to NULL or the child exits. Takes
 * ownership of sock. */
void
livepage_remote_start (GstElement * appsrc, int sock)
{
  if (!release_quark)
    release_quark = g_quark_from_static_string ("cutepi-livepage-slot");
  Remote *r = g_new0 (Remote, 1);
  r->refs = 1;
  r->sock = sock;
  r->appsrc = gst_object_ref (appsrc);
  r->alloc = gst_dmabuf_allocator_new ();
  r->thread = g_thread_new ("livepage-remote", remote_main, r);
  g_thread_unref (r->thread);
}
