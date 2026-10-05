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
	"time"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/idgen"
	"entry_profile_creation/internal/lockstack"
	"entry_profile_creation/internal/tcpserver"
)

func main() {
	cfg := config.Load()
	if err := cfg.CheckRequestTimeouts(); err != nil {
		log.Fatalf("tcp-server: %v", err)
	}
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
		LockTTL:            cfg.LockStack.LockTTL,
		WatchdogTTL:        cfg.LockStack.WatchdogTTL,
		MinWriteBudget:     cfg.LockStack.MinWriteBudget,
		MaxDeadlockRetries: cfg.LockStack.MaxDeadlockRetries,
		MaxLockedWait:      cfg.LockStack.MaxLockedWait,
		CleanupTimeout:     cfg.LockStack.CleanupTimeout,
		LeaseTTL:           cfg.LockStack.LeaseTTL,
		CacheTTL:           cfg.LockStack.CacheTTL,
		PollMin:            cfg.LockStack.PollMin,
		PollMax:            cfg.LockStack.PollMax,
		MaxLockRetries:     cfg.LockStack.MaxLockRetries,
		Disabled:           redis.UserCacheDisabled(),
	})
	if err != nil {
		log.Fatalf("tcp-server: lockstack: %v", err)
	}

	server := tcpserver.NewServer(cfg, dbh, redis, ls, gen)
	checkWriteTimeouts(server.RequestTimeout(), ls.Config(), db.EffectiveLockWaitTimeout(cfg.MySQLDataPool.LockWaitTimeout))
	if err := server.Run(ctx); err != nil {
		log.Fatalf("tcp-server: %v", err)
	}
}

// checkWriteTimeouts refuses to start with lock-stack timeouts that don't nest:
//
//	MySQL lock-wait < MinWriteBudget < min(request timeout, WatchdogTTL)
//
// A lock-wait at or above MinWriteBudget lets a statement queued in MySQL
// commit after the write's lease expired (stale cache, nobody to repair it);
// a MinWriteBudget at or above the write deadline makes every write fail.
func checkWriteTimeouts(requestTimeout time.Duration, ls lockstack.Config, mysqlLockWait time.Duration) {
	if ls.Disabled {
		return
	}
	deadline := min(requestTimeout, ls.WatchdogTTL)
	if mysqlLockWait >= ls.MinWriteBudget {
		log.Fatalf("tcp-server: MySQL lock-wait timeout %v must be below LOCKSTACK_MIN_WRITE_BUDGET_MS (%v)", mysqlLockWait, ls.MinWriteBudget)
	}
	if ls.MinWriteBudget >= deadline {
		log.Fatalf("tcp-server: LOCKSTACK_MIN_WRITE_BUDGET_MS (%v) must be below the write deadline min(TCP_REQUEST_TIMEOUT_MS, LOCKSTACK_WATCHDOG_TTL_MS) = %v", ls.MinWriteBudget, deadline)
	}
}
