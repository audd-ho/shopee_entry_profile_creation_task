// Package config loads runtime configuration from environment variables with defaults.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration.
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
	Addr        string
	TCPAddr     string
	TCPPoolSize int
	WebRoot     string
}

type TCPConfig struct {
	Addr string
}

type MySQLConfig struct {
	DSN     string
	MaxOpen int
	MaxWait time.Duration
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	PoolSize int
}

type UploadsConfig struct {
	Root     string
	MaxBytes int64
}

type BcryptConfig struct {
	Cost int
}

type SessionConfig struct {
	TTL time.Duration
}

// Load reads config from environment variables and applies defaults.
func Load() *Config {
	return &Config{
		HTTP: HTTPConfig{
			Addr:        envStr("HTTP_ADDR", ":8080"),
			TCPAddr:     envStr("TCP_ADDR", "127.0.0.1:9000"),
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

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
