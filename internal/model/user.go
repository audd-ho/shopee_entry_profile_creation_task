// Package model contains shared data types used across packages.
package model

import (
	"fmt"
	"time"
)

// User represents a row in the users table.
type User struct {
	ID            uint64
	Username      string
	PasswordHash  []byte
	Nickname      string
	AvatarVersion uint32
	AvatarSuffix  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AvatarURL returns the URL path to serve for this user's avatar.
// Returns "/default-avatar.svg" if the user has not uploaded an avatar.
// Otherwise: /uploads/<user_id >> 14>/<user_id>/<user_id>_v<version>_<suffix>.jpg
func (u *User) AvatarURL() string {
	if u.AvatarVersion == 0 || u.AvatarSuffix == "" {
		return "/default-avatar.svg"
	}
	bucket := u.ID >> 14
	return fmt.Sprintf("/uploads/%d/%d/%d_v%d_%s.jpg",
		bucket, u.ID, u.ID, u.AvatarVersion, u.AvatarSuffix)
}

// AvatarPath returns the filesystem path (relative to uploads root) for this user's avatar.
// Only meaningful when AvatarVersion > 0.
func (u *User) AvatarPath() string {
	bucket := u.ID >> 14
	return fmt.Sprintf("%d/%d/%d_v%d_%s.jpg",
		bucket, u.ID, u.ID, u.AvatarVersion, u.AvatarSuffix)
}
