// TODO: Session key helpers (sess:<token>).
//
// Value shape: JSON {user_id, username, created_at, ip?, last_seen}
//
// Functions:
//   - CreateSession(ctx, userID, rowVersion, username, ip) (token string, err error)
//       Generate 32-byte random token (via crypto/random.go)
//       SETEX sess:<token> {...} 24h
//   - GetSession(ctx, token) (*model.Session, error)
//       GET sess:<token>; parse JSON
//       Refresh last_seen and TTL if sliding window (see §4.2)
//   - DeleteSession(ctx, token) error
//       DEL sess:<token>
package cache

// ---------------------------------------------------------------------------
// Phase 2 implementation. Sessions only; user.go and invalidate.go are phase 5.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/model"
)

// sessionKeyPrefix namespaces session keys so they can be scanned, counted or
// flushed independently of the phase 5 user-profile keys in the same database.
const sessionKeyPrefix = "sess:"

// ErrSessionNotFound means the token is unknown or has expired. Both cases are
// the same thing to a caller: the request is unauthenticated. go-redis's
// redis.Nil is translated here so handlers never import go-redis.
var ErrSessionNotFound = errors.New("cache: session not found")

func sessionKey(token string) string { return sessionKeyPrefix + token }

// CreateSession mints a new session and returns its token, which the HTTP layer
// sets as a cookie.
//
// rowVersion seeds the session's cache-version hint (model.Session.RowVersion)
// so the very first GetUser call on this session can try the right
// profile:<user_id>:v<N> key instead of guessing 0. The caller should pass the
// row_version the row actually had at login time — from a fresh MySQL read on
// a cache miss, or carried through from model.UserCached on a cache hit —
// never a hardcoded 0, or every fresh login would eat one guaranteed miss on
// its first profile read.
func (c *Client) CreateSession(ctx context.Context, userID, rowVersion uint64, username, ip string) (string, error) {
	token, err := crypto.SessionToken()
	if err != nil {
		return "", fmt.Errorf("cache: create session: %w", err)
	}

	now := time.Now().UTC()
	blob, err := json.Marshal(&model.Session{
		UserID:     userID,
		Username:   username,
		RowVersion: rowVersion,
		CreatedAt:  now,
		LastSeen:   now,
		IP:         ip,
	})
	if err != nil {
		return "", fmt.Errorf("cache: create session: marshal: %w", err)
	}

	// Set with a TTL is SETEX. The TTL is the only expiry mechanism: there is
	// no sweeper to write, and an abandoned session cannot leak forever.
	if err := c.rdb.Set(ctx, sessionKey(token), blob, c.sessionTTL).Err(); err != nil {
		return "", fmt.Errorf("cache: create session: %w", err)
	}
	return token, nil
}

// GetSession resolves a token to its session, refreshing the TTL when it is
// running low.
func (c *Client) GetSession(ctx context.Context, token string) (*model.Session, error) {
	if token == "" {
		return nil, ErrSessionNotFound
	}
	key := sessionKey(token)

	// GET and PTTL are pipelined into a single round trip. Issuing them
	// separately would double the Redis latency of every authenticated request.
	// PTTL rather than TTL because TTL answers in whole seconds, which would
	// make the comparison below lumpy for any short-lived session.
	pipe := c.rdb.Pipeline()
	getCmd := pipe.Get(ctx, key)
	ttlCmd := pipe.PTTL(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("cache: get session: %w", err)
	}

	blob, err := getCmd.Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, ErrSessionNotFound
	case err != nil:
		return nil, fmt.Errorf("cache: get session: %w", err)
	}

	var s model.Session
	if err := json.Unmarshal(blob, &s); err != nil {
		return nil, fmt.Errorf("cache: get session: unmarshal: %w", err)
	}
	// Token is the key, not part of the value, so it is filled in on read.
	s.Token = token

	// Sliding window. Refreshing on every read would turn each authenticated
	// request into a Redis write; refreshing only in the second half of the
	// lifetime keeps sessions alive for active users at a small fraction of the
	// write volume, at the cost of expiry being approximate.
	if remaining := ttlCmd.Val(); remaining > 0 && remaining < c.sessionTTL/2 {
		c.refresh(ctx, key, &s)
	}

	return &s, nil
}

// refresh extends the TTL and updates last_seen. Failures are deliberately
// swallowed: the session that was just read is still valid, and failing the
// request over a bookkeeping write would be a worse outcome than an early
// expiry. Once logging is wired into this layer, this is where it belongs.
func (c *Client) refresh(ctx context.Context, key string, s *model.Session) {
	s.LastSeen = time.Now().UTC()
	blob, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, key, blob, c.sessionTTL).Err()
}

// RefreshSessionRowVersion corrects a session's cache-version hint after a
// GetProfileCache guess using it turned out to be stale.
//
// A no-op when the hint is already right, which is the common case: this only
// ever runs on the miss path, right after a MySQL read has supplied the true
// row_version. Failures are swallowed like every other refresh in this file —
// the session that was just read is still valid either way, and the cost of a
// missed correction is one more avoidable cache miss on this session's next
// read, never wrong data.
func (c *Client) RefreshSessionRowVersion(ctx context.Context, token string, s *model.Session, rowVersion uint64) {
	if s.RowVersion == rowVersion {
		return
	}
	s.RowVersion = rowVersion
	c.refresh(ctx, sessionKey(token), s)
}

// DeleteSession revokes a session. Deleting a key that does not exist is not an
// error, which makes logout idempotent.
func (c *Client) DeleteSession(ctx context.Context, token string) error {
	if err := c.rdb.Del(ctx, sessionKey(token)).Err(); err != nil {
		return fmt.Errorf("cache: delete session: %w", err)
	}
	return nil
}
