// Command commandcode-proxy serves the OpenAI- and Anthropic-compatible proxy
// over Command Code's API. Keyless: every request carries the caller's own key.
package main

import (
	"log"
	"net/http"
	"os"

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
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
