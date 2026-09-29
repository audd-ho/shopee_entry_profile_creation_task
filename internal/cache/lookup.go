// TODO: Lookup cache helpers (see sharding_design.md §8.3).
//
// Key pattern:
//   lookup:<username> → JSON {user_id}, TTL 10min
//
// Write-once data: a username maps to one user_id forever. No versioning,
// no invalidation needed. SET on signup (after successful insert), populated
// on login cache miss.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	lookupKeyPrefix = "lookup:"
	lookupTTL       = 10 * time.Minute
)

func lookupKey(username string) string { return lookupKeyPrefix + username }

type lookupCached struct {
	UserID uint64 `json:"user_id"`
}

// GetLookupCache reads the username→userID mapping from cache.
func (c *Client) GetLookupCache(ctx context.Context, username string) (uint64, bool, error) {
	if c.userCacheDisabled {
		return 0, false, nil
	}
	key := lookupKey(username)
	blob, err := c.rdb.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("cache: get %s: %w", key, err)
	}

	var v lookupCached
	if err := json.Unmarshal(blob, &v); err != nil {
		return 0, false, nil
	}
	return v.UserID, true, nil
}

// SetLookupCache populates the username→userID mapping after a successful
// lookup-table read or signup insert.
func (c *Client) SetLookupCache(ctx context.Context, username string, userID uint64) error {
	if c.userCacheDisabled {
		return nil
	}
	return setJSON(ctx, c, lookupKey(username), &lookupCached{UserID: userID}, lookupTTL)
}
