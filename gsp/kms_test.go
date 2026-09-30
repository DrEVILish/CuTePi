package gsp

import (
	"testing"
	"unsafe"
)

// The ioctl structs must match the kernel's drm_mode.h layouts byte for byte.
func TestKMSIoctlStructSizes(t *testing.T) {
	for name, got := range map[string]uintptr{
		"drm_set_client_cap":          unsafe.Sizeof(drmSetClientCap{}),
		"drm_mode_card_res":           unsafe.Sizeof(drmModeCardRes{}),
		"drm_mode_modeinfo":           unsafe.Sizeof(drmModeInfo{}),
		"drm_mode_crtc":               unsafe.Sizeof(drmModeCRTC{}),
		"drm_mode_get_plane_res":      unsafe.Sizeof(drmModeGetPlaneRes{}),
		"drm_mode_get_plane":          unsafe.Sizeof(drmModeGetPlane{}),
		"drm_mode_obj_get_properties": unsafe.Sizeof(drmModeObjGetProps{}),
		"drm_mode_get_property":       unsafe.Sizeof(drmModeGetProperty{}),
		"drm_mode_obj_set_property":   unsafe.Sizeof(drmModeObjSetProperty{}),
	} {
		want := map[string]uintptr{
			"drm_set_client_cap": 16, "drm_mode_card_res": 64, "drm_mode_modeinfo": 68,
			"drm_mode_crtc": 104, "drm_mode_get_plane_res": 16, "drm_mode_get_plane": 32,
			"drm_mode_obj_get_properties": 32, "drm_mode_get_property": 64, "drm_mode_obj_set_property": 24,
		}[name]
		if got != want {
			t.Errorf("sizeof(%s) = %d, want %d", name, got, want)
		}
	}
}

func TestRotationBits(t *testing.T) {
	for _, c := range []struct {
		deg  int
		flip string
		bits uint64
		ok   bool
	}{
		{0, "", rotate0, true},
		{180, "", rotate180, true},
		{0, "h", rotate0 | reflectX, true},
		{180, "v", rotate180 | reflectY, true},
		{90, "", 0, false},
		{270, "h", 0, false},
	} {
		bits, ok := rotationBits(c.deg, c.flip)
		if bits != c.bits || ok != c.ok {
			t.Errorf("rotationBits(%d,%q) = %d,%v want %d,%v", c.deg, c.flip, bits, ok, c.bits, c.ok)
		}
	}
}

// On hardware with a display attached: the probe finds the active mode and
// overlay planes carrying alpha and zpos. Skips anywhere else.
func TestKMSWallProbe(t *testing.T) {
	w, err := openKMSWall()
	if err != nil {
		t.Skipf("no KMS display: %v", err)
	}
	if w.Width == 0 || w.Height == 0 || w.Refresh == 0 {
		t.Fatalf("mode %dx%d@%d", w.Width, w.Height, w.Refresh)
	}
	if len(w.planes) < 2 {
		t.Fatalf("overlay planes = %d, want at least 2 for a crossfade", len(w.planes))
	}
	t.Logf("display %dx%d@%dHz, %d overlay planes, crtc %d", w.Width, w.Height, w.Refresh, len(w.planes), w.crtcID)
}
