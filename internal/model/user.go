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
	if u.AvatarVersion == 0 {
		return DefaultAvatarURL
	}
	return fmt.Sprintf("/uploads/%d/%d/%d_v%d_%s.jpg",
		u.ID>>avatarShardShift, u.ID, u.ID, u.AvatarVersion, u.AvatarSuffix)
}
