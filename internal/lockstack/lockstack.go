// Lock stack package — coordinates Redis cache, per-key writer/reader leases,
// and a caller-provided DB callback to keep cache and DB consistent under
// concurrent reads and writes.
//
// The caller provides:
//   - A *redis.Client (for the cache slot and its leases)
//   - A DB write callback (for Write) or a DB read callback (for Read)
//
// The package handles:
//   - Writer lease in the cache slot as the lock, with TTL (M2c)
//   - Watchdog goroutine to extend TTL, capped at WatchdogTTL (M3)
//   - Acquiring the writer lease invalidates the slot (M7 delete-first)
//   - Cache write-through with version compare, which also unlocks (M5 + M9)
//   - Serve-if-exists for readers (M8)
//   - Reader lease for cache population (M13)
//   - One deadline shared by the watchdog and the DB write
//   - Graceful degradation when Redis is down
//
// Key scheme: one key per cached item, holding data or a lease. Every Lua
// script touches only this key, so no hash-tag or CROSSSLOT concerns in
// Redis Cluster (the braces are kept so keys stay stable):
//
//	{<key>}   (e.g. "{user:12345}")
//
// Slot values:
//
//	<version>\n<json_data>   real data. Version is a decimal uint64 string;
//	                         the Lua scripts parse the prefix to compare
//	                         versions without deserializing the payload.
//	LW:<writer_id>           writer lease = the lock. Placing it invalidates
//	                         the slot and voids any reader lease.
//	LR:<token>               reader lease, placed before reading DB. Only its
//	                         holder may replace it with data.
//
// Real data always starts with a digit, so any value starting with 'L' is a
// lease, never data.
package lockstack

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config tunes the lock stack's timing parameters.
type Config struct {
	// LockTTL is the writer lease's time-to-live. Should be > expected write
	// latency but short enough for fast crash recovery. The watchdog extends
	// it periodically so a live writer never loses the lock.
	LockTTL time.Duration

	// WatchdogTTL is the longest a single write can hold the lock when the
	// caller's context has no earlier deadline. A hung writer (process alive,
	// stuck on a call) would otherwise have its lock extended forever,
	// starving readers. The write runs under min(caller deadline,
	// WatchdogTTL), and the lease never outlives that deadline: acquiring and
	// every watchdog extension are clipped to it. Should be well above the
	// slowest legitimate write and at most the request timeout; raised to
	// LockTTL if lower.
	WatchdogTTL time.Duration

	// MinWriteBudget is the least time that must be left before the deadline
	// to START a DB write attempt; with less, Write refuses (ErrWriteBudget)
	// before anything is sent. Once sent, MySQL may queue the statement for up
	// to its lock-wait timeout, so MinWriteBudget must exceed the MySQL
	// session lock-wait timeout plus normal execution time — then every
	// attempt finishes or fails while the lease is still held, and nothing
	// can commit after the lock expired.
	MinWriteBudget time.Duration

	// MaxDeadlockRetries is how many times a DB write that MySQL rolled back
	// as a deadlock victim (error 1213) is retried, each retry still gated by
	// MinWriteBudget. Nothing else is retried: a lock-wait timeout means the
	// row stays locked, and an unknown outcome may already have committed.
	MaxDeadlockRetries int

	// MaxLockedWait is how long a reader polls while a writer lease holds
	// the slot before giving up and reading the DB uncached (default
	// 2 × LockTTL). Readers also give up early when only about LeaseTTL is
	// left before their request's deadline.
	MaxLockedWait time.Duration

	// CleanupTimeout bounds the Redis calls that run after the write's own
	// deadline may have passed: the write-through finish, releasing the
	// writer lease, and releasing a reader lease (default 2s).
	CleanupTimeout time.Duration

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
	if c.WatchdogTTL == 0 {
		c.WatchdogTTL = 4 * time.Second
	}
	if c.WatchdogTTL < c.LockTTL {
		c.WatchdogTTL = c.LockTTL
	}
	if c.MinWriteBudget == 0 {
		c.MinWriteBudget = 1500 * time.Millisecond
	}
	if c.MaxDeadlockRetries == 0 {
		c.MaxDeadlockRetries = 2
	}
	if c.MaxLockedWait == 0 {
		c.MaxLockedWait = 2 * c.LockTTL
	}
	if c.CleanupTimeout == 0 {
		c.CleanupTimeout = 2 * time.Second
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

// ErrWriteBudget is returned when too little time is left before the
// deadline to start a DB write safely (see Config.MinWriteBudget). Nothing
// was sent to the DB.
var ErrWriteBudget = errors.New("lockstack: not enough time left to start the DB write")

// errCommitted marks a DB write error that happened after the mutation
// committed (e.g. reading back the new row failed). See Committed.
var errCommitted = errors.New("lockstack: mutation committed")

// Committed wraps an error that a DB write callback hits AFTER its mutation
// has committed. Write then leaves the cache slot empty instead of restoring
// the old value — restoring would cache data the DB no longer has.
func Committed(err error) error {
	return fmt.Errorf("%w: %w", errCommitted, err)
}

// --- Lua scripts ---
// Defined as package-level *redis.Script for EVALSHA with automatic
// NOSCRIPT → EVAL fallback (needed per node on Redis Cluster). Every script
// touches only KEYS[1], the cache slot.

// writerAcquireCmd: take the writer lease (the lock). Waits only on another
// writer's lease; overwriting data invalidates the slot (M7) and overwriting
// a reader lease voids it (readers never block writers). Returns the old
// value when it was real data, so a failed DB write can restore it.
//
//	{"HELD"}           — another writer holds the slot, wait and retry
//	{"OK"}             — acquired; slot held no data
//	{"OK", <old_data>} — acquired; old data returned for restoration
//
// KEYS[1] = cache key
// ARGV[1] = writer lease value (LW:<writer_id>)
// ARGV[2] = LockTTL in milliseconds
var writerAcquireCmd = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v and string.sub(v, 1, 3) == 'LW:' then
  return {'HELD'}
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', tonumber(ARGV[2]))
if v and string.sub(v, 1, 1) ~= 'L' then
  return {'OK', v}
end
return {'OK'}`)

// writerFinishCmd: write-through after a successful DB write (M5 + M9);
// replacing the writer lease with data is also the unlock. Must NOT require
// owning the slot: if this writer's lease expired and a reader leased the
// slot before the commit, that reader's DB read is old — overwriting its
// lease voids it. Skips only another writer's lease (that writer finishes
// the job) and data at the same or a newer version.
// KEYS[1] = cache key
// ARGV[1] = this writer's lease value (LW:<writer_id>)
// ARGV[2] = new version (decimal string)
// ARGV[3] = encoded value (version\ndata)
// ARGV[4] = CacheTTL in seconds
var writerFinishCmd = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v and v ~= ARGV[1] then
  if string.sub(v, 1, 3) == 'LW:' then
    return 0
  end
  if string.sub(v, 1, 1) ~= 'L' then
    local sep = string.find(v, '\n')
    if sep then
      local ver = tonumber(string.sub(v, 1, sep - 1))
      if ver and ver >= tonumber(ARGV[2]) then
        return 0
      end
    end
  end
end
redis.call('SET', KEYS[1], ARGV[3], 'EX', tonumber(ARGV[4]))
return 1`)

// writerReleaseCmd: release the writer lease only if it is still ours —
// restoring the saved old data when given (DB write definitely failed, so
// the old data is still the truth), otherwise leaving the slot empty.
// A no-op after a successful finish: the slot no longer holds our lease.
// KEYS[1] = cache key
// ARGV[1] = this writer's lease value (LW:<writer_id>)
// ARGV[2] = saved value to restore, or "" for none
// ARGV[3] = CacheTTL in seconds
var writerReleaseCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
if ARGV[2] ~= '' then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
else
  redis.call('DEL', KEYS[1])
end
return 1`)

// lockExtendCmd: extend the writer lease TTL only if it is still ours.
// KEYS[1] = cache key
// ARGV[1] = this writer's lease value (LW:<writer_id>)
// ARGV[2] = TTL in milliseconds
var lockExtendCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end
return 0`)

