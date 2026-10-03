/* GPU compositor wall (DESIGN §6.1.1): one always-running GStreamer GL
 * pipeline composites every cue layer on the Pi's V3D GPU and the service
 * presents the result on a KMS plane it already owns. See glwall.c. */
#ifndef CUTEPI_GLWALL_H
#define CUTEPI_GLWALL_H

#include <gst/gst.h>
#include <stdint.h>

typedef struct glwall_layer glwall_layer;

/* glwall_open builds the wall on the display described (the service's DRM
 * fd, which stays the DRM master; the CRTC; one overlay plane the wall owns
 * for its lifetime; the mode) and starts presenting black. 0 on success,
 * else -1 with an explanation in *err (caller g_free()s it). */
int glwall_open(int drm_fd, uint32_t crtc_id, uint32_t plane_id, int width, int height, int refresh_hz, char **err);
void glwall_close(void);
int glwall_is_open(void);

/* glwall_prepare_sink readies a cue pipeline's appsink before the cue
 * negotiates: it answers the decoder's allocation query (video meta and a
 * pool-size hint, without which a V4L2 decoder stalls or the ISP copies). */
void glwall_prepare_sink(GstElement *appsink, int pool_buffers);

/* glwall_layer_attach joins a prerolled cue (its appsink holds a preroll
 * sample) to the wall as a new layer: a bounded appsrc feeding the mixer,
 * with a pump thread moving frames in C and re-stamping them from the cue's
 * running time onto the wall clock. colorimetry (e.g. "bt709") is set on the
 * layer caps when the cue's carry none. The layer starts at alpha 0 at the
 * bottom. NULL on failure. */
glwall_layer *glwall_layer_attach(GstElement *appsink, const char *colorimetry, char **err);
void glwall_layer_set_alpha(glwall_layer *l, double alpha);
void glwall_layer_set_zorder(glwall_layer *l, int zorder);
/* keep_aspect: letterbox inside the rectangle (fit) rather than stretch. */
void glwall_layer_set_rect(glwall_layer *l, int x, int y, int w, int h, int keep_aspect);
/* Every cue pipeline runs on the system clock, like the wall, so the pump's
 * base-time mapping holds (an audio sink would otherwise impose its own). */
void glwall_use_system_clock(GstElement *pipeline);
/* glwall_layer_free detaches the layer. The cue pipeline must be in NULL
 * (its appsink has released the pump). */
void glwall_layer_free(glwall_layer *l);

/* Counters since open: frames the mixer produced, frames put on the plane. */
void glwall_stats(uint64_t *mixed, uint64_t *presented);
/* Per layer: frames pulled from the cue and pushed to the mixer. */
void glwall_layer_stats(glwall_layer *l, uint64_t *pulled, uint64_t *pushed);

#endif
