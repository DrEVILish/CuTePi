package gsp

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/gsp/scanout"
	"CuTePi/logs"
)

// Live-page standby (DESIGN §12.14): live cues' pages are built before their
// GO. Its renderer child starts at once, so WebKit launches, loads
// and lays the page out off screen while the cue waits; the frames it paints
// are dropped at the cue pipeline's appsrc, which stays stopped. GO plays
// that pipeline, so the page is on screen within a few frames instead of
// after WebKit's start-up and the page load (1.4 s for a TimerPi page and
// 4.2-4.5 s for a news site, measured cold on the Pi 4 with its assets
// already in WebKit's disk cache). Up to liveStandbyMax pages (every live cue
// of the sheet, the selected cue's and the next cue's first); only on the
// KMS wall, where pages render out of process (gsp/scanout/remote.c).

// liveSrcName names the appsrc a remote-rendered live page feeds.
const liveSrcName = "livepage-src"

// liveRemote reports whether a live page with opts renders out of process:
// on the KMS wall with the display controller doing the rotation, unless
// CUTEPI_LIVEPAGE_INPROC=1.
func liveRemote(opts LoadOpts) bool {
	_, hwRotate := rotationBits(opts.Rotation, opts.Flip)
	return kmsWall() != nil && hwRotate && os.Getenv("CUTEPI_LIVEPAGE_INPROC") != "1" && gst.Find("wpevideosrc") != nil
}

type liveStandbyKey struct {
	title, url string
	opts       LoadOpts
}

// liveStandbyMax is how many pages stand by at once: every live cue of the
// sheet up to this many, the selected and next cues' first. A standing page
// costs ~0.4-0.5 GB RSS (Pi 4, 2026-11-12: renderer child 133 MB, WebKit
// network process 70 MB, web process 180 MB for a TimerPi page and 300 MB
// for bbc.co.uk), so the pool is sized by the machine's memory: 3 on a 4 GB
// Pi, 1 on 2 GB and less (liveStandbyCap).
var liveStandbyMax = liveStandbyCap(memTotalKB())

// liveStandbyCap is the pool size for memKB of RAM: what is left after
// 1.5 GB (the service and the system, the service has reached 1.2 GB) at
// 512 MB a page, 1 to 3.
func liveStandbyCap(memKB int) int {
	return max(1, min(3, (memKB/1024-1536)/512))
}

// memTotalKB is /proc/meminfo's MemTotal (0 when unreadable).
func memTotalKB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

// liveStandbyIdle ends a standby page nobody has wanted for this long (a
// WebKit renderer holds 100s of MB and some CPU).
const liveStandbyIdle = 2 * time.Minute

type standbyPage struct {
	p      *gst.Pipeline
	key    liveStandbyKey
	wanted time.Time // last time the keeper asked for it
	built  time.Time // when its renderer started
	paced  bool      // its renderer takes liveStandbyFPS (pacePage)
}

// A page standing by liveStandbyPaceAfter (loaded and painted: a news site
// took 4.2-4.5 s cold) is paced to liveStandbyFPS: WebKit paints only the
// frames taken, so a page with animations stops costing a core or two while
// it waits (its frames were dropped at the appsrc anyway). GO resumes every
// frame (takeStandbyLive) and shows the first one painted after it.
const (
	liveStandbyPaceAfter = 5 * time.Second
	liveStandbyFPS       = 2
)

// pacePage sets p's renderer to fps frames a second (0: all).
func pacePage(p *gst.Pipeline, fps int) {
	if src, _ := p.GetElementByName(liveSrcName); src != nil {
		scanout.PaceRemote(src, fps)
	}
}

var liveStandby struct {
	mu    sync.Mutex
	pages []*standbyPage
}

// liveOpts strips the options that do not apply to a live page, as every
// live load does: a standby build and its GO then compare equal.
func liveOpts(opts LoadOpts) LoadOpts {
	opts.LiveEndpoint = true
	opts.InPoint, opts.OutPoint, opts.Rate = 0, 0, 1
	opts.Hold, opts.Loop, opts.LoopCount = false, false, 0
	opts.WarmPreroll = false
	return opts
}

// LiveStandbyWant is a live cue's page the keeper wants ready for a GO.
type LiveStandbyWant struct {
	Title, URL string
	Opts       LoadOpts
}

