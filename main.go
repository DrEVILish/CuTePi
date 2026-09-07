package main

import (
	"fmt"
	"html/template"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		ctp.CloseDB()
		os.Exit(0)
	}()

	go worker.RunThumbnailWorker(2 * time.Second)

	r := gin.Default()
	r.SetTrustedProxies(nil)
	// Optional operator password (config.json auth_password / Settings).
	// Applies to every route group below, including the WebSocket handshake.
	r.Use(routes.AuthMiddleware())

	extendedFuncs := map[string]any{
		"contains":    strings.Contains,
		"hasPrefix":   strings.HasPrefix,
		"hasSuffix":   strings.HasSuffix,
		"formatTime":  ctp.FormatTime,
		"urlPath":     url.PathEscape,
		"typeIcon":    routes.TypeIcon,
		"displayTime": routes.DisplayTime,
		"progressPct": routes.ProgressPct,
		"div":         func(a, b int) int { return a / b },
	}

	// Load templates with FunctionMap
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(extendedFuncs).ParseGlob("templates/*")))

	index := r.Group("/")
	api := r.Group("/api")
	install := r.Group("/install")
	upload := r.Group("/upload")
	youtube := r.Group("/youtube")

	{
		routes.Public(r)
		routes.Index(index)
		routes.Api(api)
		routes.Groups(api)
		routes.Show(api)
		routes.Logs(api)
		routes.Install(install)
		routes.Upload(upload)
		routes.Youtube(youtube)
	}

	// Start the server
	address := fmt.Sprintf(":%d", config.Port())
	printNetworkInfo()
	r.Run(address)

}
