// TODO: Configuration loading.
// Reads config from .env file and env vars.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTP            HTTPConfig
	TCP             TCPConfig
	MySQLDataPool   MySQLPoolConfig
	MySQLLookupPool MySQLPoolConfig
	Shard           ShardConfig
	IDGen           IDGenConfig
	Redis           RedisConfig
	Uploads         UploadsConfig
	Bcrypt          BcryptConfig
	Session         SessionConfig
	LockStack       LockStackConfig
}

type HTTPConfig struct {
	Addr              string
	TCPPoolSize       int
	WebRoot           string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}
type TCPConfig struct {
	Addr string
}
type MySQLPoolConfig struct {
	MaxOpen           int
	MaxWait           time.Duration
	ConnMaxLifetime   time.Duration
	MaxIdleTime       time.Duration
	SweepInterval     time.Duration
	MaxBadConnRetries int
}
type ShardConfig struct {
	DataShardDSNs   []string
	LookupShardDSNs []string
	VnodesPerShard  int
}
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	PoolSize int

	// UserCacheDisabled turns off the user:<username> and profile:<user_id>
	// caches, for the "without cache layer" half of the performance runs.
	UserCacheDisabled bool

	MinIdleConns    int
	ConnMaxLifetime time.Duration
	PoolTimeout     time.Duration
	IOTimeout       time.Duration
	PingTimeout     time.Duration

	AuthTTL   time.Duration
	UserTTL   time.Duration
	LookupTTL time.Duration
}
type UploadsConfig struct {
	Root     string
	MaxBytes int64
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

type IDGenConfig struct {
	MachineID int
}

type SessionConfig struct {
	TTL time.Duration
}
type LockStackConfig struct {
	LockTTL        time.Duration // default 2s; 0 means use lockstack's built-in default
	LeaseTTL       time.Duration // default 500ms; 0 means use lockstack's built-in default
	CacheTTL       time.Duration // default 1h; 0 means use lockstack's built-in default
	PollMin        time.Duration // default 500µs
	PollMax        time.Duration // default 4ms
	MaxLockRetries int           // default 200
}

func Load() *Config {
	loadDotEnv(".env")

	return &Config{
		HTTP: HTTPConfig{
			Addr:              envStr("HTTP_ADDR", ""),
			TCPPoolSize:       envInt("HTTP_TCP_POOL_SIZE", 0),
			WebRoot:           envStr("WEB_ROOT", ""),
			ReadHeaderTimeout: time.Duration(envInt("HTTP_READ_HEADER_TIMEOUT_SECONDS", 0)) * time.Second,
			ReadTimeout:       time.Duration(envInt("HTTP_READ_TIMEOUT_SECONDS", 0)) * time.Second,
			WriteTimeout:      time.Duration(envInt("HTTP_WRITE_TIMEOUT_SECONDS", 0)) * time.Second,
			IdleTimeout:       time.Duration(envInt("HTTP_IDLE_TIMEOUT_SECONDS", 0)) * time.Second,
		},
		TCP: TCPConfig{
			Addr: envStr("TCP_LISTEN", ""),
		},
		MySQLDataPool: MySQLPoolConfig{
			MaxOpen:           envInt("MYSQL_DATA_MAX_OPEN", 0),
			MaxWait:           time.Duration(envInt("MYSQL_DATA_MAX_WAIT_MS", 0)) * time.Millisecond,
			ConnMaxLifetime:   time.Duration(envInt("MYSQL_DATA_CONN_MAX_LIFETIME_SECONDS", 0)) * time.Second,
			MaxIdleTime:       time.Duration(envInt("MYSQL_DATA_MAX_IDLE_TIME_SECONDS", 0)) * time.Second,
			SweepInterval:     time.Duration(envInt("MYSQL_DATA_SWEEP_INTERVAL_SECONDS", 0)) * time.Second,
			MaxBadConnRetries: envInt("MYSQL_DATA_MAX_BAD_CONN_RETRIES", 0)},
		MySQLLookupPool: MySQLPoolConfig{
			MaxOpen:           envInt("MYSQL_LOOKUP_MAX_OPEN", 0),
			MaxWait:           time.Duration(envInt("MYSQL_LOOKUP_MAX_WAIT_MS", 0)) * time.Millisecond,
			ConnMaxLifetime:   time.Duration(envInt("MYSQL_LOOKUP_CONN_MAX_LIFETIME_SECONDS", 0)) * time.Second,
			MaxIdleTime:       time.Duration(envInt("MYSQL_LOOKUP_MAX_IDLE_TIME_SECONDS", 0)) * time.Second,
			SweepInterval:     time.Duration(envInt("MYSQL_LOOKUP_SWEEP_INTERVAL_SECONDS", 0)) * time.Second,
			MaxBadConnRetries: envInt("MYSQL_LOOKUP_MAX_BAD_CONN_RETRIES", 0)},
		Shard: ShardConfig{
			DataShardDSNs:   envCSV("DATA_SHARD_DSNS", nil),
			LookupShardDSNs: envCSV("LOOKUP_SHARD_DSNS", nil),
			VnodesPerShard:  envInt("SHARD_VNODES", 0),
		},
		IDGen: IDGenConfig{
			MachineID: envInt("MACHINE_ID", 0),
		},
		Redis: RedisConfig{
			Addr:              envStr("REDIS_ADDR", ""),
			Password:          envStr("REDIS_PASSWORD", ""),
			DB:                envInt("REDIS_DB", 0),
			PoolSize:          envInt("REDIS_POOL_SIZE", 0),
			UserCacheDisabled: envStr("CACHE_LAYER", "") == "off",
			MinIdleConns:      envInt("REDIS_MIN_IDLE_CONNS", 0),
			ConnMaxLifetime:   time.Duration(envInt("REDIS_CONN_MAX_LIFETIME_SECONDS", 0)) * time.Second,
			PoolTimeout:       time.Duration(envInt("REDIS_POOL_TIMEOUT_MS", 0)) * time.Millisecond,
			IOTimeout:         time.Duration(envInt("REDIS_IO_TIMEOUT_MS", 0)) * time.Millisecond,
			PingTimeout:       time.Duration(envInt("REDIS_PING_TIMEOUT_SECONDS", 0)) * time.Second,
			AuthTTL:           time.Duration(envInt("CACHE_AUTH_TTL_SECONDS", 0)) * time.Second,
			UserTTL:           time.Duration(envInt("CACHE_USER_TTL_SECONDS", 0)) * time.Second,
			LookupTTL:         time.Duration(envInt("CACHE_LOOKUP_TTL_SECONDS", 0)) * time.Second,
		},
		Uploads: UploadsConfig{
			Root:     envStr("UPLOADS_ROOT", ""),
			MaxBytes: int64(envInt("UPLOADS_MAX_BYTES", 0)),
		},
		Bcrypt: BcryptConfig{
			Cost: envInt("BCRYPT_COST", 0),
		},
		Session: SessionConfig{
			TTL: time.Duration(envInt("SESSION_TTL_SECONDS", 0)) * time.Second,
		},
		LockStack: LockStackConfig{
			LockTTL:        time.Duration(envInt("LOCKSTACK_LOCK_TTL_MS", 0)) * time.Millisecond,
			LeaseTTL:       time.Duration(envInt("LOCKSTACK_LEASE_TTL_MS", 0)) * time.Millisecond,
			CacheTTL:       time.Duration(envInt("LOCKSTACK_CACHE_TTL_SECONDS", 0)) * time.Second,
			PollMin:        time.Duration(envInt("LOCKSTACK_POLL_MIN_US", 0)) * time.Microsecond,
			PollMax:        time.Duration(envInt("LOCKSTACK_POLL_MAX_US", 0)) * time.Microsecond,
			MaxLockRetries: envInt("LOCKSTACK_MAX_LOCK_RETRIES", 0),
		},
	}
}

// loadDotEnv reads a .env file and sets vars not already present in the
// environment. Real env vars always take precedence.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
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

func envCSV(envKey string, fallback []string) []string {
	v := os.Getenv(envKey)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
