// TODO: Cache invalidation helpers (delete-on-write, see §4.1).
//
// Delete-on-write is race-safe under concurrent writers (deletes are idempotent).
//
// Functions:
//   - InvalidateOnNicknameChange(ctx, userID, username) error
//       DEL profile:<user_id>
//       DEL user:<username>
//   - InvalidateOnAvatarChange(ctx, userID, username) error
//       DEL profile:<user_id>
//       DEL user:<username>
//   - InvalidateOnPasswordChange(ctx, username) error
//       DEL user:<username>
//       (Future: also invalidate all sessions for that user)
//   - InvalidateSession(ctx, token) error
//       DEL sess:<token>
//
// Errors from Redis DEL should be logged, not fatal — TTL will eventually clear.
package cache

// ---------------------------------------------------------------------------
// Phase 5 implementation.
//
// Why delete rather than update: two concurrent writers that each SET their own
// version can commit to MySQL in one order and reach Redis in the other,
// leaving the cache holding the value that lost. DEL has no such ordering
// problem — whichever lands last, the next reader re-reads MySQL and caches the
// row that actually won.
//
// The delete happens AFTER the MySQL write in every caller. Deleting first
// would leave a window in which a concurrent reader repopulates the key from
// the not-yet-updated row and pins the stale value for a full TTL.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
)

// del removes keys in a single round trip. Redis DEL is variadic and
// idempotent, so a key that is not cached costs nothing and is not an error.
func (c *Client) del(ctx context.Context, what string, keys ...string) error {
	if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cache: invalidate %s: %w", what, err)
	}
	return nil
}

// InvalidateOnNicknameChange drops both records that embed the nickname.
//
// The user: key has to go too, not just profile:. It is keyed by username and
// read by login, but it carries a nickname copy, so leaving it would let a
// login-path read serve the old display name for up to 10 minutes.
func (c *Client) InvalidateOnNicknameChange(ctx context.Context, userID uint64, username string) error {
	return c.del(ctx, "nickname", profileKey(userID), userKey(username))
}

// InvalidateOnAvatarChange drops both records that embed the avatar version.
//
// Strictly this is belt and braces: the avatar URL is versioned, so a stale
// entry points at a file that still exists rather than at a broken image. It is
// still dropped, because "the upload succeeded but my picture is unchanged" is
// exactly the bug report this design is meant to make impossible.
func (c *Client) InvalidateOnAvatarChange(ctx context.Context, userID uint64, username string) error {
	return c.del(ctx, "avatar", profileKey(userID), userKey(username))
}

// InvalidateOnPasswordChange drops the record holding the bcrypt hash.
//
// Only user: matters here, since profile: never sees the hash. Note what this
// does NOT do: existing sessions survive a password change, because sessions
// are keyed by token and there is no token index per user. Revoking them needs
// a user:<id> → set-of-tokens index, which is deliberately out of scope.
func (c *Client) InvalidateOnPasswordChange(ctx context.Context, username string) error {
	return c.del(ctx, "password", userKey(username))
}

// InvalidateSession revokes one session by token. DeleteSession is the same
// operation; this exists so callers thinking in terms of invalidation find it
// next to the others.
func (c *Client) InvalidateSession(ctx context.Context, token string) error {
	return c.DeleteSession(ctx, token)
}
