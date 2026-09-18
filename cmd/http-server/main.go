// http-server is the HTTP frontend binary.
// It serves the web UI and relays API requests to the TCP backend.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/httpserver"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := httpserver.New(cfg)
	if err != nil {
		log.Fatalf("http-server init: %v", err)
	}
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("http-server run: %v", err)
	}
}
