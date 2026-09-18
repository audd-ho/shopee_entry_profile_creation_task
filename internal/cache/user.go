// TODO: User-record cache helpers.
//
// Two key patterns (see §5):
//   user:<username>   → JSON {id, password_hash, nickname, avatar_version, avatar_suffix}, TTL 10min
//   profile:<user_id> → JSON {nickname, avatar_version, avatar_suffix}, TTL 1h
//
// Functions:
//   - GetUserCache(ctx, username) (*model.UserCached, bool, error)
//       GET user:<username>; returns (val, hit, err)
//   - SetUserCache(ctx, username, user) error
//       SETEX user:<username> 10min
//   - GetProfileCache(ctx, userID) (*model.ProfileCached, bool, error)
//       GET profile:<user_id>
//   - SetProfileCache(ctx, userID, profile) error
//       SETEX profile:<user_id> 1h
package cache
