package tcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/model"
	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// handleSignup creates a new user account.
func (s *Server) handleSignup(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.SignupData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}

	if err := validate.Username(data.Username); err != nil {
		return errResponse(req.ReqID, err.Error())
	}
	if err := validate.Password(data.Password); err != nil {
		return errResponse(req.ReqID, err.Error())
	}
	nickname, err := validate.Nickname(data.Nickname)
	if err != nil {
		// Fall back to username as nickname if none provided
		if data.Nickname == "" {
			nickname = data.Username
		} else {
			return errResponse(req.ReqID, err.Error())
		}
	}

	hash, err := crypto.HashPasswordCost(data.Password, s.cfg.Bcrypt.Cost)
	if err != nil {
		return errResponse(req.ReqID, "hashing failed")
	}

	id, err := db.InsertUser(ctx, s.pool, data.Username, hash, nickname)
	if err != nil {
		if errors.Is(err, db.ErrDuplicateUsername) {
			return errResponse(req.ReqID, "username already exists")
		}
		return errResponse(req.ReqID, "database error")
	}

	return okResponse(req.ReqID, protocol.SignupResult{UserID: id})
}

// handleLogin authenticates a user and creates a session.
func (s *Server) handleLogin(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.LoginData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}
	if data.Username == "" || data.Password == "" {
		return errResponse(req.ReqID, "invalid credentials")
	}

	// Cache-aside: try Redis creds cache first
	var user *model.User
	if rec, err := s.cache.GetCreds(ctx, data.Username); err == nil {
		user = &model.User{
			ID:           rec.ID,
			Username:     data.Username,
			PasswordHash: rec.PasswordHash,
		}
	} else if errors.Is(err, cache.ErrCacheMiss) {
		user, err = db.GetUserByUsername(ctx, s.pool, data.Username)
		if err != nil {
			// Uniform error to prevent user enumeration
			return errResponse(req.ReqID, "invalid credentials")
		}
		// Fire-and-forget cache populate (creds only — no profile fields)
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.cache.SetCreds(c, data.Username, user)
		}()
	} else {
		// Redis error — fall through to DB
		user, err = db.GetUserByUsername(ctx, s.pool, data.Username)
		if err != nil {
			return errResponse(req.ReqID, "invalid credentials")
		}
	}

	if err := crypto.VerifyPassword(user.PasswordHash, data.Password); err != nil {
		return errResponse(req.ReqID, "invalid credentials")
	}

	// Successful login → create session
	token, err := crypto.SessionToken()
	if err != nil {
		return errResponse(req.ReqID, "internal error")
	}
	sess := &model.Session{
		Token:    token,
		UserID:   user.ID,
		Username: data.Username,
		IP:       data.IP,
	}
	if err := s.cache.CreateSession(ctx, sess, s.cfg.Session.TTL); err != nil {
		return errResponse(req.ReqID, "session store failed")
	}

	return okResponse(req.ReqID, protocol.LoginResult{
		Token:  token,
		UserID: user.ID,
	})
}

// handleLogout invalidates a session.
func (s *Server) handleLogout(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.LogoutData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}
	if data.Token == "" {
		return okResponse(req.ReqID, struct{}{})
	}
	_ = s.cache.DeleteSession(ctx, data.Token)
	return okResponse(req.ReqID, struct{}{})
}

// handleGetUser returns the currently-authenticated user's profile.
func (s *Server) handleGetUser(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.GetUserData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}
	if data.Token == "" {
		return errResponse(req.ReqID, "unauthenticated")
	}

	sess, err := s.cache.GetSession(ctx, data.Token)
	if err != nil {
		return errResponse(req.ReqID, "unauthenticated")
	}

	// Refresh session TTL (sliding window)
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.cache.RefreshSession(c, sess, s.cfg.Session.TTL)
	}()

	// Try user cache first (indexed by user_id)
	if rec, err := s.cache.GetUser(ctx, sess.UserID); err == nil {
		u := &model.User{
			ID:            sess.UserID,
			AvatarVersion: rec.AvatarVersion,
			AvatarSuffix:  rec.AvatarSuffix,
		}
		return okResponse(req.ReqID, protocol.GetUserResult{
			UserID:    sess.UserID,
			Username:  sess.Username,
			Nickname:  rec.Nickname,
			AvatarURL: u.AvatarURL(),
		})
	}

	// Miss → DB
	user, err := db.GetUserByID(ctx, s.pool, sess.UserID)
	if err != nil {
		return errResponse(req.ReqID, "user not found")
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.cache.SetUser(c, user)
	}()
	return okResponse(req.ReqID, protocol.GetUserResult{
		UserID:    user.ID,
		Username:  user.Username,
		Nickname:  user.Nickname,
		AvatarURL: user.AvatarURL(),
	})
}
