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
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
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
	// A malformed config.json is recovered from its last good copy, or
	// stops the start: never run on a half-read configuration.
	if err := config.LoadConfig(); err != nil {
		log.Fatalf("CuTePi: %v", err)
	}
	prepareTmpDir()
	releaseConsole := gsp.ClaimWallConsole()
	gsp.OpenWall()

	if err := ctp.InitDB(); err != nil {
		log.Fatalf("CuTePi: failed to initialize database: %v", err)
	}
	defer ctp.CloseDB()
	gsp.AlphaLookup = ctp.MediaHasAlpha

	// One-time scan flags media whose source files are missing on disk; cues
	// and pool tiles then surface a warning (see the Missing flag).
	ctp.MarkMissingFiles()
	// Repair dangling group references left by older builds (parents
	// pointing at deleted groups, missing indices). Membership itself is
	// never judged — only broken references.
	if err := ctp.HealSheet(); err != nil {
		log.Printf("CuTePi: sheet heal: %v", err)
	}

	go worker.RunThumbnailWorker(2 * time.Second)
	go routes.RunScheduler()
	// Live-page asset cache (§12.14): preloads live cues' pages while the
	// wall is idle; drops the cached assets and source rows of removed cues
	// (also the ones left from before a restart).
	go routes.RunLiveCacheKeeper()
	// Panic holding image kept armed on the wall, so a panic cuts to it at
	// once (§12.9).
	go routes.RunPanicStandby()
	// Remote control (§12.8): HyperDeck / OSC UDP / OSC TCP listeners, each
	// only when enabled in Settings > Network (all off by default).
	routes.StartRemote()

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
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("CuTePi: shutting down (signal received)")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("CuTePi: HTTP shutdown: %v", err)
		}
		gsp.Panic() // tear down the playback pipeline (stops the show cleanly)
		releaseConsole()
		ctp.CloseDB()
		os.Exit(0)
	}()

	printNetworkInfo()
	log.Printf("CuTePi: listening on %s", address)
	// A bind failure (port already in use - e.g. the restart handover losing
	// the race, or a second instance) must NOT look like a clean exit: the
	// old `r.Run(address)` ignoring the error exited 0 and systemd restarted
	// a server that had never listened.
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("CuTePi: server failed on %s: %v", address, err)
	}
}
