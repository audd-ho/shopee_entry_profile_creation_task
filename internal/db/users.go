// TODO: user-table queries.
//
// Typed query helpers that use the custom pool and prepared statements.
//
// Functions:
//   - GetUserByUsername(ctx, pool, username) (*model.User, error)
//   - GetUserByID(ctx, pool, id) (*model.User, error)
//   - GetRowVersion(ctx, pool, id) (uint64, error)
//   - InsertUser(ctx, pool, username, passwordHash, nickname) (int64, error)
//     Handle duplicate-key error → return ErrDuplicateUsername
//   - UpdateNickname(ctx, pool, id, nickname) (newRowVersion uint64, err error)
//   - UpdateAvatar(ctx, pool, id, suffix) (newRowVersion uint64, err error)
//     Set suffix + atomic row_version increment via LAST_INSERT_ID trick
//
// All queries use parameterized SQL (?)  — never string-concat.
package db

// ---------------------------------------------------------------------------
// Phase 4: now running on the custom pool.
//
// Note how little moved. The Handle alias below was re-pointed from *sql.DB to
// *Pool, Pool grew ExecContext/QueryRowContext with matching shapes, and the
// query bodies are otherwise unchanged. That was the point of the phase 2
// structure: the function signatures are the contract, the bodies are not.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/model"
)

// Handle is the database handle every query helper in this package takes.
//
// It is a type alias rather than an interface. database/sql's *sql.Row and
// *sql.Rows are concrete structs, so an interface written in terms of them
// could never be satisfied by anything else — which is exactly why this became
// an alias swap in phase 4 rather than an interface implementation.
type Handle = *Pool

// Sentinel errors. Callers in tcpserver must never see a driver-level or
// database/sql error: sql.ErrNoRows and MySQL error numbers are translated
// here so that the handlers stay ignorant of which database sits underneath.
var (
	ErrUserNotFound      = errors.New("db: user not found")
	ErrDuplicateUsername = errors.New("db: duplicate username")
)

// mysqlErrDupEntry is MySQL's ER_DUP_ENTRY, raised when an INSERT collides with
// a UNIQUE index — here, uk_username.
const mysqlErrDupEntry = 1062

// connMaxLifetime caps how long a pooled connection is reused. Bounded
// lifetimes let server-side restarts, failovers and wait_timeout drift resolve
// themselves instead of surfacing as sporadic "invalid connection" errors.
const connMaxLifetime = 5 * time.Minute

