/* planewall: the plane wall's presenter and cutepiplanesink (planewall.c). */
#ifndef CUTEPI_PLANEWALL_H
#define CUTEPI_PLANEWALL_H
#include <stdint.h>

typedef struct {
  uint64_t commits, fails;
  double p50_ms, p99_ms, max_ms;   /* intervals between commits */
} planewall_stats_t;

int planewall_open (int drm_fd, uint32_t crtc_id, const uint32_t * planes, int nplanes, int w, int h, int hz, char **err);
int planewall_is_open (void);
int planewall_set (uint32_t plane, const char *name, uint64_t value);
void planewall_detach (uint32_t plane);
void planewall_stats (planewall_stats_t * st);
void planewall_plane_stats (uint32_t plane, uint64_t * shown, uint64_t * dropped, uint64_t * steps, int *first, int64_t * t_us);
int cutepi_planesink_register (void);
#endif