// StandbyLive keeps the wanted pages loaded off screen, most important
// first. Pages wanted before stay until the pool needs their place or nobody
// has wanted them for liveStandbyIdle. With build false nothing new starts
// (right after a fire, when WebKit's start-up would compete with the cue);
// the wanted pages are only kept. Where pages do not render out of process
// nothing stands by.
func StandbyLive(wants []LiveStandbyWant, build bool) error {
	gstInit()
	now := time.Now()
	keys := make([]liveStandbyKey, 0, len(wants))
	for _, w := range wants {
		if o := liveOpts(w.Opts); liveRemote(o) {
			keys = append(keys, liveStandbyKey{w.Title, w.URL, o})
			if len(keys) == liveStandbyMax {
				break
			}
		}
	}
	var retire []*gst.Pipeline
	var missing []liveStandbyKey
	liveStandby.mu.Lock()
	kept := liveStandby.pages[:0]
	for _, pg := range liveStandby.pages {
		for _, k := range keys {
			if pg.key == k {
				pg.wanted = now
			}
		}
		if now.Sub(pg.wanted) > liveStandbyIdle {
			retire = append(retire, pg.p)
			continue
		}
		if !pg.paced && now.Sub(pg.built) >= liveStandbyPaceAfter {
			pg.paced = true
			pacePage(pg.p, liveStandbyFPS)
		}
		kept = append(kept, pg)
	}
	liveStandby.pages = kept
	for _, k := range keys {
		if findStandbyLocked(k) == nil {
			missing = append(missing, k)
		}
	}
	liveStandby.mu.Unlock()
	retireAll(retire...)
	if !build {
		return nil
	}
	var firstErr error
	for _, k := range missing {
		if err := buildStandby(k, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func findStandbyLocked(k liveStandbyKey) *standbyPage {
	for _, pg := range liveStandby.pages {
		if pg.key == k {
			return pg
		}
	}
	return nil
}

// buildStandby starts k's page, first making room by ending the page wanted
// longest ago if the pool is full (never one wanted in this same pass).
func buildStandby(k liveStandbyKey, now time.Time) error {
	liveStandby.mu.Lock()
	var evict *gst.Pipeline
	if len(liveStandby.pages) >= liveStandbyMax {
		oldest := -1
		for i, pg := range liveStandby.pages {
			if pg.wanted.Before(now) && (oldest < 0 || pg.wanted.Before(liveStandby.pages[oldest].wanted)) {
				oldest = i
			}
		}
		if oldest < 0 {
			liveStandby.mu.Unlock()
			return nil // every page is wanted now: the less important ones wait
		}
		evict = liveStandby.pages[oldest].p
		liveStandby.pages = append(liveStandby.pages[:oldest], liveStandby.pages[oldest+1:]...)
	}
	liveStandby.mu.Unlock()
	retireAll(evict)
	start := time.Now()
	p, err := buildPipeline(pipelineSpec{endpointURL: k.url, liveTitle: k.title, liveStandby: true, opts: k.opts})
	if err != nil {
		return err
	}
	if src, _ := p.GetElementByName(liveSrcName); src == nil {
		retirePipeline(p) // in-process renderer: it only loads once playing
		return nil
	}
	liveStandby.mu.Lock()
	if findStandbyLocked(k) != nil { // a concurrent pass built it too
		liveStandby.mu.Unlock()
		retirePipeline(p)
		return nil
	}
	liveStandby.pages = append(liveStandby.pages, &standbyPage{p: p, key: k, wanted: now, built: start})
	n := len(liveStandby.pages)
	liveStandby.mu.Unlock()
	logs.Printf(logs.GSPWarm, "live page standing by: %s (renderer started in %.0fms, %d standing by)", k.title, time.Since(start).Seconds()*1000, n)
	return nil
}

// DropStandbyLive ends every standby page.
func DropStandbyLive() {
	liveStandby.mu.Lock()
	pages := liveStandby.pages
	liveStandby.pages = nil
	liveStandby.mu.Unlock()
	for _, pg := range pages {
		retirePipeline(pg.p)
	}
}

// StandbyLiveCount is how many pages stand by.
func StandbyLiveCount() int {
	liveStandby.mu.Lock()
	defer liveStandby.mu.Unlock()
	return len(liveStandby.pages)
}

// takeStandbyLive claims the standby pipeline for a GO of the same page with
// the same options (opts already through liveOpts), or returns nil. Other
// standby pages stay (the next cue's is still wanted).
// A renderer that failed while standing by is not used. A page that finished
// loading meanwhile is marked loaded long enough ago that showLive raises it
// with its first frame.
func takeStandbyLive(title, url string, opts LoadOpts) *gst.Pipeline {
	k := liveStandbyKey{title, url, opts}
	liveStandby.mu.Lock()
	var p *gst.Pipeline
	var built time.Time
	paced := false
	for i, pg := range liveStandby.pages {
		if pg.key == k {
			p, built, paced = pg.p, pg.built, pg.paced
			liveStandby.pages = append(liveStandby.pages[:i], liveStandby.pages[i+1:]...)
			break
		}
	}
	liveStandby.mu.Unlock()
	if p == nil {
		return nil
	}
	if paced {
		pacePage(p, 0) // first thing: WebKit paints afresh while the GO swaps
	}
	loaded := false
	bus := p.GetPipelineBus()
	for msg := bus.Pop(); msg != nil; msg = bus.Pop() {
		switch msg.Type() {
		case gst.MessageError:
			logs.Printf(logs.GSPWarm, "live page standby failed (%s): cold load", msg.ParseError().Error())
			retirePipeline(p)
			return nil
		case gst.MessageElement:
			if st := msg.GetStructure(); st != nil && st.Name() == "wpe-stats" {
				if v, err := st.GetValue("estimated-load-progress"); err == nil {
					if f, ok := v.(float64); ok && f >= 100 {
						loaded = true
					}
				}
			}
		}
	}
	if loaded {
		liveLoaded.Store(p, time.Now().Add(-livePaintSettle))
	}
	liveFromStandby.Store(p, built) // shown within liveStandbyShowWait of GO, at once if long built
	return p
}

// stopLiveRenderer ends p's out-of-process page renderer, which a pipeline
// retired before it ever played would otherwise keep running.
func stopLiveRenderer(p *gst.Pipeline) {
	if src, _ := p.GetElementByName(liveSrcName); src != nil {
		scanout.StopRemote(src)
	}
}
