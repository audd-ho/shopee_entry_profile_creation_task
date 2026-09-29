// TODO: Lock stack package — coordinates Redis lock, cache, and a caller-provided
// DB callback to keep cache and DB consistent under concurrent writes.
//
// The caller provides:
//   - A *redis.Client (for locks and cache)
//   - A DB write callback (for Write) or a DB read callback (for Read)
//
// The package handles:
//   - Per-key lock acquisition with TTL (M2b)
//   - Watchdog goroutine to extend TTL (M3)
//   - Delete-first cache ordering (M7)
//   - Cache write-through with version compare (M5 + M9)
//   - Serve-if-exists for readers (M8)
//   - Lock-aware reader wait (M9)
//   - Graceful degradation when Redis is down
//
// Key scheme:
//   cache: <key>         (e.g. "user:12345")
//   lock:  <key>:lock    (e.g. "user:12345:lock")
//
// Cache value format:
//   <version>\n<json_data>
//   Version is a decimal uint64 string. The Lua scripts parse the prefix
//   to compare versions without deserializing the payload.
package lockstack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config tunes the lock stack's timing parameters.
type Config struct {
	// LockTTL is the Redis lock's time-to-live. Should be > expected write
	// latency but short enough for fast crash recovery. The watchdog extends
	// it periodically so a live writer never loses the lock.
	LockTTL time.Duration

	// ReadPollInterval is how long a reader sleeps between lock-check retries
	// when the cache is empty and a writer holds the lock.
	ReadPollInterval time.Duration

	// MaxReadWait bounds how long a reader will wait for a lock to release
	// before falling through to DB.
	MaxReadWait time.Duration

	// CacheTTL is the TTL set on cache entries written by the lock stack.
	CacheTTL time.Duration

	// MaxLockRetries limits how many times Write retries lock acquisition
	// before returning ErrLockHeld.
	MaxLockRetries int

	// Disabled makes Read/Write pass through to the DB callback directly,
	// skipping all Redis operations. Matches the CACHE_LAYER=off behavior.
	Disabled bool
}

func (c *Config) applyDefaults() {
	if c.LockTTL == 0 {
		c.LockTTL = 2 * time.Second
	}
	if c.ReadPollInterval == 0 {
		c.ReadPollInterval = 2 * time.Millisecond
	}
	if c.MaxReadWait == 0 {
		c.MaxReadWait = c.LockTTL * 2
	}
	if c.CacheTTL == 0 {
		c.CacheTTL = time.Hour
	}
	if c.MaxLockRetries == 0 {
		c.MaxLockRetries = int(c.LockTTL / (c.ReadPollInterval * 5))
	}
}

// LockStack coordinates Redis locks, cache, and caller-supplied DB callbacks.
type LockStack struct {
	rdb *redis.Client
	cfg Config

	versionSetSHA       string
	readerVersionSetSHA string
	lockReleaseSHA      string
	lockExtendSHA       string
	readCheckSHA        string
}

var ErrLockHeld = errors.New("lockstack: lock held by another writer after max retries")

// Lua scripts — loaded once at startup, invoked via EVALSHA.

// versionSetScript: atomically SET only if the new version is strictly greater.
// KEYS[1] = cache key
// ARGV[1] = new version (decimal string)
// ARGV[2] = encoded value (version\ndata)
// ARGV[3] = TTL in seconds
const versionSetScript = `
local current = redis.call('GET', KEYS[1])
if current then
  local sep = string.find(current, '\n')
  if sep then
    local ver = tonumber(string.sub(current, 1, sep - 1))
    if ver >= tonumber(ARGV[1]) then
      return 0
    end
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
return 1`

// lockReleaseScript: DEL the lock only if the caller still owns it.
// KEYS[1] = lock key
// ARGV[1] = writer_id
const lockReleaseScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

// lockExtendScript: extend the lock TTL only if the caller still owns it.
// KEYS[1] = lock key
// ARGV[1] = writer_id
// ARGV[2] = TTL in milliseconds
const lockExtendScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end
return 0`

// readerVersionSetScript: version-compare SET for readers. Same as
// versionSetScript but also checks the lock — if a writer is active, the
// reader skips the cache write entirely (the writer's write-through will
// handle it). This prevents a slow reader from briefly caching stale data
// between the writer's DB UPDATE and its write-through.
// KEYS[1] = cache key
// KEYS[2] = lock key
// ARGV[1] = new version (decimal string)
// ARGV[2] = encoded value (version\ndata)
// ARGV[3] = TTL in seconds
const readerVersionSetScript = `
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1
end
local current = redis.call('GET', KEYS[1])
if current then
  local sep = string.find(current, '\n')
  if sep then
    local ver = tonumber(string.sub(current, 1, sep - 1))
    if ver >= tonumber(ARGV[1]) then
      return 0
    end
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
return 1`

// readCheckScript: atomic cache-check + lock-check for readers.
// Returns the cached value if present, "__LOCKED__" if a lock is held, or nil.
// KEYS[1] = cache key
// KEYS[2] = lock key
const readCheckScript = `
local data = redis.call('GET', KEYS[1])
if data then
  return data
end
if redis.call('EXISTS', KEYS[2]) == 1 then
  return '__LOCKED__'
end
return false`

const lockedSentinel = "__LOCKED__"

// New loads the Lua scripts into Redis and returns a ready LockStack.
func New(rdb *redis.Client, cfg Config) (*LockStack, error) {
	cfg.applyDefaults()

	if cfg.Disabled {
		return &LockStack{cfg: cfg}, nil
	}

	ctx := context.Background()
	ls := &LockStack{rdb: rdb, cfg: cfg}

	var err error
	if ls.versionSetSHA, err = rdb.ScriptLoad(ctx, versionSetScript).Result(); err != nil {
		return nil, fmt.Errorf("lockstack: load version-set script: %w", err)
	}
	if ls.readerVersionSetSHA, err = rdb.ScriptLoad(ctx, readerVersionSetScript).Result(); err != nil {
		return nil, fmt.Errorf("lockstack: load reader-version-set script: %w", err)
	}
	if ls.lockReleaseSHA, err = rdb.ScriptLoad(ctx, lockReleaseScript).Result(); err != nil {
		return nil, fmt.Errorf("lockstack: load lock-release script: %w", err)
	}
	if ls.lockExtendSHA, err = rdb.ScriptLoad(ctx, lockExtendScript).Result(); err != nil {
		return nil, fmt.Errorf("lockstack: load lock-extend script: %w", err)
	}
	if ls.readCheckSHA, err = rdb.ScriptLoad(ctx, readCheckScript).Result(); err != nil {
		return nil, fmt.Errorf("lockstack: load read-check script: %w", err)
	}

	return ls, nil
}

func cacheKeyFor(key string) string { return key }
func lockKeyFor(key string) string  { return key + ":lock" }

func encodeValue(version int64, data []byte) string {
	return strconv.FormatInt(version, 10) + "\n" + string(data)
}

func decodeValue(raw string) (data []byte, version int64, ok bool) {
	idx := strings.IndexByte(raw, '\n')
	if idx < 0 {
		return nil, 0, false
	}
	v, err := strconv.ParseInt(raw[:idx], 10, 64)
	if err != nil {
		return nil, 0, false
	}
	return []byte(raw[idx+1:]), v, true
}

func generateWriterID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func cacheTTLSeconds(d time.Duration) int64 {
	s := int64(d.Seconds())
	if s < 1 {
		s = 1
	}
	return s
}
