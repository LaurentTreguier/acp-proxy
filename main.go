// acp-proxy is a web server exposing an OpenAI-compatible API that routes
// requests to ACP (Agent Client Protocol) agents. Zero-config: running it
// with no arguments starts a server on 127.0.0.1:11734; agents are resolved
// on demand from the ACP registry by id, via the /{agent}/v1/... URL
// prefix. Every option is available as a flag and as an ACP_PROXY_*
// environment variable.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"acp-proxy/internal/acp"
	"acp-proxy/internal/anthropic"
	"acp-proxy/internal/config"
	"acp-proxy/internal/openai"
	"acp-proxy/internal/registry"
)

func main() {
	opts, err := config.Parse(os.Args[1:], os.Getenv)
	if err != nil {
		log.Fatalf("acp-proxy: %v", err)
	}

	static := make(map[string]acp.Agent, len(opts.Agents))
	for _, a := range opts.Agents {
		static[a.Model] = acp.Agent{
			ID:          a.Model,
			Command:     a.Command,
			Args:        a.Args,
			Dir:         opts.Dir,
			Permissions: opts.PermissionPolicy(),
		}
	}
	resolver := registry.New(opts.RegistryURL, opts.CacheDir, opts.Dir, opts.PermissionPolicy(), static)

	// Fetch the registry at startup; failures are non-fatal, unknown ids
	// trigger a refetch on demand.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := resolver.Warmup(ctx); err != nil {
			log.Printf("acp-proxy: registry unavailable: %v (will retry on demand)", err)
			return
		}
		agents, err := resolver.List(ctx)
		if err != nil {
			return
		}
		log.Printf("acp-proxy: registry: %d agent(s) available", len(agents))
	}()

	if opts.AutoPermissions {
		log.Print("acp-proxy: permission requests are auto-approved (-auto)")
	}

	mux := http.NewServeMux()
	pool := acp.NewPool()
	defer pool.Close()
	openai.New(resolver, pool).Register(mux)
	anthropic.New(resolver, pool).Register(mux)
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
