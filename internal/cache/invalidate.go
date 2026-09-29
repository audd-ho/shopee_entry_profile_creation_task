// TODO: Cache invalidation helpers (delete-on-write, see §4.1).
//
// Delete-on-write is race-safe under concurrent writers (deletes are idempotent).
//
// Functions:
//   - InvalidateOnNicknameChange(ctx, userID, oldRowVersion) error
//       DEL user:<user_id>:v<oldRowVersion>
//   - InvalidateOnAvatarChange(ctx, userID, oldRowVersion) error
//       DEL user:<user_id>:v<oldRowVersion>
//   - InvalidateOnPasswordChange(ctx, username) error
//       DEL auth:<username>
//       (No caller today — no change-password feature exists. Kept for when
//       one is added, rather than deleted; see its own comment.)
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

// InvalidateOnNicknameChange drops the record that embeds the nickname.
//
// oldRowVersion is the row_version from just before the write that triggered
// this call — it targets the specific user:<user_id>:v<N> key that write
// stamped as stale. auth:<username> is not touched: it no longer carries
// nickname, avatar, or anything else a profile edit could make stale — see
// model.AuthCached.
func (c *Client) InvalidateOnNicknameChange(ctx context.Context, userID, oldRowVersion uint64) error {
	return c.del(ctx, "nickname", userKey(userID, oldRowVersion))
}

// InvalidateOnAvatarChange drops the record that embeds the avatar suffix.
//
// oldRowVersion is the row_version from just before the write, same reasoning
// as InvalidateOnNicknameChange. This is belt and braces — the avatar URL is
// itself versioned by suffix, so a stale user: entry would point at a file
// that still exists rather than at a broken image — but it is still dropped,
// because "the upload succeeded but my picture is unchanged" is exactly the
// bug report this design is meant to make impossible. auth:<username> is not
// touched, for the same reason as InvalidateOnNicknameChange.
func (c *Client) InvalidateOnAvatarChange(ctx context.Context, userID, oldRowVersion uint64) error {
	return c.del(ctx, "avatar", userKey(userID, oldRowVersion))
}

// InvalidateOnPasswordChange drops the record holding the bcrypt hash.
//
// No caller exists today: this service has no change-password feature, so
// the trigger for this never fires. Kept rather than deleted, since deleting
// working code for a feature that is merely unimplemented — as opposed to
// impossible — just means writing it again later; auth:<username> is exactly
// the key such a feature would need to invalidate, unchanged by everything
// else in this redesign. Note what this does NOT do: existing sessions
// survive a password change, because sessions are keyed by token and there is
// no token index per user. Revoking them needs a user:<id> → set-of-tokens
// index, which is deliberately out of scope.
func (c *Client) InvalidateOnPasswordChange(ctx context.Context, username string) error {
	return c.del(ctx, "password", authKey(username))
}

// InvalidateSession revokes one session by token. DeleteSession is the same
// operation; this exists so callers thinking in terms of invalidation find it
// next to the others.
func (c *Client) InvalidateSession(ctx context.Context, token string) error {
	return c.DeleteSession(ctx, token)
}
