package gsp

import (
	"time"

	"CuTePi/gsp/webcache"
)

// Live-page asset cache (DESIGN §12.14): thin wrappers over webcache that
// make sure GStreamer is up first.

// PageCacheAvailable reports whether live-page caching works on this machine.
func PageCacheAvailable() bool {
	gstInit()
	return webcache.Available()
}

// PreloadPage loads url off screen so its assets are cached; it gives up as
// soon as keepGoing reports false.
func PreloadPage(url string, keepGoing func() bool) error {
	gstInit()
	return webcache.Preload(url, liveFrameWait, keepGoing)
}

// ClearPageCaches drops the cached assets of the sites of remove that no
// host in keep shares (everything when keep is empty).
func ClearPageCaches(remove, keep []string) ([]string, error) {
	gstInit()
	return webcache.Clear(remove, keep, 10*time.Second)
}

// ErrPreloadStopped is PreloadPage's "stopped because the wall got busy".
var ErrPreloadStopped = webcache.ErrStopped

// WallIdle reports that nothing is on the wall (no cue, no test pattern).
func WallIdle() bool {
	return CurrentPlaying() == "" && !TestShowing()
}

// PruneWebKitSandboxes removes WebKit's stale per-launch sandbox folders
// (~/.cache/.flatpak/webkit-*), which WebKit never removes itself.
func PruneWebKitSandboxes() int {
	return webcache.PruneSandboxDirs()
}
