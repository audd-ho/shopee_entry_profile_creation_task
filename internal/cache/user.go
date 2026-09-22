// TODO: User-record cache helpers.
//
// Two key patterns (see §5):
//   user:<username>            → JSON {id, password_hash, nickname, avatar_version, avatar_suffix, row_version}, TTL 10min (key itself NOT versioned; see below)
//   profile:<user_id>:v<row_version> → JSON {nickname, avatar_version, avatar_suffix}, TTL 1h
//
// Functions:
//   - GetUserCache(ctx, username) (*model.UserCached, bool, error)
//       GET user:<username>; returns (val, hit, err)
//   - SetUserCache(ctx, username, user) error
//       SETEX user:<username> 10min
//   - GetProfileCache(ctx, userID, rowVersion) (*model.ProfileCached, bool, error)
//       GET profile:<user_id>:v<rowVersion>
//   - SetProfileCache(ctx, userID, rowVersion, profile) error
//       SETEX profile:<user_id>:v<rowVersion> 1h
package cache

// ---------------------------------------------------------------------------
// Phase 5 implementation.
//
// Both caches are read-through on miss and delete-on-write (invalidate.go), not
// write-through. A miss costs one extra Redis round trip and one MySQL query;
// keeping writes as deletes is what makes concurrent updates safe without a
// lock, because a DEL cannot land out of order the way two competing SETs can.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"entry_profile_creation/internal/model"
)

const (
	userKeyPrefix    = "user:"
	profileKeyPrefix = "profile:"

	// userTTL is short because the value carries the password hash: a password
	// change that somehow escaped invalidation must not keep authenticating the
	// old password for long.
	userTTL = 10 * time.Minute

	// profileTTL is long because the value is cosmetic (nickname, avatar) and
	// every path that changes it deletes the key explicitly. The TTL is only a
	// backstop for an invalidation that failed.
	profileTTL = time.Hour
)

// userKey is deliberately unversioned — see the comment on model.UserCached
// for why row_version cannot cheaply live in this particular key.
func userKey(username string) string { return userKeyPrefix + username }

// profileKey embeds row_version directly in the key (system_design.md §4.1),
// so the previous version's key is structurally unreachable the instant
// row_version bumps in MySQL — a reader can never repopulate it, because
// nothing will ever ask for it again. The delete-on-write call in
// invalidate.go still removes the old key so it does not linger to its TTL,
// but correctness does not depend on that delete winning any race.
//
// This works cheaply for profile:<user_id> specifically because a caller
// always has a version to try at near-zero cost: the session record already
// carries one (see model.Session.RowVersion), and the session is being read
// on every authenticated request anyway to resolve identity. A wrong guess
// degrades to an ordinary cache miss, not an error — see
// tcpserver.handleGetUser.
func profileKey(userID uint64, rowVersion uint64) string {
	return profileKeyPrefix + strconv.FormatUint(userID, 10) + ":v" + strconv.FormatUint(rowVersion, 10)
}

// getJSON is the shared body of both getters: a GET whose miss is a value, not
// an error. The (value, hit, err) shape forces callers to distinguish "not
// cached" from "Redis is broken", which matters because the first is routine
// and the second must not be treated as "no such user".
func getJSON[T any](ctx context.Context, c *Client, key string) (*T, bool, error) {
	blob, err := c.rdb.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("cache: get %s: %w", key, err)
	}

	var v T
	if err := json.Unmarshal(blob, &v); err != nil {
		// A corrupt or stale-shaped entry is treated as a miss rather than an
		// error: the caller falls through to MySQL and overwrites it. Failing
		// the request instead would mean one bad key breaks a user until it
		// expires.
		return nil, false, nil
	}
	return &v, true, nil
}

func setJSON(ctx context.Context, c *Client, key string, v any, ttl time.Duration) error {
	blob, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache: set %s: marshal: %w", key, err)
	}
	if err := c.rdb.Set(ctx, key, blob, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %s: %w", key, err)
	}
	return nil
}

// The four helpers below are the entire cache layer as far as the handlers are
// concerned, which is why the CACHE_LAYER=off switch is implemented here rather
// than in tcpserver. A disabled cache reports a permanent miss and drops its
// writes, so every read falls through to MySQL on every request — a true
// steady-state uncached run, not the cold start that flushing Redis would give.
// The handlers in tcpserver need no branch and cannot drift out of sync with
// the flag.

// GetUserCache reads the login record for a username.
func (c *Client) GetUserCache(ctx context.Context, username string) (*model.UserCached, bool, error) {
	if c.userCacheDisabled {
		return nil, false, nil
	}
	return getJSON[model.UserCached](ctx, c, userKey(username))
}

// SetUserCache populates the login record after a MySQL read.
func (c *Client) SetUserCache(ctx context.Context, username string, u *model.UserCached) error {
	if c.userCacheDisabled {
		return nil
	}
	return setJSON(ctx, c, userKey(username), u, userTTL)
}

// GetProfileCache reads the display record for a user id at a specific
// row_version — normally a hint carried on the caller's session, not a value
// the caller has independently confirmed is current. See profileKey.
func (c *Client) GetProfileCache(ctx context.Context, userID, rowVersion uint64) (*model.ProfileCached, bool, error) {
	if c.userCacheDisabled {
		return nil, false, nil
	}
	return getJSON[model.ProfileCached](ctx, c, profileKey(userID, rowVersion))
}

// SetProfileCache populates the display record after a MySQL read, under the
// row_version that MySQL read returned — never a guessed or stale value, or
// this would cache the row under the wrong key.
func (c *Client) SetProfileCache(ctx context.Context, userID, rowVersion uint64, p *model.ProfileCached) error {
	if c.userCacheDisabled {
		return nil
	}
	return setJSON(ctx, c, profileKey(userID, rowVersion), p, profileTTL)
}
