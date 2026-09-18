package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"entry_profile_creation/internal/model"

	"github.com/redis/go-redis/v9"
)

var ErrCacheMiss = errors.New("cache miss")

const (
	credsCacheTTL = 10 * time.Minute
	userCacheTTL  = 1 * time.Hour
)

// Key builders — align with endpoint naming:
//   creds:<username>   → login   (POST /api/login)
//   user:<user_id>     → display (GET  /api/user)
func credsKey(username string) string { return "creds:" + username }
func userKey(userID uint64) string    { return "user:" + itoa(userID) }

// CredsRecord is cached under creds:<username>. Login-only.
type CredsRecord struct {
	ID           uint64 `json:"id"`
	PasswordHash []byte `json:"password_hash"`
}

// UserRecord is cached under user:<user_id>. Display data for /api/user.
type UserRecord struct {
	Nickname      string `json:"nickname"`
	AvatarVersion uint32 `json:"avatar_version"`
	AvatarSuffix  string `json:"avatar_suffix"`
}

// GetCreds reads creds:<username>. Returns ErrCacheMiss on miss.
func (c *Client) GetCreds(ctx context.Context, username string) (*CredsRecord, error) {
	b, err := c.rdb.Get(ctx, credsKey(username)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheMiss
	}
	if err != nil {
		return nil, err
	}
	var rec CredsRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// SetCreds writes creds:<username> with the default TTL.
func (c *Client) SetCreds(ctx context.Context, username string, u *model.User) error {
	rec := CredsRecord{
		ID:           u.ID,
		PasswordHash: u.PasswordHash,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, credsKey(username), b, credsCacheTTL).Err()
}

// GetUser reads user:<user_id>. Returns ErrCacheMiss on miss.
func (c *Client) GetUser(ctx context.Context, userID uint64) (*UserRecord, error) {
	b, err := c.rdb.Get(ctx, userKey(userID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheMiss
	}
	if err != nil {
		return nil, err
	}
	var rec UserRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// SetUser writes user:<user_id> with the default TTL.
func (c *Client) SetUser(ctx context.Context, u *model.User) error {
	rec := UserRecord{
		Nickname:      u.Nickname,
		AvatarVersion: u.AvatarVersion,
		AvatarSuffix:  u.AvatarSuffix,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, userKey(u.ID), b, userCacheTTL).Err()
}

// simple uint64 → string
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
