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

// ---------------------------------------------------------------------------
// Phase 3 implementation.
//
// Not yet here, deliberately: the Redis user:<username> and profile:<user_id>
// caches described above. Those are phase 5, and adding them before there is a
// working uncached path would mean debugging invalidation and SQL at once.
// Every handler below therefore goes straight to MySQL.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"log"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/model"
	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// handleSignup creates an account.
//
// The inputs are validated again even though the HTTP server already did. That
// is not redundancy for its own sake: the HTTP server is a separate process
// reachable over a TCP port, so from here it is just another client, and a
// second HTTP server, a stray test, or a mistaken refactor must not be able to
// write an invalid row.
func (s *Server) handleSignup(ctx context.Context, d *protocol.SignupData) (*protocol.SignupResult, error) {
	if err := validate.Username(d.Username); err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}
	if err := validate.Password(d.Password); err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}
	nickname, err := validate.Nickname(d.Nickname)
	if err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}

	hash, err := crypto.HashPasswordWithCost(d.Password, s.cfg.Bcrypt.Cost)
	if err != nil {
		log.Printf("signup: hash password: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	// No SELECT to check availability first. Two concurrent signups for the
	// same name would both see it free; uk_username is the only real check.
	id, err := db.InsertUser(ctx, s.db, d.Username, hash, nickname)
	switch {
	case errors.Is(err, db.ErrDuplicateUsername):
		return nil, protocol.ErrCodeDuplicateUsername
	case err != nil:
		log.Printf("signup: insert user: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	// Signup deliberately does not issue a session; the client logs in next.
	return &protocol.SignupResult{UserID: uint64(id)}, nil
}

// handleLogin authenticates and mints a session.
func (s *Server) handleLogin(ctx context.Context, d *protocol.LoginData) (*protocol.LoginResult, error) {
	user, err := db.GetUserByUsername(ctx, s.db, d.Username)
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		// Spend a bcrypt anyway. Returning immediately would make a missing
		// account answer in microseconds and a wrong password take ~75ms, and
		// that gap tells an attacker which usernames are real no matter how
		// uniform the error message is.
		crypto.DummyVerify(s.cfg.Bcrypt.Cost)
		return nil, protocol.ErrCodeInvalidCredentials
	case err != nil:
		log.Printf("login: lookup user: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	if err := crypto.VerifyPassword(user.PasswordHash, []byte(d.Password)); err != nil {
		if errors.Is(err, crypto.ErrPasswordMismatch) {
			// Same code as "no such user", on purpose.
			return nil, protocol.ErrCodeInvalidCredentials
		}
		// A corrupt hash in the row, not a wrong password.
		log.Printf("login: verify password for user %d: %v", user.ID, err)
		return nil, protocol.ErrCodeInternal
	}

	token, err := s.cache.CreateSession(ctx, user.ID, user.Username, d.IP)
	if err != nil {
		log.Printf("login: create session for user %d: %v", user.ID, err)
		return nil, protocol.ErrCodeInternal
	}

	return &protocol.LoginResult{Token: token, UserID: user.ID}, nil
}

// handleLogout revokes a session.
func (s *Server) handleLogout(ctx context.Context, d *protocol.LogoutData) (*protocol.LogoutResult, error) {
	// An empty or unknown token is not an error. Logout is idempotent, and
	// reporting "that session did not exist" would turn this endpoint into a
	// way to test whether a stolen token is still live.
	if d.Token == "" {
		return &protocol.LogoutResult{}, nil
	}

	if err := s.cache.DeleteSession(ctx, d.Token); err != nil {
		log.Printf("logout: delete session: %v", err)
		return nil, protocol.ErrCodeInternal
	}
	return &protocol.LogoutResult{}, nil
}

// handleGetUser returns the profile of whoever owns the token.
func (s *Server) handleGetUser(ctx context.Context, d *protocol.GetUserData) (*protocol.GetUserResult, error) {
	session, err := s.resolveSession(ctx, d.Token)
	if err != nil {
		return nil, err
	}

	user, err := db.GetUserByID(ctx, s.db, session.UserID)
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		// The session outlived its user. Revoke it rather than serving 401s
		// until the TTL expires.
		if delErr := s.cache.DeleteSession(ctx, d.Token); delErr != nil {
			log.Printf("get_user: revoke orphaned session: %v", delErr)
		}
		return nil, protocol.ErrCodeUnauthorized
	case err != nil:
		log.Printf("get_user: lookup user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	return &protocol.GetUserResult{
		UserID:   user.ID,
		Username: user.Username,
		Nickname: user.Nickname,
		// Derived, never stored. See model.User.AvatarURL.
		AvatarURL: user.AvatarURL(),
	}, nil
}

// resolveSession turns an opaque token into a session, or an error code.
//
// This is the single place session validation happens for the whole service.
// The HTTP server only reads the cookie and forwards the string; it has no
// Redis connection and no way to interpret the token, which is what keeps
// authentication decisions in one process instead of two.
func (s *Server) resolveSession(ctx context.Context, token string) (*model.Session, error) {
	if token == "" {
		return nil, protocol.ErrCodeUnauthorized
	}

	session, err := s.cache.GetSession(ctx, token)
	switch {
	case errors.Is(err, cache.ErrSessionNotFound):
		// Unknown and expired are the same answer: not authenticated.
		return nil, protocol.ErrCodeUnauthorized
	case err != nil:
		// Redis is down. This is NOT unauthorized — saying so would log every
		// user out of a healthy application because a cache is unreachable.
		log.Printf("resolve session: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	return session, nil
}
