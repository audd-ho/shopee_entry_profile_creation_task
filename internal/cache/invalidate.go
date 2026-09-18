package cache

import "context"

// InvalidateUser deletes the display cache after a nickname or avatar change.
// The credentials cache (creds:<username>) is intentionally NOT touched — password
// didn't change, so the cached hash remains valid. Login's hot-path cache stays warm.
// Delete-on-write invalidation is race-safe (deletes are idempotent).
func (c *Client) InvalidateUser(ctx context.Context, userID uint64) error {
	return c.rdb.Del(ctx, userKey(userID)).Err()
}

// InvalidateCreds deletes the login cache after a password change.
// (Not currently used — password change endpoint not implemented — but here for future.)
func (c *Client) InvalidateCreds(ctx context.Context, username string) error {
	return c.rdb.Del(ctx, credsKey(username)).Err()
}
