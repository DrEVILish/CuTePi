/* bridgebench [-pool N] [-qmax N] "<cue 0 chain>" ["<cue 1 chain>" ...]
 * Each cue chain is a gst-launch description (source .. decoder); it runs in its
 * own pipeline ending in an appsink. The wall pipeline has one appsrc per cue ->
 * glupload -> glcolorconvert -> queue -> glvideomixerelement -> fakesink.
 * A C thread per layer pulls samples and pushes the buffers (shallow copy, no
 * pixel copy). Counts frames pulled, pushed and mixed. Headless throughput. */
#include <gst/gst.h>
#include <gst/app/gstappsink.h>
#include <gst/app/gstappsrc.h>
#include <gst/video/video.h>
#include <gst/gl/gl.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <unistd.h>
#define MAXL 4
static int pool_min = 8;
static guint64 mixed = 0, pulled[MAXL], pushed[MAXL];
typedef struct { GstElement *sink; GstElement *src; int i; } Layer;
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
    GstBuffer *b = gst_buffer_copy(gst_sample_get_buffer(s)); /* shallow: memory shared */
    gst_sample_unref(s);
    if (gst_app_src_push_buffer(GST_APP_SRC(l->src), b) != GST_FLOW_OK) return NULL;
    pushed[l->i]++;
  }
}
int main(int argc, char **argv) {
  gst_init(&argc, &argv);
  int qmax = 3, a = 1, display = 0, cuesync = 0, split = 0; long mixlat = 33000000; const char *dsink = "glimagesink sync=true";
  while (a < argc && argv[a][0] == '-') {
    if (!strcmp(argv[a], "-pool")) pool_min = atoi(argv[++a]);
    else if (!strcmp(argv[a], "-qmax")) qmax = atoi(argv[++a]);
    else if (!strcmp(argv[a], "-display")) display = 1;
    else if (!strcmp(argv[a], "-cuesync")) cuesync = 1;
    else if (!strcmp(argv[a], "-sink")) dsink = argv[++a];
    else if (!strcmp(argv[a], "-split")) split = 1;
    else if (!strcmp(argv[a], "-mixlat")) mixlat = atol(argv[++a]) * 1000000L;
    a++;
  }
  int n = argc - a; if (n < 1 || n > MAXL) { fprintf(stderr, "1..%d cue chains\n", MAXL); return 2; }
  /* -display: real time on HDMI. A live GPU black source paces the mixer at
   * 60 fps (bridge rule), glimagesink presents, every pipeline shares one
   * clock and base time so cue timestamps are wall timestamps. */
  GString *w = display ? g_string_new(NULL) : NULL;
  if (display) g_string_printf(w,
      "gltestsrc is-live=true pattern=black ! video/x-raw(memory:GLMemory),width=1920,height=1080,framerate=60/1 ! m.sink_0 "
      "glvideomixerelement name=m background=black latency=%ld ! video/x-raw(memory:GLMemory),width=1920,height=1080,framerate=60/1,format=RGBA ! "
      "identity name=wsink ! %s", mixlat, split ? "appsink name=wout sync=false max-buffers=2 drop=false" : dsink);
  else w = g_string_new(
      "glvideomixerelement name=m background=black ! "
      "video/x-raw(memory:GLMemory),width=1920,height=1080,format=RGBA ! fakesink name=wsink sync=false");
  for (int i = 0; i < n; i++)
    g_string_append_printf(w, " appsrc name=src%d format=time max-buffers=%d block=true ! glupload ! glcolorconvert ! "
      "video/x-raw(memory:GLMemory),format=RGBA ! queue max-size-buffers=2 name=out%d", i, qmax, i);
  GError *err = NULL;
  GstElement *wall = gst_parse_launch(w->str, &err);
  if (!wall) { fprintf(stderr, "wall: %s\n", err->message); return 2; }
  GstElement *m = gst_bin_get_by_name(GST_BIN(wall), "m");
  Layer L[MAXL]; GstElement *cue[MAXL];
  for (int i = 0; i < n; i++) {
    char nm[16]; snprintf(nm, sizeof nm, "out%d", i);
    /* (with -display the black source holds sink_0; cue pads follow) */
    GstPad *src = gst_element_get_static_pad(gst_bin_get_by_name(GST_BIN(wall), nm), "src");
    gst_pad_link_full(src, gst_element_request_pad_simple(m, "sink_%u"), GST_PAD_LINK_CHECK_NOTHING);
    GString *c = g_string_new(argv[a + i]);
    g_string_append_printf(c, " ! appsink name=sink sync=%s max-buffers=2 drop=false", (display && cuesync) ? "true" : "false");
    cue[i] = gst_parse_launch(c->str, &err);
    if (!cue[i]) { fprintf(stderr, "cue %d: %s\n", i, err->message); return 2; }
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
    gst_app_src_set_caps(GST_APP_SRC(L[i].src), gst_sample_get_caps(ps));
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
    printf("\n"); fflush(stdout); fflush(stderr); _exit(0);
  }
  if (display) { /* the live black source never ends: run for the clip length */
    g_usleep(9 * G_USEC_PER_SEC);
    double s = (g_get_monotonic_time() - t0) / 1e6;
    printf("display: mixer out %llu in %.2f s = %.1f fps;", (unsigned long long)mixed, s, mixed / s);
    for (int i = 0; i < n; i++) printf(" layer %d pulled %llu pushed %llu;", i, (unsigned long long)pulled[i], (unsigned long long)pushed[i]);
    printf("\n"); fflush(stdout); _exit(0);
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
