// TODO: Session key helpers (sess:<token>).
//
// Value shape: JSON {user_id, username, created_at, ip?, last_seen}
//
// Functions:
//   - CreateSession(ctx, userID, username, ip) (token string, err error)
//       Generate 32-byte random token (via crypto/random.go)
//       SETEX sess:<token> {...} 24h
//   - GetSession(ctx, token) (*model.Session, error)
//       GET sess:<token>; parse JSON
//       Refresh last_seen and TTL if sliding window (see §4.2)
//   - DeleteSession(ctx, token) error
//       DEL sess:<token>
package cache
