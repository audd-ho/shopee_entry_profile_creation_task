// tcp-server is the business-logic backend binary.
// It speaks the custom JSON-lines TCP protocol, talks to MySQL and Redis,
// and manages the filesystem for avatar storage.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/tcpserver"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(cfg.MySQL.DSN, cfg.MySQL.MaxOpen, cfg.MySQL.MaxWait)
	if err != nil {
		log.Fatalf("mysql pool: %v", err)
	}
	defer pool.Close()

	rcache, err := cache.New(&cfg.Redis)
	if err != nil {
		log.Fatalf("redis client: %v", err)
	}
	defer rcache.Close()

	srv := tcpserver.New(cfg, pool, rcache)
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("tcp-server: %v", err)
	}
}
