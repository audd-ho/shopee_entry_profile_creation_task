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
//     HIT → deserialize {id, password_hash, nickname, avatar_version, avatar_suffix, row_version}
//     MISS → SELECT ... FROM users WHERE username = ?
//     SET user:<username> with 10-min TTL
//     If user not found → return err (uniform message)
//     bcrypt.CompareHashAndPassword(hash, password)
//     Mismatch → return err (uniform message)
//     Generate 32-byte random session token (crypto/random.go)
//     SETEX sess:<token> {user_id, username, row_version, created_at, ip, last_seen} 24h
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
// Phase 3 implementation, with the phase 5 caches now layered on top.
//
// The read paths are cache-aside: look in Redis, fall through to MySQL on a
// miss, populate on the way back. Writes never update the cache, they delete it
// (cache/invalidate.go). A Redis failure on either side is logged and ignored
// rather than returned — the cache is an optimisation, and a service that 500s
// because a cache is unreachable is worse than a slow one.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
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

	id, err := s.idgen.Next()
	if err != nil {
		log.Printf("signup: generate id: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	// Reserve the username globally on the lookup shard FIRST. Two concurrent
	// signups for the same name both hash to the same lookup shard and the same
	// PK — one wins, one gets a duplicate key error. No cross-shard coordination.
	lookupPool := s.db.LookupShardFor(d.Username)
	if err := db.InsertLookup(ctx, lookupPool, d.Username, id); err != nil {
		if errors.Is(err, db.ErrDuplicateUsername) {
			return nil, protocol.ErrCodeDuplicateUsername
		}
		log.Printf("signup: insert lookup: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	dataPool := s.db.DataShardFor(id)
	if err := db.InsertUser(ctx, dataPool, id, d.Username, hash, nickname); err != nil {
		// Rollback the lookup reservation — explicit, not defer, because we only
		// want this on failure.
		if delErr := db.DeleteLookup(ctx, lookupPool, d.Username); delErr != nil {
			log.Printf("signup: rollback lookup for %q: %v", d.Username, delErr)
		}
		log.Printf("signup: insert user: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	// Background: cache the lookup so the next login skips the MySQL lookup.
	go func() {
		if cErr := s.cache.SetLookupCache(context.Background(), d.Username, id); cErr != nil {
			log.Printf("signup: cache lookup for %q: %v", d.Username, cErr)
		}
	}()

	return &protocol.SignupResult{UserID: id}, nil
}

// handleLogin authenticates and mints a session.
func (s *Server) handleLogin(ctx context.Context, d *protocol.LoginData) (*protocol.LoginResult, error) {
	user, err := s.lookupUserForLogin(ctx, d.Username)
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

	token, err := s.cache.CreateSession(ctx, user.ID, d.IP)
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

	dataPool := s.db.DataShardFor(session.UserID)
	data, _, err := s.lockstack.Read(ctx, userCacheKey(session.UserID), func(ctx context.Context) ([]byte, int64, error) {
		user, err := db.GetUserByID(ctx, dataPool, session.UserID)
		if err != nil {
			return nil, 0, err
		}
		blob, err := json.Marshal(user.CachedUser())
		if err != nil {
			return nil, 0, err
		}
		return blob, int64(user.RowVersion), nil
	})
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		if delErr := s.cache.DeleteSession(ctx, d.Token); delErr != nil {
			log.Printf("get_user: revoke orphaned session: %v", delErr)
		}
		return nil, protocol.ErrCodeUnauthorized
	case err != nil:
		log.Printf("get_user: lookup user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	var cached model.UserCached
	if err := json.Unmarshal(data, &cached); err != nil {
		log.Printf("get_user: unmarshal cached data for %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	return &protocol.GetUserResult{
		UserID:    session.UserID,
		Username:  cached.Username,
		Nickname:  cached.Nickname,
		AvatarURL: cached.AvatarURL(session.UserID),
	}, nil
}

// lookupUserForLogin resolves a username to a user, through the lookup shard
// and auth:<username> cache.
//
// The flow has two phases:
//  1. Lookup phase: resolve username → userID via lookup cache / lookup shard.
//  2. Auth phase: resolve userID → *User via auth cache / data shard.
//
// Returns db.ErrUserNotFound for an unknown account so the caller's existing
// switch is unchanged. Note that a miss is NOT cached: storing a negative entry
// would let anyone fill Redis with keys for usernames that do not exist simply
// by attempting logins. Unknown usernames therefore always cost a MySQL query,
// which is acceptable because they also always cost a dummy bcrypt.
func (s *Server) lookupUserForLogin(ctx context.Context, username string) (*model.User, error) {
	// Phase 1: username → userID via lookup cache / lookup shard.
	userID, err := s.resolveUserID(ctx, username)
	if err != nil {
		return nil, err
	}

	dataPool := s.db.DataShardFor(userID)

	// Phase 2: auth cache → data shard, same as before but routed to the
	// correct shard pool.
	if cached, hit, cErr := s.cache.GetAuthCache(ctx, username); cErr != nil {
		log.Printf("login: read auth cache for %q: %v", username, cErr)
	} else if hit {
		return cached.User(username), nil
	}

	user, err := db.GetUserByUsername(ctx, dataPool, username)
	if err != nil {
		return nil, err
	}

	if cErr := s.cache.SetAuthCache(ctx, username, user.CachedAuth()); cErr != nil {
		log.Printf("login: populate auth cache for %q: %v", username, cErr)
	}

	return user, nil
}

// resolveUserID turns a username into a userID, checking the lookup cache
// first, then falling back to the lookup shard.
func (s *Server) resolveUserID(ctx context.Context, username string) (uint64, error) {
	if userID, hit, err := s.cache.GetLookupCache(ctx, username); err != nil {
		log.Printf("login: read lookup cache for %q: %v", username, err)
	} else if hit {
		return userID, nil
	}

	lookupPool := s.db.LookupShardFor(username)
	userID, err := db.GetLookup(ctx, lookupPool, username)
	if err != nil {
		return 0, err
	}

	if cErr := s.cache.SetLookupCache(ctx, username, userID); cErr != nil {
		log.Printf("login: populate lookup cache for %q: %v", username, cErr)
	}
	return userID, nil
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
