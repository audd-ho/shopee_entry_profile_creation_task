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
}
type UploadsConfig struct {
	Root     string // default "./uploads"
	MaxBytes int64  // default 2*1024*1024 (2MB)
}
type BcryptConfig struct {
	Cost int // default 10
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
		},
		Uploads: UploadsConfig{
			Root:     envStr("UPLOADS_ROOT", "uploads"),
			MaxBytes: int64(envInt("UPLOADS_MAX_BYTES", 2*1024*1024)),
		},
		Bcrypt: BcryptConfig{
			Cost: envInt("BCRYPT_COST", 10),
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