// Open builds the pool and verifies it can reach MySQL.
func Open(ctx context.Context, cfg *config.MySQLConfig) (Handle, error) {
	p, err := New(cfg)
	if err != nil {
		return nil, err
	}

	// The pool dials lazily, so without an explicit ping a wrong password or a
	// down server would first surface on some unlucky user request rather than
	// at boot.
	if err := p.PingContext(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

// All SQL is parameterised with `?`. No query in this package is ever built by
// string concatenation, which is what keeps SQL injection structurally
// impossible rather than merely unlikely.
const (
	// Trimmed to exactly what login uses: nickname/avatar_suffix used to ride
	// along here too, back when the login cache carried them. Now that
	// auth:<username> (model.AuthCached) is login-only, this query is
	// login-only as well — see GetUserByUsername's own comment.
	qGetUserByUsername = `SELECT id, password_hash, row_version
	                      FROM users WHERE username = ?`

	qGetUserByID = `SELECT id, username, nickname, avatar_suffix, row_version
	                FROM users WHERE id = ?`

	// qGetRowVersion backs the login path's fresh-every-time read (see
	// GetRowVersion) — used only when the auth cache is a HIT, since a MISS's
	// qGetUserByUsername above already returns row_version in the same query.
	qGetRowVersion = `SELECT row_version FROM users WHERE id = ?`

	qInsertUser = `INSERT INTO users (id, username, password_hash, nickname) VALUES (?, ?, ?, ?)`

	// row_version rides LAST_INSERT_ID here because nothing else in this
	// statement needs an atomic counter — there is no avatar_version column
	// competing for MySQL's one LAST_INSERT_ID register per statement (see
	// model.AvatarRelPath's comment for why that column was removed).
	qUpdateNickname = `UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`

	// Same trick as qUpdateNickname, for the same reason: with avatar_version
	// gone, row_version is the only atomic counter this statement touches, so
	// nothing stops it from using LAST_INSERT_ID directly. UpdateAvatar no
	// longer needs a pre-read to learn the old row_version the way it once
	// did when avatar_version and row_version were both competing for that
	// one register.
	qUpdateAvatar = `UPDATE users SET avatar_suffix = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`
)

// GetUserByUsername is the login-path lookup: the only helper that returns
// the password hash, which must never leave the server. It backs the auth
// cache's MISS path only, so it returns exactly what login needs — id,
// password_hash, row_version — and leaves Nickname/AvatarSuffix on the
// returned User unset (zero value). Nothing on the login path reads them;
// callers that need nickname or avatar use GetUserByID instead, after a
// session exists.
func GetUserByUsername(ctx context.Context, h Handle, username string) (*model.User, error) {
	u := &model.User{Username: username}

	err := h.QueryRowContext(ctx, qGetUserByUsername, username).
		Scan(&u.ID, &u.PasswordHash, &u.RowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by username: %w", err)
	}

	return u, nil
}

// GetRowVersion reads a single row's current row_version by primary key.
//
// This backs the login path's auth-cache HIT case (see
// tcpserver/auth.go:lookupUserForLogin): AuthCached does not carry
// row_version at all (see model.AuthCached's comment), so a hit has nothing
// to read it from, and this is the deliberate, accepted extra cost of always
// seeding a session's cache-version hint fresh rather than from a guess. A
// single indexed SELECT by primary key — cheap, and the only case that pays
// it, since a MISS already gets row_version for free from
// GetUserByUsername's own query.
func GetRowVersion(ctx context.Context, h Handle, id uint64) (uint64, error) {
	var rowVersion uint64
	err := h.QueryRowContext(ctx, qGetRowVersion, id).Scan(&rowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return 0, ErrUserNotFound
	case err != nil:
		return 0, fmt.Errorf("db: get row version: %w", err)
	}
	return rowVersion, nil
}

// GetUserByID backs get_me and profile reads. CreatedAt and UpdatedAt are left
// zero: no read path needs them, and selecting fewer columns keeps the row
// narrow on a query that runs on every authenticated request.
func GetUserByID(ctx context.Context, h Handle, id uint64) (*model.User, error) {
	u := &model.User{}

	err := h.QueryRowContext(ctx, qGetUserByID, id).
		Scan(&u.ID, &u.Username, &u.Nickname, &u.AvatarSuffix, &u.RowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by id: %w", err)
	}

	return u, nil
}

// InsertUser creates a user with an application-generated snowflake ID.
//
// The ID is generated by the caller (idgen.Generator.Next) before the INSERT —
// no AUTO_INCREMENT, no LastInsertId. Global username uniqueness is enforced by
// the lookup table (InsertLookup); the per-shard uk_username is a safety net.
func InsertUser(ctx context.Context, h Handle, id uint64, username string, passwordHash []byte, nickname string) error {
	_, err := h.ExecContext(ctx, qInsertUser, id, username, passwordHash, nickname)
	if err != nil {
		if isDuplicateKey(err) {
			return ErrDuplicateUsername
		}
		return fmt.Errorf("db: insert user: %w", err)
	}
	return nil
}

// UpdateNickname sets a new nickname and returns the row's new row_version,
// so the caller can invalidate the cache key the OLD version stamped
// (system_design.md §4.1) without a separate read.
//
// It deliberately does not inspect RowsAffected to detect a missing user: MySQL
// reports rows *changed*, not rows *matched*, so submitting the nickname you
// already have yields 0 and is indistinguishable from "no such id". Callers
// reach this path with an id taken from a validated session, so the user exists
// by construction. row_version is exempt from that ambiguity: LAST_INSERT_ID
// always returns what THIS statement just set, whether or not the nickname
// itself changed, since row_version = row_version + 1 is unconditional.
func UpdateNickname(ctx context.Context, h Handle, id uint64, nickname string) (uint64, error) {
	res, err := h.ExecContext(ctx, qUpdateNickname, nickname, id)
	if err != nil {
		return 0, fmt.Errorf("db: update nickname: %w", err)
	}
	newRowVersion, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: update nickname: last insert id: %w", err)
	}
	return uint64(newRowVersion), nil
}

// UpdateAvatar stores a new filename suffix and returns the row's new
// row_version, for the caller to invalidate the cache key the OLD version
// stamped (system_design.md §4.1).
//
// Called AFTER the file at that suffix has already been written to disk —
// see tcpserver/profile.go:handleUpdateAvatar and model.AvatarRelPath's
// comment for why the write order changed from the DB-first scheme this once
// was. There is no avatar_version to bump here any more, which is what makes
// row_version safe to fetch directly off LAST_INSERT_ID: with only one atomic
// counter in this statement, nothing else is competing for MySQL's
// one-per-statement LAST_INSERT_ID register, so unlike the old version of
// this function there is no need for a separate pre-read.
//
// RowsAffected is trustworthy for detecting a missing id, the same reasoning
// UpdateNickname's comment explains it is NOT trustworthy for that on its
// own: row_version always increments, so a matched row always changes, and 0
// rows changed can only mean 0 rows matched.
func UpdateAvatar(ctx context.Context, h Handle, id uint64, suffix string) (newRowVersion uint64, err error) {
	res, err := h.ExecContext(ctx, qUpdateAvatar, suffix, id)
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: rows affected: %w", err)
	}
	if n == 0 {
		return 0, ErrUserNotFound
	}

	newRV, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: last insert id: %w", err)
	}
	return uint64(newRV), nil
}

// isDuplicateKey reports whether err is a MySQL unique-constraint violation.
// errors.As is used rather than a type assertion so the check still works once
// the error has been wrapped further down.
func isDuplicateKey(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry
}
