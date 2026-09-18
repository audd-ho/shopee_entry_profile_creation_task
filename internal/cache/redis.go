// Package cache wraps the Redis client and provides typed helpers for our key patterns.
package cache

import (
	"context"

	"entry_profile_creation/internal/config"

	"github.com/redis/go-redis/v9"
)

// Client holds the Redis client. Uses go-redis's built-in pool (see §4.6.1).
type Client struct {
	rdb *redis.Client
}

// New constructs a Redis client from config.
func New(cfg *config.RedisConfig) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})
	// Verify connectivity
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &Client{rdb: rdb}, nil
}

// Raw returns the underlying redis.Client for advanced use.
func (c *Client) Raw() *redis.Client {
	return c.rdb
}

// Close closes the underlying client.
func (c *Client) Close() error {
	return c.rdb.Close()
}
