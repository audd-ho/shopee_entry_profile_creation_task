// TODO: user-table queries.
//
// Typed query helpers that use the custom pool and prepared statements.
//
// Functions:
//   - GetUserByUsername(ctx, pool, username) (*model.User, error)
//   - GetUserByID(ctx, pool, id) (*model.User, error)
//   - InsertUser(ctx, pool, username, passwordHash, nickname) (int64, error)
//       Handle duplicate-key error → return ErrDuplicateUsername
//   - UpdateNickname(ctx, pool, id, nickname) error
//   - UpdateAvatar(ctx, pool, id, suffix) (newVersion uint32, err error)
//       Atomic increment + set suffix via LAST_INSERT_ID trick
//
// All queries use parameterized SQL (?)  — never string-concat.
package db

// ---------------------------------------------------------------------------
// Phase 2 implementation: stock database/sql.
//
// The custom pool referenced above lands in phase 4. Until then the handle is
// a *sql.DB and these helpers are the seam that the swap happens behind: the
// function signatures are the contract callers depend on, the bodies are not.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/model"
)

// Handle is the database handle every query helper in this package takes.
//
// It is deliberately a type alias rather than an interface. database/sql's
// *sql.Row and *sql.Rows are concrete structs, not interfaces, so a custom pool
// could never satisfy an interface written in terms of them. In phase 4 this
// alias is re-pointed at *Pool and the bodies below are rewritten; every caller
// in tcpserver keeps compiling untouched.
type Handle = *sql.DB

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

// Open dials MySQL and returns a ready, verified handle.
func Open(ctx context.Context, cfg *config.MySQLConfig) (Handle, error) {
	h, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}

	h.SetMaxOpenConns(cfg.MaxOpen)
	// Idle is deliberately equal to open. The default is 2, which would have
	// the pool dialling and tearing down connections continuously as soon as
	// concurrency exceeded 2 — the cost shows up as latency, not as an error.
	h.SetMaxIdleConns(cfg.MaxOpen)
	h.SetConnMaxLifetime(connMaxLifetime)

	// sql.Open is lazy: it only parses the DSN, it does not connect. Without an
	// explicit ping, a wrong password or a down server first surfaces on some
	// unlucky user request rather than at boot.
	if err := h.PingContext(ctx); err != nil {
		h.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return h, nil
}

// All SQL is parameterised with `?`. No query in this package is ever built by
// string concatenation, which is what keeps SQL injection structurally
// impossible rather than merely unlikely.
const (
	qGetUserByUsername = `SELECT id, password_hash, nickname, avatar_version, avatar_suffix
	                      FROM users WHERE username = ?`

	qGetUserByID = `SELECT id, username, nickname, avatar_version, avatar_suffix
	                FROM users WHERE id = ?`

	qInsertUser = `INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)`

	qUpdateNickname = `UPDATE users SET nickname = ? WHERE id = ?`

	qUpdateAvatar = `UPDATE users
	                 SET avatar_version = LAST_INSERT_ID(avatar_version + 1), avatar_suffix = ?
	                 WHERE id = ?`
)

// GetUserByUsername is the login lookup. It is the only helper that returns the
// password hash, and the hash must never leave the server.
func GetUserByUsername(ctx context.Context, h Handle, username string) (*model.User, error) {
	u := &model.User{Username: username}
	// avatar_suffix is NULL until the first upload, so it cannot scan straight
	// into a string.
	var suffix sql.NullString

	err := h.QueryRowContext(ctx, qGetUserByUsername, username).
		Scan(&u.ID, &u.PasswordHash, &u.Nickname, &u.AvatarVersion, &suffix)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by username: %w", err)
	}

	u.AvatarSuffix = suffix.String
	return u, nil
}

// GetUserByID backs get_me and profile reads. CreatedAt and UpdatedAt are left
// zero: no read path needs them, and selecting fewer columns keeps the row
// narrow on a query that runs on every authenticated request.
func GetUserByID(ctx context.Context, h Handle, id uint64) (*model.User, error) {
	u := &model.User{}
	var suffix sql.NullString

	err := h.QueryRowContext(ctx, qGetUserByID, id).
		Scan(&u.ID, &u.Username, &u.Nickname, &u.AvatarVersion, &suffix)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("db: get user by id: %w", err)
	}

	u.AvatarSuffix = suffix.String
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

// UpdateNickname sets a new nickname.
//
// It deliberately does not inspect RowsAffected to detect a missing user: MySQL
// reports rows *changed*, not rows *matched*, so submitting the nickname you
// already have yields 0 and is indistinguishable from "no such id". Callers
// reach this path with an id taken from a validated session, so the user exists
// by construction.
func UpdateNickname(ctx context.Context, h Handle, id uint64, nickname string) error {
	if _, err := h.ExecContext(ctx, qUpdateNickname, nickname, id); err != nil {
		return fmt.Errorf("db: update nickname: %w", err)
	}
	return nil
}

// UpdateAvatar bumps avatar_version and stores the new filename suffix,
// returning the version the caller should use when writing the file.
//
// The LAST_INSERT_ID(expr) trick makes this a single atomic statement: MySQL
// evaluates avatar_version+1, stashes it in the connection's LAST_INSERT_ID
// register, and the driver hands it back through LastInsertId. A SELECT
// followed by an UPDATE would let two concurrent uploads compute the same
// version and overwrite each other's file.
func UpdateAvatar(ctx context.Context, h Handle, id uint64, suffix string) (uint32, error) {
	res, err := h.ExecContext(ctx, qUpdateAvatar, suffix, id)
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: %w", err)
	}

	// Unlike UpdateNickname, RowsAffected is trustworthy here: the version
	// always increments, so a matched row always changes. 0 means no such id.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: rows affected: %w", err)
	}
	if n == 0 {
		return 0, ErrUserNotFound
	}

	version, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: update avatar: last insert id: %w", err)
	}
	return uint32(version), nil
}

// isDuplicateKey reports whether err is a MySQL unique-constraint violation.
// errors.As is used rather than a type assertion so the check still works once
// the error has been wrapped further down.
func isDuplicateKey(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry
}
