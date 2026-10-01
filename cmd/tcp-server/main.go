// TODO: Entry point for the TCP server binary (business logic backend).
//
// Responsibilities:
//   - Load config (listen port, MySQL DSN, Redis addr, etc.) via internal/config
//   - Initialize shard pools (data + lookup) and the consistent hash ring
//   - Initialize the snowflake ID generator
//   - Initialize the Redis client (internal/cache)
//   - Start the internal/tcpserver accept loop
//   - Handle SIGINT/SIGTERM for graceful shutdown (drain in-flight requests, close pools)
//
// Build:  go build ./cmd/tcp-server
// Run:    ./tcp-server (or `go run ./cmd/tcp-server`)
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/idgen"
	"entry_profile_creation/internal/lockstack"
	"entry_profile_creation/internal/tcpserver"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Snowflake ID generator ---
	gen, err := idgen.New(cfg.IDGen.MachineID)
	if err != nil {
		log.Fatalf("tcp-server: idgen: %v", err)
	}

	// --- Open data shard pools ---
	dataShards := make([]*db.Pool, len(cfg.Shard.DataShardDSNs))
	for i, dsn := range cfg.Shard.DataShardDSNs {
		pool, err := db.Open(ctx, dsn, &cfg.MySQLDataPool)
		if err != nil {
			log.Fatalf("tcp-server: data shard %d: %v", i, err)
		}
		defer pool.Close()
		dataShards[i] = pool
		log.Printf("tcp-server: data shard %d connected", i)
	}

	// --- Open lookup shard pools ---
	lookupShards := make([]*db.Pool, len(cfg.Shard.LookupShardDSNs))
	for i, dsn := range cfg.Shard.LookupShardDSNs {
		pool, err := db.Open(ctx, dsn, &cfg.MySQLLookupPool)
		if err != nil {
			log.Fatalf("tcp-server: lookup shard %d: %v", i, err)
		}
		defer pool.Close()
		lookupShards[i] = pool
		log.Printf("tcp-server: lookup shard %d connected", i)
	}

	// --- Consistent hash ring for data shards ---
	dbh := db.NewShardedHandle(dataShards, lookupShards, cfg.Shard.VnodesPerShard)

	// --- Redis ---
	redis, err := cache.New(&cfg.Redis, cfg.Session.TTL)
	if err != nil {
		log.Fatalf("tcp-server: redis: %v", err)
	}
	defer redis.Close()

	// --- Lock stack ---
	ls, err := lockstack.New(redis.RDB(), lockstack.Config{
		LockTTL:        cfg.LockStack.LockTTL,
		LeaseTTL:       cfg.LockStack.LeaseTTL,
		CacheTTL:       cfg.LockStack.CacheTTL,
		PollMin:        cfg.LockStack.PollMin,
		PollMax:        cfg.LockStack.PollMax,
		MaxLockRetries: cfg.LockStack.MaxLockRetries,
		Disabled:       redis.UserCacheDisabled(),
	})
	if err != nil {
		log.Fatalf("tcp-server: lockstack: %v", err)
	}

	server := tcpserver.NewServer(cfg, dbh, redis, ls, gen)
	if err := server.Run(ctx); err != nil {
		log.Fatalf("tcp-server: %v", err)
	}
}
