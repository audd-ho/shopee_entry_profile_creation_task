// TODO: Redis client wrapper (see §4.6.1).
//
// Uses redis/go-redis/v9's built-in pool (not custom).
//
// Responsibilities:
//   - Construct *redis.Client with configured addr, password, DB, pool settings
//   - Expose typed helpers for our key patterns (session.go, user.go)
//   - Wrap client so tests can substitute a mock/miniredis
//
// Pool settings to configure (see §4.6.1):
//   PoolSize: 50-100 (tune during perf)
//   MinIdleConns: 10-20
//   MaxConnAge: 30 min
//   PoolTimeout: 200ms
//   ReadTimeout / WriteTimeout: 500ms
//
// Public API:
//   func New(cfg *config.RedisConfig) (*Client, error)
//   func (c *Client) Close() error
package cache

// ---------------------------------------------------------------------------
// Phase 2 implementation.
//
// Deviation from the sketch above: New also takes the session TTL. It has to
// live somewhere, the session helpers in session.go have no config of their
// own, and hanging it off the client is preferable to a package-level global.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"entry_profile_creation/internal/config"
)

const (
	// minIdleConns keeps warm connections ready so a burst of traffic does not
	// pay TCP setup on the critical path. Clamped to PoolSize below.
	minIdleConns = 10

	// connMaxLifetime recycles connections periodically, so that a Redis
	// restart or a failover does not leave stale sockets in the pool.
	connMaxLifetime = 30 * time.Minute

	// poolTimeout bounds how long a caller waits for a free connection. Failing
	// fast under overload is better than queueing: a queued request still holds
	// its goroutine, its TCP conn and the client's patience.
	poolTimeout = 200 * time.Millisecond

	// ioTimeout bounds a single read or write. Redis commands here are O(1)
	// GET/SET, so anything near half a second means the server is in trouble.
	ioTimeout = 500 * time.Millisecond

	// pingTimeout bounds only the startup connectivity check.
	pingTimeout = 2 * time.Second
)

// Client wraps *redis.Client so the rest of the service depends on our typed
// helpers rather than on go-redis directly. Keeping the embedded client
// unexported is what makes it possible to swap in miniredis, or to add metrics,
// without touching callers.
type Client struct {
	rdb        *redis.Client
	sessionTTL time.Duration
}

// New builds a Redis client and verifies connectivity.
//
// Unlike MySQL, the pool here is go-redis's own. Writing a custom one is the
// MySQL exercise (phase 4); duplicating it for Redis would buy nothing, since
// Redis commands are short and the library's pool is already well tuned.
func New(cfg *config.RedisConfig, sessionTTL time.Duration) (*Client, error) {
	idle := minIdleConns
	if cfg.PoolSize < idle {
		idle = cfg.PoolSize
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Password:        cfg.Password,
		DB:              cfg.DB,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    idle,
		ConnMaxLifetime: connMaxLifetime,
		PoolTimeout:     poolTimeout,
		ReadTimeout:     ioTimeout,
		WriteTimeout:    ioTimeout,
	})

	// redis.NewClient, like sql.Open, does not connect. Ping so a wrong address
	// or password fails at boot instead of on the first login attempt.
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("cache: ping redis at %s: %w", cfg.Addr, err)
	}

	return &Client{rdb: rdb, sessionTTL: sessionTTL}, nil
}

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }
