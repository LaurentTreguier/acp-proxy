// acp-proxy is a web server exposing an OpenAI-compatible API that routes
// requests to ACP (Agent Client Protocol) agents.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"acp-proxy/internal/config"
	"acp-proxy/internal/openai"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	addr := flag.String("addr", "", "listen address, overriding the configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("acp-proxy: %v", err)
	}
	if *addr != "" {
		cfg.Addr = *addr
	}

	mux := http.NewServeMux()
	openai.New(cfg).Register(mux)
	server := &http.Server{Addr: cfg.Addr, Handler: mux}

	go func() {
		log.Printf("acp-proxy: listening on %s (%d model(s))", cfg.Addr, len(cfg.Agents))
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
