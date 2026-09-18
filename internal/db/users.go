package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"time"

	"entry_profile_creation/internal/model"

	"github.com/go-sql-driver/mysql"
)

// ErrDuplicateUsername is returned when signup attempts a username that already exists.
var ErrDuplicateUsername = errors.New("username already exists")

// GetUserByUsername fetches a user for login (needs password_hash).
func GetUserByUsername(ctx context.Context, pool *Pool, username string) (*model.User, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()

	rows, err := c.Query(ctx,
		"SELECT id, password_hash, nickname, avatar_version, avatar_suffix, created_at, updated_at "+
			"FROM users WHERE username = ?",
		[]driver.Value{username},
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	row := make([]driver.Value, 7)
	if err := rows.Next(row); err != nil {
		return nil, ErrNoRows
	}
	u := &model.User{
		Username: username,
	}
	u.ID = toUint64(row[0])
	if b, ok := row[1].([]byte); ok {
		u.PasswordHash = append([]byte(nil), b...)
	}
	u.Nickname = toString(row[2])
	u.AvatarVersion = uint32(toUint64(row[3]))
	u.AvatarSuffix = toString(row[4])
	u.CreatedAt = toTime(row[5])
	u.UpdatedAt = toTime(row[6])
	return u, nil
}

// GetUserByID fetches profile info by user ID (no password hash needed).
func GetUserByID(ctx context.Context, pool *Pool, id uint64) (*model.User, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Release()

	rows, err := c.Query(ctx,
		"SELECT username, nickname, avatar_version, avatar_suffix, created_at, updated_at "+
			"FROM users WHERE id = ?",
		[]driver.Value{id},
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	row := make([]driver.Value, 6)
	if err := rows.Next(row); err != nil {
		return nil, ErrNoRows
	}
	u := &model.User{ID: id}
	u.Username = toString(row[0])
	u.Nickname = toString(row[1])
	u.AvatarVersion = uint32(toUint64(row[2]))
	u.AvatarSuffix = toString(row[3])
	u.CreatedAt = toTime(row[4])
	u.UpdatedAt = toTime(row[5])
	return u, nil
}

// InsertUser creates a new user row and returns the assigned ID.
func InsertUser(ctx context.Context, pool *Pool, username string, passwordHash []byte, nickname string) (uint64, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Release()

	res, err := c.Exec(ctx,
		"INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)",
		[]driver.Value{username, passwordHash, nickname},
	)
	if err != nil {
		if isDuplicateKey(err) {
			return 0, ErrDuplicateUsername
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// UpdateNickname sets a new nickname for a user.
func UpdateNickname(ctx context.Context, pool *Pool, id uint64, nickname string) error {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()

	_, err = c.Exec(ctx,
		"UPDATE users SET nickname = ? WHERE id = ?",
		[]driver.Value{nickname, id},
	)
	return err
}

// UpdateAvatar atomically increments avatar_version and sets avatar_suffix.
// Returns the new version.
func UpdateAvatar(ctx context.Context, pool *Pool, id uint64, suffix string) (uint32, error) {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Release()

	// One statement: atomic increment. Then read the new value.
	// Two statements over the same connection so LAST_INSERT_ID is consistent.
	_, err = c.Exec(ctx,
		"UPDATE users SET avatar_version = LAST_INSERT_ID(avatar_version + 1), avatar_suffix = ? WHERE id = ?",
		[]driver.Value{suffix, id},
	)
	if err != nil {
		return 0, err
	}
	rows, err := c.Query(ctx, "SELECT LAST_INSERT_ID()", nil)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	row := make([]driver.Value, 1)
	if err := rows.Next(row); err != nil {
		return 0, err
	}
	return uint32(toUint64(row[0])), nil
}

// --- type-conversion helpers for driver.Value ---

func toUint64(v driver.Value) uint64 {
	switch x := v.(type) {
	case int64:
		return uint64(x)
	case uint64:
		return x
	case []byte:
		var n uint64
		for _, c := range x {
			if c < '0' || c > '9' {
				return n
			}
			n = n*10 + uint64(c-'0')
		}
		return n
	}
	return 0
}

func toString(v driver.Value) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return ""
}

func toTime(v driver.Value) time.Time {
	if t, ok := v.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return strings.Contains(err.Error(), "Duplicate entry")
}
