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
//   - Reader lease for cache population (M13)
//   - Graceful degradation when Redis is down
//
// Key scheme (hash-tagged for Redis Cluster readiness):
//   cache: {<key>}         (e.g. "{user:12345}")
//   lock:  {<key>}:lock    (e.g. "{user:12345}:lock")
//
// Cache value format:
//   <version>\n<json_data>
//   Version is a decimal uint64 string. The Lua scripts parse the prefix
//   to compare versions without deserializing the payload.
//
// Lease value format:
//   L:<token>
//   A reader holding a lease placed this before reading DB. The writer's
//   DEL-first step wipes it, voiding the lease.
package lockstack

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config tunes the lock stack's timing parameters.
type Config struct {
	// LockTTL is the Redis lock's time-to-live. Should be > expected write
	// latency but short enough for fast crash recovery. The watchdog extends
	// it periodically so a live writer never loses the lock.
	LockTTL time.Duration

	// LeaseTTL is how long a reader's lease survives while it reads DB.
	// Should be above the DB query timeout + margin (e.g. 300–500ms for a
	// PK SELECT). Too short: slow readers lose their lease (safe, just a
	// wasted fill). Too long: waiters wait the full TTL if a holder dies.
	LeaseTTL time.Duration

	// CacheTTL is the TTL set on cache entries written by the lock stack.
	CacheTTL time.Duration

	// PollMin is the initial backoff duration for reader polling on
	// LOCKED/LEASED states. Doubled each retry up to PollMax.
	PollMin time.Duration

	// PollMax caps the exponential backoff for reader polling.
	PollMax time.Duration

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
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 500 * time.Millisecond
	}
	if c.CacheTTL == 0 {
		c.CacheTTL = time.Hour
	}
	if c.PollMin == 0 {
		c.PollMin = 500 * time.Microsecond
	}
	if c.PollMax == 0 {
		c.PollMax = 4 * time.Millisecond
	}
	if c.MaxLockRetries == 0 {
		c.MaxLockRetries = 200
	}
}

// LockStack coordinates Redis locks, cache, and caller-supplied DB callbacks.
type LockStack struct {
	rdb *redis.Client
	cfg Config
}

var ErrLockHeld = errors.New("lockstack: lock held by another writer after max retries")

// --- Lua scripts ---
// Defined as package-level *redis.Script for EVALSHA with automatic
// NOSCRIPT → EVAL fallback (needed per node on Redis Cluster).

// versionSetCmd: atomically SET only if the new version is strictly greater.
// Used by the writer's write-through (M5 + M9). A lease value (L:<token>,
// no \n) is overwritten — it shouldn't be present inside the lock, but
// overwriting is correct if it is.
// KEYS[1] = cache key
// ARGV[1] = new version (decimal string)
// ARGV[2] = encoded value (version\ndata)
// ARGV[3] = TTL in seconds
var versionSetCmd = redis.NewScript(`
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
return 1`)

// lockReleaseCmd: DEL the lock only if the caller still owns it.
// KEYS[1] = lock key
// ARGV[1] = writer_id
var lockReleaseCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

// lockExtendCmd: extend the lock TTL only if the caller still owns it.
// KEYS[1] = lock key
// ARGV[1] = writer_id
// ARGV[2] = TTL in milliseconds
var lockExtendCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end
return 0`)

// readCheckCmd: atomic cache-check + lock-check + lease grant for readers.
// Returns a 1- or 2-element array:
//
//	{"HIT", <cached_value>} — real data in cache (M8)
//	{"LOCKED"}              — writer holds the lock, wait and retry
//	{"LEASED"}              — another reader holds the lease, wait and retry
//	{"GRANTED"}             — lease granted, caller should read DB then leaseSet
//
// KEYS[1] = cache key
// KEYS[2] = lock key
// ARGV[1] = lease token
// ARGV[2] = LeaseTTL in milliseconds
var readCheckCmd = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v and string.sub(v, 1, 2) ~= 'L:' then
  return {'HIT', v}
end
if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'LOCKED'}
end
if v then
  return {'LEASED'}
end
redis.call('SET', KEYS[1], 'L:' .. ARGV[1], 'PX', tonumber(ARGV[2]))
return {'GRANTED'}`)

// leaseSetCmd: populate cache only if the slot still holds this reader's
// lease token. Replaces the old readerVersionSetScript — no version compare
// needed because the reader only writes over its own placeholder.
// KEYS[1] = cache key
// ARGV[1] = lease token
// ARGV[2] = encoded value (version\ndata)
// ARGV[3] = CacheTTL in seconds
var leaseSetCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == 'L:' .. ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
  return 1
end
return 0`)

// releaseLeaseCmd: DEL the lease only if it's still ours. Used on DB read
// failure so waiters don't sit out the full LeaseTTL.
// KEYS[1] = cache key
// ARGV[1] = lease token
var releaseLeaseCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == 'L:' .. ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

// New returns a ready LockStack. Lua scripts are loaded lazily on first use
// via redis.NewScript (EVALSHA + automatic EVAL fallback on NOSCRIPT).
func New(rdb *redis.Client, cfg Config) (*LockStack, error) {
	cfg.applyDefaults()
	if cfg.Disabled {
		return &LockStack{cfg: cfg}, nil
	}
	return &LockStack{rdb: rdb, cfg: cfg}, nil
}

// --- Key scheme: hash-tagged for Redis Cluster readiness ---

func cacheKeyFor(key string) string { return "{" + key + "}" }
func lockKeyFor(key string) string  { return "{" + key + "}:lock" }

// --- Value encoding/decoding ---

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

func isLeaseValue(raw string) bool {
	return len(raw) >= 2 && raw[0] == 'L' && raw[1] == ':'
}

// --- Writer ID and lease token generation ---

func generateWriterID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}

var (
	leaseInstanceID = func() string {
		b := make([]byte, 8)
		crand.Read(b)
		return hex.EncodeToString(b)
	}()
	leaseSeq atomic.Uint64
)

func newLeaseToken() string {
	return leaseInstanceID + ":" + strconv.FormatUint(leaseSeq.Add(1), 36)
}

// --- Helpers ---

func cacheTTLSeconds(d time.Duration) int64 {
	s := int64(d.Seconds())
	if s < 1 {
		s = 1
	}
	return s
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}
