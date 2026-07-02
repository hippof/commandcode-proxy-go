// Command commandcode-proxy serves the OpenAI- and Anthropic-compatible proxy
// over Command Code's API. Keyless: every request carries the caller's own key.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/config"
	"github.com/liwei/commandcode-proxy-go/internal/server"
)

// version is set via -ldflags "-X main.version=..." at build time.
var version = "dev"

func main() {
	cfg := config.Load()
	srv := server.New(cfg)
	srv.Version = version

	addr := env("COMMANDCODE_PROXY_HOST", "127.0.0.1") + ":" + env("COMMANDCODE_PROXY_PORT", "8787")
	if cfg.LogEnabled("info") {
		log.Printf("commandcode-proxy %s listening on http://%s", version, addr)
	}
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// Slowloris guard for non-localhost binds. Only the header read is
		// bounded — no Read/WriteTimeout, so long SSE streams aren't cut.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// On SIGINT/SIGTERM, stop accepting and let in-flight requests (including
	// SSE streams) finish, up to a grace period before forcing the close.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	if cfg.LogEnabled("info") {
		log.Printf("shutting down (waiting up to 30s for in-flight requests)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown forced: %v", err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
