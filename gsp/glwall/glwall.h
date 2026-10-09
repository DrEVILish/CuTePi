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
/* Crop the layer's picture as it reaches the mixer, pixels per edge (the
 * mixer pad's crop: no extra pass). */
void glwall_layer_set_crop(glwall_layer *l, int left, int right, int top, int bottom);
/* A fade evaluated for every output frame from that frame's clock time:
 * alpha = from + (to - from) * curve((T - start_ns) / dur_ns), holding `to`
 * once done. start_ns is on the system clock (glwall_now). A set_alpha ends
 * it. Curves as gsp's fadeShape. */
enum { GLWALL_CURVE_LINEAR, GLWALL_CURVE_SMOOTH, GLWALL_CURVE_LOG, GLWALL_CURVE_EXP };
void glwall_layer_ramp(glwall_layer *l, double from, double to, uint64_t start_ns, uint64_t dur_ns, int curve);
uint64_t glwall_now(void);
/* The ramp's envelope at t (0..1, clamped), for tests. */
double glwall_ramp_shape(int curve, double t);
/* How long after its time a frame is seen (mid-screen): the presenter's
 * moving average of time -> latch, plus half a refresh (ns). */
int64_t glwall_display_delay(void);
/* Delay a cue's audio sink (its ts-offset) by the display delay, so sound
 * and picture stay together. A non-live cue pipeline does not pass a
 * configured latency on to its sinks (measured: 0), so it is set per sink. */
void glwall_align_audio(GstElement *sink);
/* The largest ts-offset on a pipeline's sinks other than the video appsink, ns (-1: none). */
int64_t glwall_audio_offset(GstElement *pipeline);
/* keep_aspect: letterbox inside the rectangle (fit) rather than stretch. */
void glwall_layer_set_rect(glwall_layer *l, int x, int y, int w, int h, int keep_aspect);
/* Every cue pipeline runs on the system clock, like the wall, so the pump's
 * base-time mapping holds (an audio sink would otherwise impose its own). */
void glwall_use_system_clock(GstElement *pipeline);
/* glwall_share_gl_display gives e (a pipeline) the process-wide GstGLDisplay. */
void glwall_share_gl_display(GstElement *e);
/* glwall_layer_free detaches the layer. The cue pipeline must be in NULL
 * (its appsink has released the pump). */
void glwall_layer_free(glwall_layer *l);

/* Counters since open: frames the mixer produced, frames put on the plane. */
void glwall_stats(uint64_t *mixed, uint64_t *presented);
/* Ring pool diagnostics: allocs/frees of ring-backed buffers, allocation
 * failures, pool configs/activations, mixer allocation queries answered,
 * slots wrapped by live buffers, the slot on screen, frames queued,
 * frames skipped by the presenter to catch up. */
typedef struct {
  uint64_t allocs, frees, exhausted, set_configs, activations, alloc_queries, skipped;
  /* presenter: total and maximum (since the last read) microseconds waiting
   * for the GPU fence and in the SetPlane commit; commits longer than a refresh */
  uint64_t fence_us, fence_max_us, flip_us, flip_max_us, flips_long;
  /* GPU time from fence to signal when the presenter had to wait (µs), and
   * how late mixed frames left the wall's sink against their due time (µs) */
  uint64_t gpu_us, gpu_max_us, late_us, late_max_us;
  uint64_t unsnapped; /* frames presented without a mixer snapshot (uncounted) */
  int allocated, onscreen, queued;
} glwall_pool_stats_t;
void glwall_pool_stats(glwall_pool_stats_t *st);

/* Per layer: frames pulled from the cue, pushed to the mixer, opacity changes applied. */
void glwall_layer_stats(glwall_layer *l, uint64_t *pulled, uint64_t *pushed, uint64_t *steps);
/* Per layer, counted on presented output frames only: frames that showed a
 * new cue frame, and frames whose opacity differed from the previous one
 * shown (a fade's steps as the viewer sees them); and cue frames that
 * arrived too late for their output frame (shown on arrival, decoder told
 * through QoS), and how late the last cue frame was (ns, 0 when on time). */
void glwall_layer_shown(glwall_layer *l, uint64_t *frames, uint64_t *steps, uint64_t *late, int64_t *lag_ns);

#endif
