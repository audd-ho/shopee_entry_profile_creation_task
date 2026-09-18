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
