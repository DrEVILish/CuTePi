package routes

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// Live-page asset cache keeper (DESIGN §12.14). WebKit's own HTTP cache
// holds a page's .js/.css/images under the server's cache rules; this
// keeper fills it ahead of time and empties it when cues go:
//
//   - each live cue's page is preloaded once per run, off screen and only
//     while the wall is idle, so a later fire loads from disk;
//   - a source row no cue uses any more (cue deleted, URL changed, show
//     cleared or replaced) has its site's cached assets removed, unless a
//     remaining live cue uses the same site, and the row is then deleted.
const liveCacheTick = 5 * time.Second

// livePreloadRetry is how long a page that failed to preload waits before
// the next try (offline server, bad certificate, ...).
const livePreloadRetry = time.Minute

// sandboxSweep is how often WebKit's leftover sandbox folders (one per web
// process launch, never removed by WebKit) are cleared: at start, then this
// often.
const sandboxSweep = time.Minute

var (
	liveCacheMu sync.Mutex
	// preloaded: source id -> URL preloaded this run; failed: id -> next try.
	preloaded = map[int]string{}
	preloadAt = map[int]time.Time{}
)

// RunLiveCacheKeeper runs the keeper forever (main starts it).
func RunLiveCacheKeeper() {
	caching := gsp.PageCacheAvailable()
	if !caching {
		logs.Printf(logs.GSPPipeDebug, "live-page cache: WebKit not available, cache keeping off")
	}
	var lastSweep time.Time
	for {
		keepLiveCache(caching)
		if time.Since(lastSweep) >= sandboxSweep {
			if n := gsp.PruneWebKitSandboxes(); n > 0 {
				logs.Printf(logs.GSPPipeDebug, "live-page cache: removed %d stale WebKit sandbox folders", n)
			}
			lastSweep = time.Now()
		}
		time.Sleep(liveCacheTick)
	}
}

// keepLiveCache runs one keeper pass.
func keepLiveCache(caching bool) {
	orphans, err := ctp.OrphanEndpoints()
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "live-page cache: %v", err)
		return
	}
	live, err := ctp.LiveSources()
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "live-page cache: %v", err)
		return
	}
	if len(orphans) > 0 {
		dropOrphanSources(caching, orphans, live)
	}
	if caching {
		preloadNext(live)
	}
}

// dropOrphanSources clears the cached assets of the orphans' sites that no
// live cue still uses, then deletes the orphan rows.
func dropOrphanSources(caching bool, orphans, live []ctp.LiveSource) {
	remove, keep := sourceHosts(orphans), sourceHosts(live)
	if caching && len(remove) > 0 {
		removed, err := gsp.ClearPageCaches(remove, keep)
		if err != nil {
			// Not retried: the rows go anyway, and WebKit evicts on its own.
			logs.Printf(logs.GSPPipeDebug, "live-page cache: clearing %v: %v", remove, err)
		} else if len(removed) > 0 {
			logs.Printf(logs.GSPPipeDebug, "live-page cache: removed cached assets of %s", strings.Join(removed, ", "))
		}
	}
	liveCacheMu.Lock()
	for _, o := range orphans {
		delete(preloaded, o.MediaID)
		delete(preloadAt, o.MediaID)
	}
	liveCacheMu.Unlock()
	for _, o := range orphans {
		if err := ctp.DeleteOrphanEndpoint(o.MediaID); err != nil {
			logs.Printf(logs.GSPPipeDebug, "live-page cache: deleting source %d: %v", o.MediaID, err)
		}
	}
}

// preloadNext preloads the first live cue page not preloaded this run, if
// the wall is idle. One page per pass: each takes ~2 s and ~1.5 cores.
func preloadNext(live []ctp.LiveSource) {
	if !gsp.WallIdle() {
		return
	}
	now := time.Now()
	liveCacheMu.Lock()
	var next *ctp.LiveSource
	for i, s := range live {
		if preloaded[s.MediaID] == s.URL || now.Before(preloadAt[s.MediaID]) {
			continue
		}
		next = &live[i]
		break
	}
	liveCacheMu.Unlock()
	if next == nil {
		return
	}
	start := time.Now()
	err := gsp.PreloadPage(next.URL, gsp.WallIdle)
	liveCacheMu.Lock()
	defer liveCacheMu.Unlock()
	switch {
	case err == nil:
		preloaded[next.MediaID] = next.URL
		delete(preloadAt, next.MediaID)
		logs.Printf(logs.GSPPipeDebug, "live-page cache: preloaded %s in %.1fs", next.URL, time.Since(start).Seconds())
	case errors.Is(err, gsp.ErrPreloadStopped):
		// The wall got busy: try again on a later idle pass.
	default:
		preloadAt[next.MediaID] = time.Now().Add(livePreloadRetry)
		logs.Printf(logs.GSPPipeDebug, "live-page cache: preloading %s: %v (retry in %v)", next.URL, err, livePreloadRetry)
	}
}

// sourceHosts lists the distinct hosts of sources' URLs, sorted.
func sourceHosts(sources []ctp.LiveSource) []string {
	set := map[string]bool{}
	for _, s := range sources {
		if u, err := url.Parse(s.URL); err == nil && u.Hostname() != "" {
			set[strings.ToLower(u.Hostname())] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
