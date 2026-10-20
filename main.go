// acp-proxy is a web server exposing an OpenAI-compatible API that routes
// requests to ACP (Agent Client Protocol) agents. Zero-config: running it
// with no arguments starts a server on 127.0.0.1:8080; every option is
// available as a flag and as an ACP_PROXY_* environment variable.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"acp-proxy/internal/config"
	"acp-proxy/internal/openai"
)

func main() {
	opts, err := config.Parse(os.Args[1:], os.Getenv)
	if err != nil {
		log.Fatalf("acp-proxy: %v", err)
	}

	models := make([]string, 0, len(opts.Agents))
	for _, a := range opts.Agents {
		models = append(models, a.Model)
	}
	if len(models) == 0 {
		log.Printf("acp-proxy: no agents configured; configure one with -agent \"[name=]command [args...]\" or %sAGENTS", "ACP_PROXY_")
	} else {
		log.Printf("acp-proxy: models: %s", strings.Join(models, ", "))
	}
	if opts.AutoPermissions {
		log.Print("acp-proxy: permission requests are auto-approved (-auto)")
	}

	mux := http.NewServeMux()
	openai.New(opts).Register(mux)
	addr := fmt.Sprintf("%s:%d", opts.Host, opts.Port)
	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("acp-proxy: listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("acp-proxy: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("acp-proxy: shutting down")
	if err := server.Shutdown(context.Background()); err != nil {
		log.Printf("acp-proxy: shutdown: %v", err)
	}
}
