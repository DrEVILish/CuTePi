package gsp

// GPU compositor wall selection (DESIGN §6.1.1). CUTEPI_WALL=gl runs the
// always-on GL mixer (gsp/glwall) on top of the KMS display owner: the
// service keeps the DRM master and its planes (the panic image stays on its
// own plane above), and the wall presents the mixed picture on one overlay
// plane. The KMS plane wall remains the default.

import (
	"os"
	"sync"

	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// glWallEnabled reports whether the GPU wall is selected.
func glWallEnabled() bool { return os.Getenv("CUTEPI_WALL") == "gl" }

// glEnv points GStreamer's GL at EGL on the render node (no window system):
// the wall's context never touches /dev/dri/card*, which the KMS owner holds.
func glEnv() {
	if !glWallEnabled() {
		return
	}
	for k, v := range map[string]string{"GST_GL_API": "gles2", "GST_GL_PLATFORM": "egl", "GST_GL_WINDOW": "surfaceless"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

var (
	glOnce  sync.Once
	glPlane *kmsPlane
	glOpen  bool
)

// glWall opens the GPU wall once the KMS display is open: it takes the
// lowest overlay plane (zpos 1, fully opaque, straight alpha) for the
// composited picture. Returns whether the GL wall is running.
func glWall() bool {
	if !glWallEnabled() {
		return false
	}
	glOnce.Do(func() {
		w := kmsWall()
		if w == nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall needs the KMS display: not available")
			return
		}
		p := w.acquire()
		if p == nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall: no free display plane")
			return
		}
		_ = w.set(p, "zpos", 1)
		_ = w.set(p, "pixel blend mode", blendCoverage)
		_ = w.setAlpha(p, 1)
		if err := glwall.Open(w.fd, w.crtcID, p.id, w.Width, w.Height, w.Refresh); err != nil {
			logs.Printf(logs.GSPPipeDebug, "gsp: GL wall unavailable, staying on the KMS plane wall: %v", err)
			w.release(p)
			return
		}
		glPlane, glOpen = p, true
		logs.Printf(logs.GSPPipeDebug, "gsp: GL wall on plane %d at %dx%d@%d", p.id, w.Width, w.Height, w.Refresh)
	})
	return glOpen
}

// GLWallStats reports frames mixed and presented by the GPU wall.
func GLWallStats() (mixed, presented uint64, on bool) {
	if !glOpen {
		return 0, 0, false
	}
	m, p := glwall.Stats()
	return m, p, true
}
