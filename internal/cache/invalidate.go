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
