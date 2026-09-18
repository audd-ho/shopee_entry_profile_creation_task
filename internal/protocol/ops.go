package protocol

// Op-codes used in Request.Op.
const (
	OpSignup         = "signup"
	OpLogin          = "login"
	OpLogout         = "logout"
	OpGetUser        = "get_user"
	OpUpdateNickname = "update_nickname"
	OpUpdateAvatar   = "update_avatar"
)

// Response statuses.
const (
	StatusOK  = "ok"
	StatusErr = "err"
)

// --- Op-specific data payloads ---

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

type UpdateAvatarResult struct {
	AvatarURL string `json:"avatar_url"`
	Version   uint32 `json:"version"`
}
