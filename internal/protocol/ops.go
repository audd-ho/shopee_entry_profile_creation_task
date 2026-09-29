// TODO: Op-code constants for the HTTP↔TCP protocol.
//
// String constants used in Request.Op to identify which handler on the TCP server
// should process the request. Kept in one place to avoid typos.
//
// Ops:
//
//	OpSignup         = "signup"
//	OpLogin          = "login"
//	OpLogout         = "logout"
//	OpGetMe          = "get_me"           // fetch current user info (for /api/me)
//	OpUpdateNickname = "update_nickname"
//	OpUpdateAvatar   = "update_avatar"
//
// Response statuses:
//
//	StatusOK  = "ok"
//	StatusErr = "err"
package protocol

// definig the different ops possible

// Op-codes used in Request.Op.
const (
	OpSignup         = "signup"
	OpLogin          = "login"
	OpLogout         = "logout"
	OpGetUser        = "get_user"
	OpUpdateNickname = "update_nickname"
	OpUpdateAvatar   = "update_avatar"

	OpPing = "ping"
)

// Response statuses.
const (
	StatusOK  = "ok"
	StatusErr = "err"
)

// --- Op-specific data payloads ---

type PingReq struct {
	Hi string `json:"hi"`
}

type PingResp struct {
	Bye string `json:"bye"`
}

type SignupData struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

type SignupResult struct {
	UserID uint64 `json:"user_id"`
}

type LoginData struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IP       string `json:"ip,omitempty"`
}

// LoginResult is intentionally minimal: login only authenticates and issues a session.
// The frontend calls /api/user afterward to retrieve profile info.
type LoginResult struct {
	Token  string `json:"token"`
	UserID uint64 `json:"user_id"`
}

type LogoutData struct {
	Token string `json:"token"`
}

// LogoutResult is empty but must exist: a handler returning nil data produces a
// response with no Data field, and the client's Call would have nothing to
// unmarshal into.
type LogoutResult struct{}

type GetUserData struct {
	Token string `json:"token"`
}

type GetUserResult struct {
	UserID    uint64 `json:"user_id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

type UpdateNicknameData struct {
	Token    string `json:"token"`
	Nickname string `json:"nickname"`
}

type UpdateNicknameResult struct {
	Nickname string `json:"nickname"`
}

type UpdateAvatarData struct {
	Token    string `json:"token"`
	ImageB64 string `json:"image_b64"` // base64-encoded image bytes
}

// UpdateAvatarResult carries only the URL. There is no version number to
// report: the design that had one (an avatar_version column, embedded in the
// filename) was replaced by a bare random suffix — see
// model.AvatarRelPath's comment for why — and nothing else in this service
// needs a monotonic count of how many times a user has uploaded.
type UpdateAvatarResult struct {
	AvatarURL string `json:"avatar_url"`
}

// --- Error codes ---
//
// Response.Error carries one of these codes rather than a prose message. The
// TCP server decides what went wrong, the HTTP server decides what that means
// in HTTP terms, and neither has to parse the other's English. Anything not on
// this list is a bug and gets reported as ErrCodeInternal.
//
// ErrorCode implements error so handlers can `return nil, ErrCodeUnauthorized`
// directly, and dispatch can tell a deliberate code from a leaked internal
// error with errors.As.
type ErrorCode string

func (e ErrorCode) Error() string { return string(e) }

const (
	// ErrCodeInvalidInput: the payload failed validation on the server side.
	ErrCodeInvalidInput ErrorCode = "invalid_input"

	// ErrCodeDuplicateUsername: signup lost the race on uk_username.
	ErrCodeDuplicateUsername ErrorCode = "duplicate_username"

	// ErrCodeInvalidCredentials: unknown username OR wrong password. The two
	// are deliberately indistinguishable so the endpoint cannot be used to
	// enumerate which accounts exist.
	ErrCodeInvalidCredentials ErrorCode = "invalid_credentials"

	// ErrCodeUnauthorized: missing, unknown or expired session token.
	ErrCodeUnauthorized ErrorCode = "unauthorized"

	// ErrCodeInternal: anything else. Details stay in the server log.
	ErrCodeInternal ErrorCode = "internal"
)
