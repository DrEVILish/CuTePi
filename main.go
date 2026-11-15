package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/unix"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/gsp/scanout"
	"CuTePi/gsp/webcache"
	"CuTePi/media"
	"CuTePi/routes"
	"CuTePi/worker"
)

// prepareTmpDir creates <working dir>/tmp, empties it of scratch files a
// crash left behind (half uploads, show-import extracts, yt-dlp dirs), and
// points TMPDIR at it so Go's multipart spooling and os.CreateTemp use the
// data disk rather than /tmp — a RAM-backed tmpfs on current Raspberry Pi
// OS, where a multi-GB upload or show import would exhaust memory.
func prepareTmpDir() {
	dir := config.TmpDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("CuTePi: creating %s: %v (falling back to system temp)", dir, err)
		return
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				log.Printf("CuTePi: clearing stale temp %s: %v", e.Name(), err)
			}
		}
	}
	if err := os.Setenv("TMPDIR", dir); err != nil {
		log.Printf("CuTePi: setting TMPDIR: %v", err)
	}
}

// appliance tunes the process for show playback (PERFORMANCE_PROPOSALS 3,
// DESIGN §7): CUTEPI_MLOCK=1 locks its memory (the presenter, the decoders'
// buffers and the Go heap never page; needs LimitMEMLOCK=infinity), and a
// Go memory limit (GOMEMLIMIT, else a quarter of RAM) keeps the heap from
// creeping over a long show.
func appliance() {
	if os.Getenv("GOMEMLIMIT") == "" {
		var si syscall.Sysinfo_t
		if syscall.Sysinfo(&si) == nil && si.Totalram > 0 {
			debug.SetMemoryLimit(int64(si.Totalram) * int64(si.Unit) / 4)
		}
	}
	if os.Getenv("CUTEPI_MLOCK") == "1" {
		if err := unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE); err != nil {
			log.Printf("CuTePi: CUTEPI_MLOCK: %v (raise LimitMEMLOCK in the unit)", err)
		} else {
			log.Printf("CuTePi: memory locked (CUTEPI_MLOCK)")
		}
	}
}

// closeInheritedFDs closes every fd from keep up that this process got from
// its parent without close-on-exec: the service's C libraries open them so
// (GStreamer's v4l2 decoders, /dev/video10), and a child that kept a
// decoder's fd kept that decoder open after its pipeline was retired, in
// itself and in the WebKit processes it starts; with it held, the Pi 4's
// H.264 decoder gave no frame to any other decoder until a restart (DESIGN
// §6.1). Go's own fds are all close-on-exec, so none of them is touched.
func closeInheritedFDs(keep int) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < keep {
			continue
		}
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil && flags&unix.FD_CLOEXEC == 0 {
			unix.Close(fd)
		}
	}
}

