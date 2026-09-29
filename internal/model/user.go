// TODO: User model.
//
// Shared type used by db, cache, tcpserver packages.
//
// type User struct {
//     ID            uint64
//     Username      string
//     PasswordHash  []byte     // bcrypt output, 60 bytes
//     Nickname      string
//     AvatarSuffix  string     // 8 hex chars; empty when no avatar
//     CreatedAt     time.Time
//     UpdatedAt     time.Time
// }
//
// Helper: (u *User) AvatarURL() string
//   Returns "/default.png" if AvatarSuffix == "".
//   Otherwise "/uploads/<id>>>14/<id>/<id>_<suffix>.jpg"
package model

import (
	"fmt"
	"time"
)

// User mirrors one row of the `users` table (see schema.sql).
type User struct {
	ID           uint64
	Username     string
	PasswordHash []byte // bcrypt output, 60 bytes; never leaves the server
	Nickname     string

	// AvatarSuffix is empty until the first upload, and is the sole signal for
	// whether one exists — there is no separate avatar_version column. See the
	// package comment above AvatarRelPath for why a version column existed
	// once and was deliberately removed.
	AvatarSuffix string // 8 hex chars; empty when no avatar has been uploaded

	// RowVersion bumps on any mutating write (nickname update or avatar
	// upload). It exists purely to build/tear down the Redis keys that cache
	// this row (system_design.md §4.1, §4.4.4) — it is never used in a
	// filename or path, and never shown to a client.
	RowVersion uint64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultAvatarURL is served for users who have never uploaded an avatar.
const DefaultAvatarURL = "/default.png"

// avatarShardShift buckets users into directories of 2^14 = 16384 ids each, so
// that no single directory has to hold 10M entries.
const avatarShardShift = 14

// AvatarSweepGracePeriod is the minimum file age a future orphan sweeper must
// require before deleting anything under the uploads root, regardless of
// whether the file currently matches what MySQL points at.
//
// Why this exists: the upload flow (tcpserver/profile.go:handleUpdateAvatar)
// writes the file BEFORE updating the users row, specifically so a failed or
// crashed write never leaves the database pointing at a file that does not
// exist (see AvatarRelPath's package comment for the full trade-off against
// the version-based scheme this replaced). That ordering means a brand-new
// file can legitimately sit on disk for a brief window — the time between the
// rename landing and the UPDATE committing — during which nothing in MySQL
// references it yet. A sweeper that only asks "does the DB currently point at
// this file?" would see exactly that and correctly, but wrongly, conclude the
// file is an orphan; deleting it there is worse than the crash this ordering
// was chosen to avoid, because it requires no crash at all, just an unlucky
// sweep tick, and at 10M-user scale an event that CAN happen on any given
// upload WILL happen periodically.
//
// The fix is for the sweeper to skip anything younger than this, checked via
// the file's mtime, before ever consulting the database. Ten minutes is two
// to three orders of magnitude longer than an upload plus its UPDATE could
// plausibly take, so it costs nothing in practice while making the race
// structurally impossible rather than merely unlikely.
//
// No sweeper exists yet (see system_design.md §4.4.4, "Orphan cleanup") —
// this constant is here so whichever one is written next cannot miss the
// requirement by skipping the design doc.
const AvatarSweepGracePeriod = 10 * time.Minute

// AvatarURL derives the public path of the user's avatar from (ID, suffix).
// The path is never stored in MySQL: deriving it keeps the row narrow, and a
// fresh random suffix on every upload is what busts any browser or CDN cache
// without touching the previous file.
func (u *User) AvatarURL() string {
	return AvatarURL(u.ID, u.AvatarSuffix)
}

// AvatarRelPath is the avatar's location relative to the uploads root, using
// forward slashes because the same string is both a URL suffix and (after
// filepath.FromSlash) a filesystem path. Returns "" when there is no avatar.
//
// There is deliberately no version number in this path, even though an
// earlier iteration of this design had one. Dropping it changes which write
// order is possible: a version had to come from an atomic MySQL increment,
// which forced the database write to happen BEFORE the file existed — so a
// crash between the two left the row pointing at a version whose file was
// never written, a broken avatar with no rollback. A suffix alone is
// generated in Go with no DB round trip, so the file can be written FIRST and
// the database updated second; a failure at either step now either leaves an
// untouched, still-valid row (DB step never ran) or a harmless orphan file
// (DB step ran, nothing yet points at the orphan). See
// tcpserver/profile.go:handleUpdateAvatar for where that ordering lives, and
// AvatarSweepGracePeriod above for the new race this ordering opens up for any
// future cleanup job.
//
// The extension is always .jpg even for a PNG or WebP upload. That is forced
// by the design: the path must be derivable from (id, suffix) alone, and the
// users table has no column to remember which format was uploaded. Browsers
// decode <img> by content rather than by declared type, so this renders
// correctly; it is the reason /uploads/ must send X-Content-Type-Options:
// nosniff, which routes.go does.
func AvatarRelPath(id uint64, suffix string) string {
	if suffix == "" {
		return ""
	}
	return fmt.Sprintf("%d/%d/%d_%s.jpg", id>>avatarShardShift, id, id, suffix)
}

// AvatarURL derives the public path from the stored fields. Kept as a package
// function as well as a method because the cached profile record holds the
// same fields without being a full User.
func AvatarURL(id uint64, suffix string) string {
	if suffix == "" {
		return DefaultAvatarURL
	}
	return "/uploads/" + AvatarRelPath(id, suffix)
}

// ---------------------------------------------------------------------------
// Cached projections (phase 5).
//
// Both are deliberately narrower than User. What goes into Redis is only what a
// hit must answer with: caching the whole row would mean re-serialising
// timestamps nothing reads, and would make every added column silently inflate
// every cache entry.
// ---------------------------------------------------------------------------

// AuthCached is the value stored under auth:<username>, serving the login
// path only. It carries nothing beyond what verifying a password and minting
// a session need — no nickname, no avatar, and (as of this design) no
// row_version either. Nickname/avatar live in UserCached instead, fetched
// separately once a session exists.
//
// It includes the bcrypt hash, which is what lets a cache hit verify a
// password without touching MySQL. That is a deliberate trade: the hash is
// already salted and slow to attack, and Redis is on the same trust boundary
// as the database. It is still never sent anywhere near a client.
//
// row_version is deliberately NOT cached here. An earlier version of this
// struct carried it as a hint for seeding a new session's cache-version
// guess, but that hint could only ever be as fresh as the last time this
// entry was populated — and nothing invalidates auth:<username> on a
// nickname/avatar edit (there is nothing left in it that a profile edit could
// make stale), so the hint could go stale for the entirety of its TTL. The
// login path now reads row_version fresh on every login instead (see
// tcpserver/auth.go:lookupUserForLogin) — one extra indexed SELECT on an
// auth-cache hit, none at all on a miss, since the miss-path query already
// returns it. That trades a small, deliberate, constant cost on login for a
// session's very first cache-version guess never being wrong, rather than
// carrying a value here that would just be a guess by another name.
//
// Unlike user:<user_id> (see UserCached below), this key is NOT versioned —
// login has no prior request to seed a version guess from (there is no
// session yet; that is what login creates), so a reader would need either a
// second Redis round trip or a MySQL read just to learn which versioned key
// to GET, defeating the point of caching the hot path that matters most.
//
// ID and PasswordHash are both immutable in this service's current feature
// set (no change-password endpoint exists), which is why AuthCached needs no
// invalidation path at all today beyond its TTL — see cache/invalidate.go.
type AuthCached struct {
	ID           uint64 `json:"id"`
	PasswordHash []byte `json:"password_hash"`
}

// UserCached is the value stored under user:<user_id>:v<row_version>,
// serving get_user. This is the general per-user record everything AFTER
// login reads from — nickname, avatar, and (unlike AuthCached) username,
// since a cache hit here has no session field to fall back on for it.
type UserCached struct {
	Username     string `json:"username"`
	Nickname     string `json:"nickname"`
	AvatarSuffix string `json:"avatar_suffix,omitempty"`
}

// CachedAuth projects a row into the auth cache value.
func (u *User) CachedAuth() *AuthCached {
	return &AuthCached{
		ID:           u.ID,
		PasswordHash: u.PasswordHash,
	}
}

// CachedUser projects a row into the user cache value.
func (u *User) CachedUser() *UserCached {
	return &UserCached{
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarSuffix: u.AvatarSuffix,
	}
}

// User rehydrates a full User from an auth-cache hit. The username is
// supplied by the caller because it was the lookup key. RowVersion is left
// zero — the login path always fills it in separately with a fresh read; see
// AuthCached's comment for why.
func (c *AuthCached) User(username string) *User {
	return &User{
		ID:           c.ID,
		Username:     username,
		PasswordHash: c.PasswordHash,
	}
}

// AvatarURL derives the avatar path for a cached user record, given its id.
func (c *UserCached) AvatarURL(id uint64) string {
	return AvatarURL(id, c.AvatarSuffix)
}
