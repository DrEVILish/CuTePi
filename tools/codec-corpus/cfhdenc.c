/* cfhdenc: encode raw frames with the CineForm SDK (libcutepi-cfhd.so) into a MOV, for the codec corpus.
 * FFmpeg's own CineForm encoder writes streams the SDK rejects (two thirds of the frames of a 4:2:2 file fail to
 * decode), so the corpus uses real CineForm from the SDK that defines it.
 *
 *   ffmpeg -i SRC -f rawvideo -pix_fmt yuyv422 - | cfhdenc 1920 1080 60 422 OUT.mov       (YUV 4:2:2)
 *   ffmpeg -i SRC -f rawvideo -pix_fmt bgra    - | cfhdenc 1920 1080 60 4444 OUT.mov      (RGBA 4:4:4:4)
 *   ffmpeg -i SRC -f rawvideo -pix_fmt bgra    - | cfhdenc 1920 1080 60 444 OUT.mov       (RGB 4:4:4)
 * Optional 6th argument: quality low|medium|high|film1|film2 (default film1).
 *
 * Build: cc -O2 -o cfhdenc cfhdenc.c -I third_party/cineform-sdk/Common $(pkg-config --cflags --libs gstreamer-app-1.0)
 *        -L third_party/cineform-sdk -lcutepi-cfhd */
#include <gst/gst.h>
#include <gst/app/gstappsrc.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "CFHDEncoder.h"

int main (int argc, char **argv)
{
  if (argc < 6) {
    fprintf (stderr, "usage: cfhdenc W H FPS 422|444|4444 OUT.mov [low|medium|high|film1|film2]\n");
    return 2;
  }
  int w = atoi (argv[1]), h = atoi (argv[2]), fps = atoi (argv[3]);
  int alpha = !strcmp (argv[4], "4444"), rgb = alpha || !strcmp (argv[4], "444");
  const char *q = argc > 6 ? argv[6] : "film1";
  CFHD_EncodingQuality quality = !strcmp (q, "low") ? CFHD_ENCODING_QUALITY_LOW : !strcmp (q, "medium") ?
      CFHD_ENCODING_QUALITY_MEDIUM : !strcmp (q, "high") ? CFHD_ENCODING_QUALITY_HIGH : !strcmp (q, "film2") ?
      CFHD_ENCODING_QUALITY_FILMSCAN2 : CFHD_ENCODING_QUALITY_FILMSCAN1;
  CFHD_PixelFormat in = rgb ? CFHD_PIXEL_FORMAT_BGRA : CFHD_PIXEL_FORMAT_YUY2;
  CFHD_EncodedFormat enc = alpha ? CFHD_ENCODED_FORMAT_RGBA_4444 : rgb ? CFHD_ENCODED_FORMAT_RGB_444 : CFHD_ENCODED_FORMAT_YUV_422;
  int bpp = rgb ? 4 : 2, pitch = w * bpp;
  size_t fsz = (size_t) pitch * h;

  CFHD_EncoderRef e = NULL;
  if (CFHD_OpenEncoder (&e, NULL) || CFHD_PrepareToEncode (e, w, h, in, enc, CFHD_ENCODING_FLAGS_NONE, quality)) {
    fprintf (stderr, "cfhdenc: encoder setup failed\n");
    return 1;
  }
  gst_init (&argc, &argv);
  gchar *desc = g_strdup_printf ("appsrc name=a format=time caps=video/x-cineform,width=%d,height=%d,framerate=%d/1,"
      "pixel-aspect-ratio=1/1 ! m.video_0 qtmux name=m ! filesink location=\"%s\"", w, h, fps, argv[5]);
  GError *err = NULL;
  GstElement *p = gst_parse_launch (desc, &err);
  if (!p) {
    fprintf (stderr, "cfhdenc: %s\n", err->message);
    return 1;
  }
  GstElement *a = gst_bin_get_by_name (GST_BIN (p), "a");
  gst_element_set_state (p, GST_STATE_PLAYING);
  unsigned char *frame = malloc (fsz);
  /* BGRA from FFmpeg is top-down; the SDK's RGB inputs are bottom-up ("inverted"): pass the last row and a
   * negative pitch. */
  unsigned char *start = rgb ? frame + fsz - pitch : frame;
  int epitch = rgb ? -pitch : pitch;
  long n = 0;
  size_t total = 0;
  while (fread (frame, 1, fsz, stdin) == fsz) {
    void *data;
    size_t sz;
    if (CFHD_EncodeSample (e, start, epitch) || CFHD_GetSampleData (e, &data, &sz)) {
      fprintf (stderr, "cfhdenc: encode failed at frame %ld\n", n);
      return 1;
    }
    GstBuffer *b = gst_buffer_new_memdup (data, sz);
    GST_BUFFER_PTS (b) = gst_util_uint64_scale (n, GST_SECOND, fps);
    GST_BUFFER_DURATION (b) = gst_util_uint64_scale (1, GST_SECOND, fps);
    if (gst_app_src_push_buffer (GST_APP_SRC (a), b) != GST_FLOW_OK)
      break;
    total += sz;
    n++;
  }
  gst_app_src_end_of_stream (GST_APP_SRC (a));
  GstMessage *m = gst_bus_timed_pop_filtered (GST_ELEMENT_BUS (p), GST_CLOCK_TIME_NONE, GST_MESSAGE_EOS | GST_MESSAGE_ERROR);
  gst_message_unref (m);
  gst_element_set_state (p, GST_STATE_NULL);
  CFHD_CloseEncoder (e);
  fprintf (stderr, "cfhdenc: %ld frames, %.1f Mbit/s\n", n, n ? total * 8.0 * fps / n / 1e6 : 0);
  return 0;
}
