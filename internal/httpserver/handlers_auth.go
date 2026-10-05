// TODO: Auth-related HTTP handlers.
//
// Handlers:
//
//   - Signup(w, r)
//     Parse {username, password, nickname} from JSON body
//     Validate via internal/validate
//     Forward to TCP server op=OpSignup
//     Return 201 on success, 4xx on validation/duplicate
//
//   - Login(w, r)
//     Parse {username, password} from JSON body
//     Forward to TCP server op=OpLogin
//     On success: set session cookie, return 200 with user info
//     On failure: return 401 (uniform message, don't reveal user existence)
//
//   - Logout(w, r)
//     Read session token from cookie
//     Forward to TCP server op=OpLogout (invalidates session in Redis)
//     Clear cookie, return 204
//
//   - Me(w, r)
//     Auth already checked by middleware; user_id + username in context
//     Forward to TCP server op=OpGetMe with user_id
//     Return 200 with {username, nickname, avatar_url}
//     avatar_url is derived: /uploads/<id>>>14/<id>/<id>_v<n>_<suffix>.jpg,
//     or /default.png if avatar_version=0
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// backendTimeout bounds a single call to the TCP server (HTTP_BACKEND_TIMEOUT_MS,
// default 5s). Login is the slow one at roughly one bcrypt (~75ms), so this is
// generous on purpose. It must exceed the TCP server's own per-request timeout.
func (s *Server) backendTimeout() time.Duration { return s.cfg.HTTP.BackendTimeout }

const (
	// maxJSONBody caps auth request bodies. Avatar uploads do not come through
	// here; they are multipart and get their own, larger limit.
	maxJSONBody = 64 << 10
)

// errorStatus translates a backend error code into an HTTP status. The TCP
// server decides what went wrong; this table is the only place that decides
// what that means over HTTP.
var errorStatus = map[string]int{
	string(protocol.ErrCodeInvalidInput):       http.StatusBadRequest,
	string(protocol.ErrCodeDuplicateUsername):  http.StatusConflict,
	string(protocol.ErrCodeInvalidCredentials): http.StatusUnauthorized,
	string(protocol.ErrCodeUnauthorized):       http.StatusUnauthorized,
	string(protocol.ErrCodeInternal):           http.StatusInternalServerError,
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already on the wire, so there is nothing to tell the
		// client. Log and move on.
		log.Printf("write json response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSONBody reads a bounded JSON body and rejects unknown fields, so a
// typo'd field name is a visible 400 rather than a silently ignored value.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// backendError turns a failed pool.Call into an HTTP response.
//
// pool.Call surfaces the backend's Response.Error verbatim as an error string,
// so a known error code maps to its status and anything else is a transport or
// internal failure that the client has no business seeing the details of.
func backendError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("%s: backend timed out: %v", op, err)
		writeError(w, http.StatusGatewayTimeout, "backend timeout")
	default:
		if status, ok := errorStatus[err.Error()]; ok {
			writeError(w, status, err.Error())
			return
		}
		log.Printf("%s: backend call failed: %v", op, err)
		writeError(w, http.StatusBadGateway, "backend unavailable")
	}
}

// clientIP strips the port from RemoteAddr. Stored on the session for anomaly
// detection only — it is never used for authorisation, because behind a proxy
// it is the proxy's address and X-Forwarded-For is client-controlled.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleSignup creates an account. POST /api/signup
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	// Validating here gives the user a specific message ("username must be
	// 3-32 characters") and rejects junk before it costs a TCP round trip and a
	// 75ms bcrypt. The TCP server validates again regardless, because this
	// server is not a trust boundary for it.
	if err := validate.Username(body.Username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validate.Password(body.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Nickname == "" {
		body.Nickname = body.Username // sensible default, same as the seeder
	}
	nickname, err := validate.Nickname(body.Nickname)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	var result protocol.SignupResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpSignup, &protocol.SignupData{
		Username: body.Username,
		Password: body.Password,
		Nickname: nickname,
	}, &result); err != nil {
		backendError(w, "signup", err)
		return
	}

	// Signup does not log the user in. Keeping the two separate means there is
	// exactly one code path that issues a session.
	writeJSON(w, http.StatusCreated, map[string]any{"user_id": result.UserID})
}

// handleLogin authenticates and issues a session cookie. POST /api/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	// Note the asymmetry with signup: login only checks for emptiness, it does
	// not run the validators. An account created before a rule tightened must
	// still be able to log in, and rejecting a password for being "too short"
	// at login would leak the current policy and waste a round trip telling an
	// attacker their guess was malformed rather than wrong.
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusUnauthorized, string(protocol.ErrCodeInvalidCredentials))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	var result protocol.LoginResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpLogin, &protocol.LoginData{
		Username: body.Username,
		Password: body.Password,
		IP:       clientIP(r),
	}, &result); err != nil {
		backendError(w, "login", err)
		return
	}

	SetSessionCookie(w, result.Token, s.cfg.Session.TTL)

	// The token goes in the cookie and nowhere else. Returning it in the body
	// too would invite the frontend to stash it in localStorage, which undoes
	// the HttpOnly protection the cookie exists for.
	writeJSON(w, http.StatusOK, map[string]any{"user_id": result.UserID})
}

// handleLogout revokes the session. POST /api/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)

	// Clear the cookie first and unconditionally. Even if the backend call
	// fails, the browser must stop presenting a token the user asked to drop.
	ClearSessionCookie(w)

	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	var result protocol.LogoutResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpLogout, &protocol.LogoutData{Token: token}, &result); err != nil {
		// Logged, not surfaced: the cookie is gone, so from the user's point of
		// view logout succeeded. The Redis key expires on its own TTL.
		log.Printf("logout: backend call failed: %v", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGetUser returns the current user's profile. GET /api/user
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	var result protocol.GetUserResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpGetUser, &protocol.GetUserData{
		Token: sessionToken(r),
	}, &result); err != nil {
		// A rejected token means the cookie is stale — expired, or revoked from
		// another device. Clearing it stops the browser replaying it forever.
		if err.Error() == string(protocol.ErrCodeUnauthorized) {
			ClearSessionCookie(w)
		}
		backendError(w, "get_user", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}
