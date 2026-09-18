package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"entry_profile_creation/internal/model"

	"github.com/redis/go-redis/v9"
)

// ErrSessionNotFound is returned when a session lookup misses.
var ErrSessionNotFound = errors.New("session not found")

func sessionKey(token string) string {
	return "sess:" + token
}

// CreateSession writes a new session to Redis under sess:<token> with the given TTL.
func (c *Client) CreateSession(ctx context.Context, s *model.Session, ttl time.Duration) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	s.LastSeen = time.Now().UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, sessionKey(s.Token), b, ttl).Err()
}

// GetSession reads a session from Redis. Returns ErrSessionNotFound on cache miss.
func (c *Client) GetSession(ctx context.Context, token string) (*model.Session, error) {
	b, err := c.rdb.Get(ctx, sessionKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	var s model.Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	s.Token = token
	return &s, nil
}

// RefreshSession updates last_seen and extends the TTL (sliding window).
func (c *Client) RefreshSession(ctx context.Context, s *model.Session, ttl time.Duration) error {
	s.LastSeen = time.Now().UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, sessionKey(s.Token), b, ttl).Err()
}

// DeleteSession removes a session from Redis.
func (c *Client) DeleteSession(ctx context.Context, token string) error {
	return c.rdb.Del(ctx, sessionKey(token)).Err()
}