func main() {
	// `cutepi --console graphics|text`: the systemd unit's root steps around
	// the unprivileged service (ExecStartPre/ExecStopPost). Only this
	// console ioctl needs root; the server itself runs as the cutepi user.
	if len(os.Args) == 3 && os.Args[1] == "--console" && (os.Args[2] == "graphics" || os.Args[2] == "text") {
		if err := gsp.SetConsoleMode(os.Args[2] == "graphics"); err != nil {
			log.Fatalf("CuTePi: console %s mode: %v", os.Args[2], err)
		}
		return
	}
	// `cutepi --webcache-…`: the live-page cache keeper's WebKit work, in a
	// child of the service (gsp/webcache/child.go).
	if webcache.ChildArg(os.Args[1:]) {
		closeInheritedFDs(3)
		os.Exit(webcache.RunChild(os.Args[1:]))
	}
	// `cutepi --livepage-render …`: a live page's WebKit renderer, a child of
	// the service (gsp/scanout/remote.c).
	if scanout.ChildArg(os.Args[1:]) {
		closeInheritedFDs(4) // fd 3 is its socket to the service
		os.Exit(scanout.RunChild(os.Args[1:]))
	}
	// A child spawned by /api/restart must not bind the port until the
	// parent has exited and released it. Wait up to 20s for that.
	if os.Getenv(routes.RestartEnv) != "" {
		parent := os.Getppid()
		deadline := time.Now().Add(20 * time.Second)
		for syscall.Kill(parent, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		log.Printf("restart child: parent exited, starting up")
	}

	// Load configuration before anything that depends on it (DB path, media
	// path, etc). ctp.InitDB must run after this, not via package init(),
	// so a config-file-specified DB path is actually honored.
	routes.Version = version
	log.Printf("CuTePi %s starting", version)

	// A malformed config.json is recovered from its last good copy, or
	// stops the start: never run on a half-read configuration.
	if err := config.LoadConfig(); err != nil {
		log.Fatalf("CuTePi: %v", err)
	}
	appliance()
	prepareTmpDir()
	releaseConsole := gsp.ClaimWallConsole()
	gsp.OpenWall()

	if err := ctp.InitDB(); err != nil {
		log.Fatalf("CuTePi: failed to initialize database: %v", err)
	}
	defer ctp.CloseDB()
	gsp.AlphaLookup = ctp.MediaHasAlpha
	// A prepared copy (show compiler, DESIGN §12.17) plays instead of its
	// source: HEVC 8-bit 4:2:0, progressive.
	media.PreparedDir = filepath.Join(filepath.Dir(config.ThumbnailLocation()), "prepared")
	// The renderer's URL: the stored screen key, and for a TimerPi display
	// page this unit's fixed screen name (one TimerPi screen, every load).
	gsp.EndpointAuth = func(u string) string { return ctp.WithScreenName(ctp.WithScreenKey(u), routes.InstanceName()) }
	gsp.SourceLookup = func(f string) string { return media.Prepared(config.MediaLocation(), f) }
	gsp.FieldOrderLookup = func(f string) string {
		if gsp.SourceLookup(f) != "" {
			return ""
		}
		return ctp.MediaFieldOrder(f)
	}
	gsp.CodecLookup = func(f string) string {
		if gsp.SourceLookup(f) != "" {
			return "hevc/yuv420p"
		}
		return ctp.MediaVideoCodec(f)
	}
	media.PlaybackBusy = func() bool { return !gsp.WallIdle() || len(gsp.RunningCues()) > 0 }

	// One-time scan flags media whose source files are missing on disk; cues
	// and pool tiles then surface a warning (see the Missing flag).
	ctp.MarkMissingFiles()
	// Repair dangling group references left by older builds (parents
	// pointing at deleted groups, missing indices). Membership itself is
	// never judged — only broken references.
	if err := ctp.HealSheet(); err != nil {
		log.Printf("CuTePi: sheet heal: %v", err)
	}
	// Live-page sources no cue uses any more (deleted cues, overwritten shows).
	if err := ctp.PruneEndpoints(); err != nil {
		log.Printf("CuTePi: pruning live-page sources: %v", err)
	}

	go worker.RunThumbnailWorker(2 * time.Second)
	go routes.RunScheduler()
	// Live-page asset cache (§12.14): preloads live cues' pages while the
	// wall is idle; drops the cached assets and source rows of removed cues
	// (also the ones left from before a restart).
	go routes.RunLiveCacheKeeper()
	go routes.RunLiveStandby()
	// Panic holding image kept armed on the wall, so a panic cuts to it at
	// once (§12.9).
	go routes.RunPanicStandby()
	// Remote control (§12.8): HyperDeck / OSC UDP / OSC TCP listeners, each
	// only when enabled in Settings > Network (all off by default).
	routes.StartRemote()
	// Linked units (§6.8c): pairing, mDNS, the peer poll and replication.
	go routes.RunLink()

	r := gin.Default()
	r.SetTrustedProxies(nil)
	// Cross-site guard, operator password, cache policy and body caps, in
	// that order (routes.UseMiddleware).
	routes.UseMiddleware(r)

	// Single template function map (routes.TemplateFuncs): server renders and
	// tests parse the same templates, so the map must be identical in both.
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(routes.TemplateFuncs()).ParseGlob("templates/*")))

	index := r.Group("/")
	api := r.Group("/api")
	upload := r.Group("/upload")
	youtube := r.Group("/youtube")

	{
		routes.Public(r)
		routes.Index(index)
		routes.Api(api)
		routes.Groups(api)
		routes.Show(api)
		routes.Logs(api)
		routes.Upload(upload)
		routes.Youtube(youtube)
	}

	// Start the server. An explicit http.Server so the SIGTERM handler can
	// drain in-flight requests (srv.Shutdown) instead of killing them.
	address := fmt.Sprintf(":%d", config.Port())
	srv := &http.Server{Addr: address, Handler: r}

	// Graceful shutdown on SIGTERM/SIGINT (also what /api/shutdown triggers).
	// Drain order matters: stop accepting requests and let in-flight
	// handlers finish (uploads, show imports - all of which write), THEN
	// tear down the playback pipeline, THEN close the DB. The old handler
	// closed the DB first, killing the worker's and any in-flight handler's
	// transactions with 'database is closed'.
	shutdownDone := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("CuTePi: shutting down (signal received)")
		drainServer(srv, func() {
			gsp.Panic()
			releaseConsole()
			ctp.CloseDB()
		})
		close(shutdownDone)
	}()

	printNetworkInfo()
	log.Printf("CuTePi %s: listening on %s", version, address)
	// A bind failure (port already in use - e.g. the restart handover losing
	// the race, or a second instance) must NOT look like a clean exit: the
	// old `r.Run(address)` ignoring the error exited 0 and systemd restarted
	// a server that had never listened.
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("CuTePi: server failed on %s: %v", address, err)
	}
	<-shutdownDone
}

// drainServer waits for HTTP handlers before tearing down shared resources.
func drainServer(srv *http.Server, cleanup func()) {
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("CuTePi: HTTP shutdown: %v", err)
	}
	cleanup()
}
