// TODO: User model.
//
// Shared type used by db, cache, tcpserver packages.
//
// type User struct {
//     ID            uint64
//     Username      string
//     PasswordHash  []byte     // bcrypt output, 60 bytes
//     Nickname      string
//     AvatarVersion uint32     // 0 = no avatar uploaded
//     AvatarSuffix  string     // 8 hex chars; empty when no avatar
//     CreatedAt     time.Time
//     UpdatedAt     time.Time
// }
//
// Helper: (u *User) AvatarURL() string
//   Returns "/default.png" if AvatarVersion == 0.
//   Otherwise "/uploads/<id>>>14/<id>/<id>_v<version>_<suffix>.jpg"
package model

import (
	"fmt"
	"time"
)

// User mirrors one row of the `users` table (see schema.sql).
type User struct {
	ID            uint64
	Username      string
	PasswordHash  []byte // bcrypt output, 60 bytes; never leaves the server
	Nickname      string
	AvatarVersion uint32 // 0 = no avatar uploaded yet
	AvatarSuffix  string // 8 hex chars; empty when AvatarVersion == 0
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DefaultAvatarURL is served for users who have never uploaded an avatar.
const DefaultAvatarURL = "/default.png"

// avatarShardShift buckets users into directories of 2^14 = 16384 ids each, so
// that no single directory has to hold 10M entries.
const avatarShardShift = 14

// AvatarURL derives the public path of the user's avatar from (ID, version,
// suffix). The path is never stored in MySQL: deriving it keeps the row narrow,
// and makes bumping the version alone sufficient to bust any browser or CDN
// cache without touching the old file.
func (u *User) AvatarURL() string {
	return AvatarURL(u.ID, u.AvatarVersion, u.AvatarSuffix)
}

// AvatarRelPath is the avatar's location relative to the uploads root, using
// forward slashes because the same string is both a URL suffix and (after
// filepath.FromSlash) a filesystem path. Returns "" when there is no avatar.
//
// The extension is always .jpg even for a PNG or WebP upload. That is forced by
// the design: the path must be derivable from (id, version, suffix) alone, and
// the users table has no column to remember which format was uploaded. Browsers
// decode <img> by content rather than by declared type, so this renders
// correctly; it is the reason /uploads/ must send X-Content-Type-Options:
// nosniff, which routes.go does.
func AvatarRelPath(id uint64, version uint32, suffix string) string {
	if version == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d/%d_v%d_%s.jpg", id>>avatarShardShift, id, id, version, suffix)
}

// AvatarURL derives the public path from the three stored fields. Kept as a
// package function as well as a method because the cached profile record holds
// the same three fields without being a full User.
func AvatarURL(id uint64, version uint32, suffix string) string {
	if version == 0 {
		return DefaultAvatarURL
	}
	return "/uploads/" + AvatarRelPath(id, version, suffix)
}

// ---------------------------------------------------------------------------
// Cached projections (phase 5).
//
// Both are deliberately narrower than User. What goes into Redis is only what a
// hit must answer with: caching the whole row would mean re-serialising
// timestamps nothing reads, and would make every added column silently inflate
// every cache entry.
// ---------------------------------------------------------------------------

// UserCached is the value stored under user:<username>, serving the login path.
//
// It includes the bcrypt hash, which is what lets a cache hit verify a password
// without touching MySQL. That is a deliberate trade: the hash is already
// salted and slow to attack, and Redis is on the same trust boundary as the
// database. It is still never sent anywhere near a client.
type UserCached struct {
	ID            uint64 `json:"id"`
	PasswordHash  []byte `json:"password_hash"`
	Nickname      string `json:"nickname"`
	AvatarVersion uint32 `json:"avatar_version"`
	AvatarSuffix  string `json:"avatar_suffix,omitempty"`
}

// ProfileCached is the value stored under profile:<user_id>, serving get_user.
//
// Username is absent on purpose: the caller already has it from the session, so
// storing it here would duplicate a field that can then go stale independently.
type ProfileCached struct {
	Nickname      string `json:"nickname"`
	AvatarVersion uint32 `json:"avatar_version"`
	AvatarSuffix  string `json:"avatar_suffix,omitempty"`
}

// CachedUser projects a row into the login cache value.
func (u *User) CachedUser() *UserCached {
	return &UserCached{
		ID:            u.ID,
		PasswordHash:  u.PasswordHash,
		Nickname:      u.Nickname,
		AvatarVersion: u.AvatarVersion,
		AvatarSuffix:  u.AvatarSuffix,
	}
}

// CachedProfile projects a row into the profile cache value.
func (u *User) CachedProfile() *ProfileCached {
	return &ProfileCached{
		Nickname:      u.Nickname,
		AvatarVersion: u.AvatarVersion,
		AvatarSuffix:  u.AvatarSuffix,
	}
}

// User rehydrates a full User from a login-cache hit. The username is supplied
// by the caller because it was the lookup key.
func (c *UserCached) User(username string) *User {
	return &User{
		ID:            c.ID,
		Username:      username,
		PasswordHash:  c.PasswordHash,
		Nickname:      c.Nickname,
		AvatarVersion: c.AvatarVersion,
		AvatarSuffix:  c.AvatarSuffix,
	}
}

// AvatarURL derives the avatar path for a cached profile, given its user id.
func (p *ProfileCached) AvatarURL(id uint64) string {
	return AvatarURL(id, p.AvatarVersion, p.AvatarSuffix)
}
