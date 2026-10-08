package gsp

// GPU compositor wall selection (DESIGN §6.1.1). CUTEPI_WALL=gl runs the
// always-on GL mixer (gsp/glwall) on top of the KMS display owner: the
// service keeps the DRM master and its planes (the panic image stays on its
// own plane above), and the wall presents the mixed picture on one overlay
// plane. The KMS plane wall remains the default.

import (
	"os"
	"sync"
	"time"

	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// glWallEnabled reports whether the GPU wall is selected.
func glWallEnabled() bool { return os.Getenv("CUTEPI_WALL") == "gl" }

// glEnv points GStreamer's GL at EGL on the render node (no window system):
// the wall's context never touches /dev/dri/card*, which the KMS owner holds.
// Set on every wall: live pages render through GL too (livePageGL).
func glEnv() {
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

// GLPoolStats reports the GPU wall's ring pool diagnostics.
func GLPoolStats() glwall.PoolStats {
	if !glOpen {
		return glwall.PoolStats{}
	}
	return glwall.Pool()
}

// GLLayerStats is one cue layer's counters.
type GLLayerStats struct {
	Pulled, Pushed, Steps uint64 // from the cue; Steps: opacity changes requested
	// On presented output frames: new cue frames shown, opacity changes shown.
	ShownFrames, ShownSteps uint64
	Late                    uint64  // cue frames too late for their output frame (shown on arrival)
	LagMs                   float64 // how late the last cue frame was
	Visible, Parked         bool
	Route, Caps             string // the cue's tail ("dmabuf", "alpha", "isp") and the caps it delivers
	Level                   float64
	Seq                     uint64 // attach order: newest highest
}

// GLWallStats reports frames mixed and presented by the GPU wall and the
// attached layers' counters (bottom to top).
func GLWallStats() (mixed, presented uint64, layers []GLLayerStats, on bool) {
	if !glOpen {
		return 0, 0, nil, false
	}
	m, p := glwall.Stats()
	glMu.Lock()
	for _, l := range glLayers {
		if l.layer == nil {
			continue
		}
		a, b, c := l.layer.Stats()
		sf, ss, late, lag := l.layer.Shown()
		layers = append(layers, GLLayerStats{Pulled: a, Pushed: b, Steps: c, ShownFrames: sf, ShownSteps: ss, Late: late, LagMs: float64(lag.Microseconds()) / 1000,
			Visible: l.visible, Parked: l.parked, Level: l.level, Seq: l.seq, Route: l.route, Caps: l.caps})
	}
	glMu.Unlock()
	return m, p, layers, true
}

// GLAudioSync reports the wall's display delay and the current cue's audio
// offset (the ts-offset its sound sink carries; -1 without one). Zero when
// the GL wall is off or nothing plays.
func GLAudioSync() (displayDelay, cueAudioOffset time.Duration) {
	if !glOpen {
		return 0, 0
	}
	displayDelay = glwall.DisplayDelay()
	mgr.mu.Lock()
	p := mgr.pipeline
	mgr.mu.Unlock()
	if p != nil {
		cueAudioOffset = glwall.AudioOffset(p)
	}
	return
}
