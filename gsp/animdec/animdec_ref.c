/* Test reference for animdec.c: libwebp's own animation decoder
 * (WebPAnimDecoder), which composites frames itself, to check the element's
 * frame-parallel decode and compositor against. Loaded at run time. */
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>
#include <webp/demux.h>

typedef struct {
  WebPAnimDecoder *dec;
  int w, h, n;
} cutepi_webp_ref;

static WebPAnimDecoder *(*ref_new) (const WebPData *, const WebPAnimDecoderOptions *, int);
static int (*ref_opts) (WebPAnimDecoderOptions *, int);
static int (*ref_info) (const WebPAnimDecoder *, WebPAnimInfo *);
static int (*ref_next) (WebPAnimDecoder *, uint8_t **, int *);
static void (*ref_delete) (WebPAnimDecoder *);

/* Open data (kept by the caller) for reference decoding; NULL on failure. */
void *
cutepi_webp_ref_open (const unsigned char *data, size_t len, int *w, int *h, int *n)
{
  void *l = dlopen ("libwebpdemux.so.2", RTLD_NOW | RTLD_LOCAL);
  if (!l || !(*(void **) &ref_new = dlsym (l, "WebPAnimDecoderNewInternal")) ||
      !(*(void **) &ref_opts = dlsym (l, "WebPAnimDecoderOptionsInitInternal")) ||
      !(*(void **) &ref_info = dlsym (l, "WebPAnimDecoderGetInfo")) ||
      !(*(void **) &ref_next = dlsym (l, "WebPAnimDecoderGetNext")) ||
      !(*(void **) &ref_delete = dlsym (l, "WebPAnimDecoderDelete")))
    return NULL;
  WebPAnimDecoderOptions o;
  if (!ref_opts (&o, WEBP_DEMUX_ABI_VERSION)) return NULL;
  o.color_mode = MODE_RGBA;
  WebPData wd = { data, len };
  WebPAnimDecoder *d = ref_new (&wd, &o, WEBP_DEMUX_ABI_VERSION);
  WebPAnimInfo ai;
  if (!d || !ref_info (d, &ai)) return NULL;
  *w = ai.canvas_width; *h = ai.canvas_height; *n = ai.frame_count;
  return d;
}

/* The next composited frame (w*h*4 RGBA) into out; 0 at the end. */
int
cutepi_webp_ref_next (void *d, unsigned char *out)
{
  uint8_t *buf; int ts; WebPAnimInfo ai;
  if (!ref_info (d, &ai) || !ref_next (d, &buf, &ts)) return 0;
  memcpy (out, buf, (size_t) ai.canvas_width * ai.canvas_height * 4);
  return 1;
}

void
cutepi_webp_ref_close (void *d)
{
  ref_delete (d);
}
