// TODO: Entry point for the HTTP server binary.
//
// Responsibilities:
//   - Load config (port, TCP-server address, etc.) via internal/config
//   - Initialize the internal/httpserver package
//   - Handle SIGINT/SIGTERM for graceful shutdown
//   - Keep the main goroutine simple; all real logic lives in internal/httpserver
//
// Build:  go build ./cmd/http-server
// Run:    ./http-server (or `go run ./cmd/http-server`)
package main

import (
	"context"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/httpserver"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	// TODO: implement

	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server, err := httpserver.NewServer(cfg)
	if err != nil {
		log.Fatalf("http-server init: %v", err)
	}
	if err := server.Run(ctx); err != nil {
		log.Fatalf("http-server run: %v", err)
	}
}