// readCheckCmd: atomic cache-check + lease grant for readers on a miss.
// Returns a 1- or 2-element array:
//
//	{"HIT", <cached_value>} — real data in cache (M8)
//	{"LOCKED"}              — a writer holds the slot, wait and retry
//	{"LEASED"}              — another reader holds the lease, wait and retry
//	{"GRANTED"}             — lease granted, caller should read DB then leaseSet
//
// KEYS[1] = cache key
// ARGV[1] = reader lease value (LR:<token>)
// ARGV[2] = LeaseTTL in milliseconds
var readCheckCmd = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then
  if string.sub(v, 1, 3) == 'LW:' then
    return {'LOCKED'}
  end
  if string.sub(v, 1, 1) == 'L' then
    return {'LEASED'}
  end
  return {'HIT', v}
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', tonumber(ARGV[2]))
return {'GRANTED'}`)

// leaseSetCmd: populate cache only if the slot still holds this reader's
// lease. No version compare needed: the reader only ever replaces its own
// placeholder, and any writer that started since the grant has overwritten
// that placeholder with its writer lease.
// KEYS[1] = cache key
// ARGV[1] = reader lease value (LR:<token>)
// ARGV[2] = encoded value (version\ndata)
// ARGV[3] = CacheTTL in seconds
var leaseSetCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
  return 1
end
return 0`)

// releaseLeaseCmd: DEL the reader lease only if it's still ours. Used on DB
// read failure so waiters don't sit out the full LeaseTTL.
// KEYS[1] = cache key
// ARGV[1] = reader lease value (LR:<token>)
var releaseLeaseCmd = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
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

// Config returns the configuration in effect, with defaults applied.
func (ls *LockStack) Config() Config { return ls.cfg }

// --- Key scheme ---

func cacheKeyFor(key string) string { return "{" + key + "}" }

// Lease prefixes. Real data starts with a digit, so these never collide.
const (
	writerLeasePrefix = "LW:"
	readerLeasePrefix = "LR:"
)

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

// isLeaseValue reports whether a slot value is a lease (writer or reader)
// rather than data. Data starts with its version digits.
func isLeaseValue(raw string) bool {
	return len(raw) > 0 && raw[0] == 'L'
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
