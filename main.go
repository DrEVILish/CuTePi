package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/routes"
	"CuTePi/worker"
)

func main() {
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
	config.LoadConfig()

	if err := ctp.InitDB(); err != nil {
		log.Fatalf("CuTePi: failed to initialize database: %v", err)
	}
	defer ctp.CloseDB()

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

	r := gin.Default()
	r.SetTrustedProxies(nil)
	// Optional operator password (config.json auth_password / Settings).
	// Applies to every route group below, including the WebSocket handshake.
	r.Use(routes.AuthMiddleware())
	// Client cache policy: no-store everywhere except images.
	r.Use(routes.CachePolicy())
	// Cap request bodies: one giant POST must not fill the disk or OOM the
	// in-memory .CTP parse.
	r.Use(routes.LimitBody())

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
