/* mixbench "<branch 0>" ["<branch 1>" ...]: each branch is a gst-launch
 * description ending in an element named out<i>; its src pad is linked to a
 * glvideomixerelement request pad without the parse-time caps check, and the
 * mixer feeds a fakesink. Prints frames out of the mixer per second. */
#include <gst/gst.h>
#include <stdio.h>
static guint64 frames = 0;
static GstPadProbeReturn count(GstPad *p, GstPadProbeInfo *i, gpointer d) { frames++; return GST_PAD_PROBE_OK; }
int main(int argc, char **argv) {
  gst_init(&argc, &argv);
  GString *desc = g_string_new("glvideomixerelement name=m background=black ! "
    "video/x-raw(memory:GLMemory),width=1920,height=1080,format=RGBA ! fakesink name=sink sync=false");
  for (int i = 1; i < argc; i++) g_string_append_printf(desc, " %s", argv[i]);
  GError *err = NULL;
  GstElement *pipe = gst_parse_launch(desc->str, &err);
  if (!pipe) { fprintf(stderr, "parse: %s\n", err->message); return 2; }
  GstElement *m = gst_bin_get_by_name(GST_BIN(pipe), "m");
  for (int i = 1; i < argc; i++) {
    char name[16]; snprintf(name, sizeof name, "out%d", i - 1);
    GstElement *o = gst_bin_get_by_name(GST_BIN(pipe), name);
    if (!o) { fprintf(stderr, "no element %s\n", name); return 2; }
    GstPad *src = gst_element_get_static_pad(o, "src");
    GstPad *sink = gst_element_request_pad_simple(m, "sink_%u");
    if (gst_pad_link_full(src, sink, GST_PAD_LINK_CHECK_NOTHING) != GST_PAD_LINK_OK) { fprintf(stderr, "link %s failed\n", name); return 2; }
  }
  GstElement *fs = gst_bin_get_by_name(GST_BIN(pipe), "sink");
  gst_pad_add_probe(gst_element_get_static_pad(fs, "sink"), GST_PAD_PROBE_TYPE_BUFFER, count, NULL, NULL);
  gint64 t0 = g_get_monotonic_time();
  gst_element_set_state(pipe, GST_STATE_PLAYING);
  GstMessage *msg = gst_bus_timed_pop_filtered(gst_element_get_bus(pipe), 120 * GST_SECOND, GST_MESSAGE_EOS | GST_MESSAGE_ERROR);
  double s = (g_get_monotonic_time() - t0) / 1e6;
  if (msg && GST_MESSAGE_TYPE(msg) == GST_MESSAGE_ERROR) { GError *e; gst_message_parse_error(msg, &e, NULL); fprintf(stderr, "error: %s\n", e->message); }
  printf("%llu frames in %.2f s = %.1f fps\n", (unsigned long long)frames, s, frames / s);
  gst_element_set_state(pipe, GST_STATE_NULL);
  return 0;
}
