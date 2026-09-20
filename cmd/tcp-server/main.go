// TODO: Entry point for the TCP server binary (business logic backend).
//
// Responsibilities:
//   - Load config (listen port, MySQL DSN, Redis addr, etc.) via internal/config
//   - Initialize the custom MySQL pool (internal/db)
//   - Initialize the Redis client (internal/cache)
//   - Start the internal/tcpserver accept loop
//   - Handle SIGINT/SIGTERM for graceful shutdown (drain in-flight requests, close pools)
//
// Build:  go build ./cmd/tcp-server
// Run:    ./tcp-server (or `go run ./cmd/tcp-server`)
package main

import (
	"context"
	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/tcpserver"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	cfg := config.Load()
	// this server context part can learn and read about more
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Both of these ping on construction, so a bad DSN or a down Redis fails
	// here at startup rather than on the first user request.
	dbHandle, err := db.Open(ctx, &cfg.MySQL)
	if err != nil {
		log.Fatalf("tcp-server: mysql: %v", err)
	}
	defer dbHandle.Close()

	redis, err := cache.New(&cfg.Redis, cfg.Session.TTL)
	if err != nil {
		log.Fatalf("tcp-server: redis: %v", err)
	}
	defer redis.Close()

	server := tcpserver.NewServer(cfg, dbHandle, redis)
	if err := server.Run(ctx); err != nil {
		log.Fatalf("tcp-server: %v", err)
	}
}
