// TODO: user-table queries.
//
// Typed query helpers that use the custom pool and prepared statements.
//
// Functions:
//   - GetUserByUsername(ctx, pool, username) (*model.User, error)
//   - GetUserByID(ctx, pool, id) (*model.User, error)
//   - InsertUser(ctx, pool, username, passwordHash, nickname) (int64, error)
//     Handle duplicate-key error → return ErrDuplicateUsername
//   - UpdateNickname(ctx, pool, id, nickname) (newRowVersion uint64, err error)
//   - UpdateAvatar(ctx, pool, id, suffix) (newAvatarVersion uint32, oldRowVersion uint64, err error)
//     Atomic increment + set suffix via LAST_INSERT_ID trick
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
	qGetUserByUsername = `SELECT id, password_hash, nickname, avatar_version, avatar_suffix, row_version
	                      FROM users WHERE username = ?`

	qGetUserByID = `SELECT id, username, nickname, avatar_version, avatar_suffix, row_version
	                FROM users WHERE id = ?`

	qInsertUser = `INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)`

	// row_version rides LAST_INSERT_ID here because nothing else in this
	// statement needs it: a nickname update touches no other atomic counter,
	// so this is the one write path where the trick is free.
	qUpdateNickname = `UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`

	// row_version here is a plain "+1", not LAST_INSERT_ID: avatar_version
	// already claims that mechanism (see UpdateAvatar), and MySQL's
	// LAST_INSERT_ID register holds only one value per statement — the second
	// call would silently overwrite the first. The relative "+1" is still
	// atomic and safe under concurrent writers; UpdateAvatar reads the OLD
	// row_version separately, before this statement, purely for cache
	// invalidation (see its comment for why that read does not need to be
	// exact).
	qUpdateAvatar = `UPDATE users
	                 SET avatar_version = LAST_INSERT_ID(avatar_version + 1),
	                     avatar_suffix  = ?,
	                     row_version    = row_version + 1
	                 WHERE id = ?`

	qGetRowVersion = `SELECT row_version FROM users WHERE id = ?`
)

// GetUserByUsername is the login lookup. It is the only helper that returns the
// password hash, and the hash must never leave the server.
func GetUserByUsername(ctx context.Context, h Handle, username string) (*model.User, error) {
	u := &model.User{Username: username}

	// avatar_suffix is NULL until the first upload. The custom scanner maps a
	// NULL column to the destination's zero value, so this no longer needs the
	// sql.NullString round trip the database/sql version used.
	err := h.QueryRowContext(ctx, qGetUserByUsername, username).
		Scan(&u.ID, &u.PasswordHash, &u.Nickname, &u.AvatarVersion, &u.AvatarSuffix, &u.RowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by username: %w", err)
	}

	return u, nil
}

// GetUserByID backs get_me and profile reads. CreatedAt and UpdatedAt are left
// zero: no read path needs them, and selecting fewer columns keeps the row
// narrow on a query that runs on every authenticated request.
func GetUserByID(ctx context.Context, h Handle, id uint64) (*model.User, error) {
	u := &model.User{}

	err := h.QueryRowContext(ctx, qGetUserByID, id).
		Scan(&u.ID, &u.Username, &u.Nickname, &u.AvatarVersion, &u.AvatarSuffix, &u.RowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by id: %w", err)
	}

	return u, nil
}

// InsertUser creates a user and returns its new id.
//
// The uniqueness of the username is enforced by uk_username, not by a preceding
// SELECT. Checking first would be a race: two concurrent signups for the same
// name can both observe "free" and both proceed. Letting the index reject the
// loser is the only correct check.
func InsertUser(ctx context.Context, h Handle, username string, passwordHash []byte, nickname string) (int64, error) {
	res, err := h.ExecContext(ctx, qInsertUser, username, passwordHash, nickname)
	if err != nil {
		if isDuplicateKey(err) {
			return 0, ErrDuplicateUsername
		}
		return 0, fmt.Errorf("db: insert user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: insert user: last insert id: %w", err)
	}
	return id, nil
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

// UpdateAvatar bumps avatar_version and stores the new filename suffix,
// returning the version the caller should use when writing the file, plus the
// row's row_version from just before this update, for cache invalidation.
//
// The LAST_INSERT_ID(expr) trick makes avatar_version a single atomic
// statement: MySQL evaluates avatar_version+1, stashes it in the connection's
// LAST_INSERT_ID register, and the driver hands it back through LastInsertId.
// A SELECT followed by an UPDATE would let two concurrent uploads compute the
// same version and overwrite each other's file.
//
// row_version cannot ride the same LAST_INSERT_ID call — MySQL's register
// holds one value per statement, and avatar_version already claims it — so
// the OLD row_version is read separately, before the update. That read is
// intentionally not required to be exact under concurrency: if another upload
// for the same user commits in between, this call simply invalidates a
// slightly-stale "old" key, which is either already gone or is about to be
// deleted by that other upload's own call. Either way nothing is left pointing
// at a live key, which is all cache invalidation is required to guarantee.
func UpdateAvatar(ctx context.Context, h Handle, id uint64, suffix string) (newAvatarVersion uint32, oldRowVersion uint64, err error) {
	err = h.QueryRowContext(ctx, qGetRowVersion, id).Scan(&oldRowVersion)
	switch {
	case errors.Is(err, ErrNoRows):
		return 0, 0, ErrUserNotFound
	case err != nil:
		return 0, 0, fmt.Errorf("db: update avatar: read row version: %w", err)
	}

	res, err := h.ExecContext(ctx, qUpdateAvatar, suffix, id)
	if err != nil {
		return 0, 0, fmt.Errorf("db: update avatar: %w", err)
	}

	// Unlike UpdateNickname, RowsAffected is trustworthy here: the version
	// always increments, so a matched row always changes. 0 means no such id.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("db: update avatar: rows affected: %w", err)
	}
	if n == 0 {
		return 0, 0, ErrUserNotFound
	}

	version, err := res.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("db: update avatar: last insert id: %w", err)
	}
	return uint32(version), oldRowVersion, nil
}

// isDuplicateKey reports whether err is a MySQL unique-constraint violation.
// errors.As is used rather than a type assertion so the check still works once
// the error has been wrapped further down.
func isDuplicateKey(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry
}
