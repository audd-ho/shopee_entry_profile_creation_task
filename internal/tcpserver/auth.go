// TODO: Auth business logic on the TCP server.
//
// Handlers:
//
//   - handleSignup(data)
//     Validate again (defense in depth)
//     bcrypt.GenerateFromPassword(password, cost=10)
//     INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)
//     Handle duplicate-username error → return err
//     No caching needed on signup
//
//   - handleLogin(data)
//     Try Redis GET user:<username>
//     HIT → deserialize {id, password_hash, nickname, avatar_version, avatar_suffix}
//     MISS → SELECT ... FROM users WHERE username = ?
//     SET user:<username> with 10-min TTL
//     If user not found → return err (uniform message)
//     bcrypt.CompareHashAndPassword(hash, password)
//     Mismatch → return err (uniform message)
//     Generate 32-byte random session token (crypto/random.go)
//     SETEX sess:<token> {user_id, username, created_at, ip, last_seen} 24h
//     Return {token, user_id, nickname, avatar_url}
//
//   - handleLogout(data)
//     DEL sess:<token>
//     Return ok
//
//   - handleGetMe(data)
//     user_id already known from session (passed in data)
//     Try Redis GET profile:<user_id>
//     HIT → return it
//     MISS → SELECT nickname, avatar_version, avatar_suffix FROM users WHERE id = ?
//     SET profile:<user_id> with 1h TTL
//     Compose avatar_url:
//     if avatar_version == 0 → "/default.png"
//     else → "/uploads/<user_id>>>14/<user_id>/<user_id>_v<version>_<suffix>.jpg"
//     Return {username (from session), nickname, avatar_url}
package tcpserver
