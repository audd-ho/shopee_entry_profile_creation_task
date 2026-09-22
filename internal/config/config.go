// TODO: Configuration loading.
// Reads config from env vars with sensible defaults.
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTP    HTTPConfig
	TCP     TCPConfig
	MySQL   MySQLConfig
	Redis   RedisConfig
	Uploads UploadsConfig
	Bcrypt  BcryptConfig
	Session SessionConfig
}

type HTTPConfig struct {
	Addr        string // default ":8080"
	TCPPoolSize int    // conns to TCP server; default 8
	WebRoot     string
}
type TCPConfig struct {
	Addr string // default ":9000"
}
type MySQLConfig struct {
	DSN     string        // required, e.g. "user:pass@tcp(host:3306)/db"
	MaxOpen int           // default 50
	MaxWait time.Duration // 200ms
}
type RedisConfig struct {
	Addr     string // default "localhost:6379"
	Password string
	DB       int
	PoolSize int // default 50

	// UserCacheDisabled turns off the user:<username> and profile:<user_id>
	// caches, for the "without cache layer" half of the performance runs.
	// Sessions are NOT affected: they live in the same Redis but are what
	// authentication depends on, so disabling them would measure the 401 path
	// rather than the uncached one.
	//
	// Phrased negatively so the zero value leaves the cache ON. A positively
	// named flag would silently disable the cache in every test and every
	// caller that builds a RedisConfig without thinking about it.
	UserCacheDisabled bool
}
type UploadsConfig struct {
	Root     string // default "./uploads"
	MaxBytes int64  // default 2*1024*1024 (2MB)
}
type BcryptConfig struct {
	// Cost defaults to 5, per system_design.md §4.3/§7: scaled down from the
	// textbook 10 specifically to hit the login-RPS targets, and is 1 above
	// bcrypt's own hard floor (MinCost = 4; anything lower is silently
	// replaced with cost 10 by golang.org/x/crypto/bcrypt, not rejected).
	// Weaker than the 10-12 usually recommended for password storage — still
	// salted, so still far stronger than unsalted SHA-256, but a stolen DB
	// dump is cheaper to brute-force offline than at the textbook cost. This
	// number MUST match whatever cost the seeder actually wrote (verify with
	// `SELECT LEFT(password_hash,7) ...` — bcrypt embeds cost in the hash
	// itself, so a mismatch here does not change how fast a real login
	// verifies, only how fast the DummyVerify decoy runs, reopening the
	// timing-based username-enumeration gap this cost is partly meant to
	// close alongside the uniform error message. See auth.go's DummyVerify
	// call.
	Cost int
}

type SessionConfig struct {
	TTL time.Duration // 24h
}

// Public API:
// reads env, applies defaults, validates
func Load() *Config {
	return &Config{
		HTTP: HTTPConfig{
			Addr:        envStr("HTTP_ADDR", ":8080"),
			TCPPoolSize: envInt("HTTP_TCP_POOL_SIZE", 8),
			WebRoot:     envStr("WEB_ROOT", "web"),
		},
		TCP: TCPConfig{
			Addr: envStr("TCP_LISTEN", ":9000"),
		},
		MySQL: MySQLConfig{
			DSN:     envStr("MYSQL_DSN", "app:app@tcp(127.0.0.1:3306)/userdb?parseTime=true&charset=utf8mb4"),
			MaxOpen: envInt("MYSQL_MAX_OPEN", 50),
			MaxWait: time.Duration(envInt("MYSQL_MAX_WAIT_MS", 200)) * time.Millisecond,
		},
		Redis: RedisConfig{
			Addr:     envStr("REDIS_ADDR", "127.0.0.1:6379"),
			Password: envStr("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0),
			PoolSize: envInt("REDIS_POOL_SIZE", 50),
			// CACHE_LAYER=off for a "without cache" run. Anything else, including
			// unset, leaves it on.
			UserCacheDisabled: envStr("CACHE_LAYER", "on") == "off",
		},
		Uploads: UploadsConfig{
			Root:     envStr("UPLOADS_ROOT", "uploads"),
			MaxBytes: int64(envInt("UPLOADS_MAX_BYTES", 2*1024*1024)),
		},
		Bcrypt: BcryptConfig{
			Cost: envInt("BCRYPT_COST", 5),
		},
		Session: SessionConfig{
			TTL: time.Duration(envInt("SESSION_TTL_SECONDS", 24*3600)) * time.Second,
		},
	}
}

func envStr(envKey, fallback string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return fallback
}

func envInt(envKey string, fallback int) int {
	if v := os.Getenv(envKey); v != "" {
		if v_int, err := strconv.Atoi(v); err == nil {
			return v_int
		}
	}
	return fallback
}
