# Lock Stack + Lease — Implementation Component Guide

A practical checklist for implementing the lock stack + lease
(M2b+M3+M7+M8+M9+M10+M13, with M5 version compare kept on the writer's
write-through). Each component is listed with what it does, why it exists, what
it depends on, and implementation notes for Go.

M13 (reader lease) is the chosen fix for the slow-reader cache-populate gap —
see §Reader Cache-Populate Failure Modes at the end. For the full design
reasoning and alternatives, see `db_cache_design.md`.

### Terms used in this guide

Problems:

| ID | Problem |
|---|---|
| P1 | Stale read during write — reader gets old cache while DB is being updated |
| P2 | Slow-reader overwrite — slow reader writes old data to cache after a newer write |
| P3 | DB stampede — many readers hit DB at once after the cache is cleared |
| P6 | Reader starvation — readers blocked indefinitely (writer died holding the lock) |
| P7 | Concurrent writer race — two writers update cache in the wrong order |
| P8 | Lock expires mid-write — another writer enters while the first still works |

Mechanisms in the stack:

| ID | Mechanism |
|---|---|
| M2b | Per-key Redis lock with TTL |
| M3 | Watchdog — extends the lock TTL while the writer is alive |
| M5 | Version-in-value + Lua compare-and-SET (kept on the writer's write-through) |
| M7 | Delete-first — DEL cache before the DB UPDATE, inside the lock |
| M8 | Serve-if-exists — a cache hit is served even while the lock is held |
| M9 | Write-through inside the lock — SET cache before unlocking |
| M10 | Invalidate-on-write |
| M12 | Singleflight — in-process deduplication of concurrent requests per key |
| M13 | Reader lease — a reader populates cache only if its lease token survived |

Other terms: **Simple** = M1+M10+M4 — DB UPDATE → DEL cache, no lock. When
Redis is down this stack falls back to Simple's write behaviour (DB UPDATE →
best-effort DEL) on its own key — there is no version-in-key here. **CacheTTL** = normal lifetime of a cached value.
**LeaseTTL** = lifetime of a reader's lease.

---

## Decision Points (decide before starting)

- [ ] **Pipelined or naive?**
  - Naive: 5 separate Redis round trips. Simpler code, sequential commands.
  - Pipelined: 2 Redis round trips (pre-DB Lua + post-DB Lua). Faster writes,
    but Lua scripts must be correct and maintained.
  - Can start naive, optimize to pipelined later — reader behavior is identical.

- [ ] **Lock TTL value?**
  - Configurable. Should be > expected write time but short enough for fast
    crash recovery. E.g. 500ms–2s for fast writes.
  - Benchmark your DB UPDATE + Redis ops to calibrate.

- [ ] **Watchdog extension interval?**
  - Typically TTL/3. E.g. TTL=1s → extend every 333ms.
  - Must be < TTL or the lock expires before the watchdog extends.

- [x] **Reader wait mechanism (decided): `readCheckScript` on a miss + capped
      exponential backoff polling**
  - `readCheckScript` checks cache, lock and lease atomically in one round trip
    (component 9); waiters retry it at 0.5 → 1 → 2 → 4ms (capped) + jitter.
  - Pub/sub wake-up is an optional later optimisation (component 9 note).

- [x] **Read hit path: plain GET first (decided)**
  - Hit path = plain `GET`; a value starting with `L:` is decoded as a miss in
    Go (prefix check, no Lua). Only a miss calls `readCheckScript`.
  - Why not always `EVALSHA readCheck` (one call for everything): every script
    call pays a fixed Lua cost (SHA lookup, KEYS/ARGV setup, VM entry/exit) no
    matter which branch runs — roughly 2–5µs vs ~1µs for a plain GET
    (ballpark; measure with `redis-benchmark`). Redis is single-threaded and
    shared by every instance, so that would roughly double Redis CPU on the
    hottest path. GET-first costs one extra round trip on a miss only.

- [ ] **Lease TTL (`LeaseTTL`)?**
  - How long a reader's lease survives while it reads DB. Set to the DB query
    timeout + margin (e.g. 300–500ms for a PK SELECT); the DB query timeout
    must stay below LeaseTTL.
  - Too short: slow-but-healthy readers lose their lease and don't cache (safe,
    just a wasted fill). Too long: if a lease holder dies, other readers wait on
    `LEASED` until it expires.
  - Keep the DB query timeout **below** LeaseTTL. Otherwise, when the DB is
    slow, every lease holder loses its lease before populating, waiters take
    turns re-leasing, and nothing gets cached.

- [x] **What do readers do on `LEASED`? (decided)**
  - Poll with the same backoff loop as `LOCKED`, bounded by `LeaseTTL`.
  - After the bound: read DB directly and return the result **without caching**.

- [x] **Lease token source (decided): instance ID + atomic counter**
  - Generated only on a miss, right before `readCheckScript`.
  - `instanceID` = 8 random bytes from `crypto/rand` once at startup, plus a
    per-process atomic counter → ~10ns per token, unique across instances and
    restarts (a restart gets a new instanceID).
  - `crypto/rand` 16 bytes per token (~0.1–0.5µs) is also fine.
  - Don't use a Redis `INCR` counter (it can reset on a Redis restart and repeat
    tokens) or timestamps (can repeat).

```go
var instanceID = randomHex(8) // crypto/rand, once at startup
var seq atomic.Uint64

func newToken() string {
    return instanceID + ":" + strconv.FormatUint(seq.Add(1), 36)
}
```

- [ ] **Process health sniffing in watchdog?** (optional, see component 7b)

---

## Write Path Components

### 1. Lock Acquisition (M2b)

**What:** Acquire a per-key Redis lock with TTL before any DB/cache ops.

**Why:** Serializes writers to prevent concurrent writer race (P7). Prevents
readers from seeing intermediate state (P1).

**Key:** `{user:<id>}:lock` (hash tag, see N1)

**Redis command:** `SET {user:<id>}:lock <writer_id> EX <TTL> NX`
- `NX`: only set if not exists (acquire only if free)
- `EX <TTL>`: auto-expire (crash safety)
- `<writer_id>`: unique value (instance ID + counter, or a UUID; not a goroutine
  ID, which isn't unique across instances) — needed to ensure
  only the lock owner can release it

**On failure (lock held by another writer):**
- Retry with backoff, or return error/queue
- Do NOT proceed without the lock

**On failure (Redis down):**
- Degrade to Simple: DB UPDATE → DEL cache (skip lock, accept ms stale)
- See component 11 (fallback)

**Implementation notes:**
- [ ] Generate unique writer_id per write operation
- [ ] Use SET with NX+EX in one command (atomic acquire+TTL)
- [ ] Store writer_id for lock release verification
- [ ] Handle retry logic (backoff, max retries, timeout)

---

### 2. Cache Value Save (for restoration on failure)

**What:** GET the current cache value before deleting it.

**Why:** If the DB UPDATE fails (step 4), the writer can restore the cache to
its pre-write state instead of leaving it empty. Reduces unnecessary DB
fallbacks for readers after a failed write.

**Redis command:** `GET {user:<id>}` (or `GETDEL {user:<id>}` on Redis 6.2+ to
combine with step 3)

**What to save:** The full cached value (including version if using
version-in-value). Store in a local variable — only needed for the duration of
this write operation.

**Implementation notes:**
- [ ] Store result in local variable (not shared state)
- [ ] Handle cache miss (key didn't exist) — nothing to save, nothing to restore
- [ ] Handle a lease value (`L:<token>`) — treat as nothing to save. Restoring a
      lease would be harmless (DB unchanged) but pointless.
- [ ] If using pipelining: this is inside the pre-DB Lua script

---

### 3. Cache Deletion — Delete-First Ordering (M7)

**What:** Delete the cache entry BEFORE updating the DB.

**Why:** Safety net. If the DB UPDATE fails after this point, cache is empty →
readers fall through to DB → get correct (old) data. Never stale. Compare to
DB-first: if DB succeeds but cache SET fails → stale.

Also the lease invalidation point (M13): the DEL wipes any outstanding reader
lease, so a reader whose DB read may predate this write can't populate the
cache afterwards. No extra command needed — the plain DEL does it.

This works because the lease lives in the **same key** as the data. The slot
holds either real data (`<version>\n<data>`) or a lease (`L:<token>`), never
both — a lease is only granted into an empty slot, and a successful populate
replaces it. One `DEL {user:<id>}` clears whichever is there. Don't move the lease
to a separate key (e.g. `{user:<id>}:lease`): the writer would then need to DEL
both, and `leaseSetScript` would need to check both.

**Redis command:** `DEL {user:<id>}`

**Depends on:** Step 1 (lock must be held). Step 2 (save value first if you
want restoration capability).

**Implementation notes:**
- [ ] Must happen AFTER lock acquired, AFTER value saved
- [ ] Must happen BEFORE DB UPDATE
- [ ] DEL is idempotent — safe to retry

---

### 4. DB UPDATE

**What:** The actual database write (UPDATE, INSERT, etc.).

**Why:** This is the source-of-truth mutation. Everything else exists to keep
the cache consistent with this.

**Implementation notes:**
- [ ] Standard DB operation within a transaction
- [ ] On success: proceed to step 5 (cache write-through)
- [ ] On failure: attempt cache restoration (step 4b), then release lock (step 6)
- [ ] The row version should be incremented in the DB (for version-in-value, M5)

### 4b. Cache Restoration on DB Failure

**What:** If DB UPDATE fails, restore the cache from the saved value (step 2).

**Why:** Avoids leaving cache empty unnecessarily. Readers get cache hit
(old correct data) instead of falling through to DB.

**Redis command:** `SET {user:<id>} <saved_value> EX <CacheTTL>`

**On restoration failure (Redis error):**
- Accept it — cache stays empty, readers hit DB. Correct, just slower.
- Release lock and return the DB error.

**Implementation notes:**
- [ ] Only attempt if step 2 successfully saved a value
- [ ] Use the version-compare Lua (M5) for the restore too? Not strictly
      necessary since we're inside the lock, but consistent.
- [ ] Log the DB failure and whether restoration succeeded

---

### 5. Cache Write-Through with Version Compare (M5 + M9)

**What:** After successful DB UPDATE, write the new data to cache with a Lua
script that atomically checks the version before writing.

**Why:**
- M9 (write-through): cache populated before unlock → readers after unlock get
  cache hit instead of DB stampede (P3).
- M5 (version-in-value): extra protection on the writer side — rejects a write
  if the cache already holds the same or a newer version. Readers no longer use
  this script; they populate through the lease (component 10).

**Lua script (`versionSetScript`):**
```lua
-- KEYS[1] = cache key ({user:<id>})
-- ARGV[1] = new version
-- ARGV[2] = encoded value "<version>\n<data>"
-- ARGV[3] = CacheTTL in seconds

local current = redis.call('GET', KEYS[1])
if current then
  local sep = string.find(current, '\n')
  if sep then                                   -- real data: compare versions
    local current_version = tonumber(string.sub(current, 1, sep - 1))
    if current_version and current_version >= tonumber(ARGV[1]) then
      return 0  -- reject: current is same or newer
    end
  end
  -- no newline (e.g. a lease "L:<token>") → no version → overwrite
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
return 1  -- success
```

**On failure (Redis error, Lua error):**
- Cache stays empty (DEL already ran in step 3). Readers hit DB. Correct.
- The DEL-first ordering (M7) is the safety net here.
- Release lock and return success (the DB write succeeded — that's what matters).

**Implementation notes:**
- [ ] Run via go-redis `redis.NewScript(...).Run()` (EVALSHA with automatic
      `EVAL` fallback on `NOSCRIPT` — needed per node on Cluster, see N1)
- [ ] Version must be embedded in the cached value
- [ ] The Lua compare must handle: key doesn't exist (always write), key exists
      with lower version (write), key exists with same/higher version (reject)
- [ ] A lease value (`L:<token>`, no newline → no version parsed) is
      overwritten. It shouldn't exist here (DEL ran inside the lock and
      readCheck grants no lease while the lock is held), but overwriting is the
      right behaviour if it does.
- [ ] If using pipelining: this + DEL lock is the post-DB Lua script

---

### 6. Lock Release

**What:** Delete the lock key to unblock waiting readers and other writers.

**Why:** Readers blocked on the lock can proceed. Next writer can acquire.

**Redis command:** Release should verify ownership:
```lua
-- KEYS[1] = lock key
-- ARGV[1] = writer_id (must match)
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0  -- not our lock, don't delete
```

**Why verify ownership:** If the lock TTL expired and another writer acquired
it, blindly DELeting would release someone else's lock. Checking writer_id
prevents this.

**On failure (Redis error):**
- Lock stays held until TTL expires (watchdog is dead since we're in cleanup).
- Readers with M8 can still serve cache if it was populated (step 5).
- After TTL: readers unblock normally.

**Implementation notes:**
- [ ] MUST happen in a defer/finally — ensure lock is released on any exit path
- [ ] Use the ownership-check Lua script, not plain DEL
- [ ] If using pipelining: combine with step 5 in the post-DB Lua script
      (but still need the ownership check)

---

### 7. Watchdog (M3)

**What:** A background goroutine that periodically extends the lock's TTL while
the writer is alive and working.

**Why:** Prevents lock from expiring mid-write if the write takes longer than
expected (P8). If the writer crashes, the goroutine dies too → TTL expires →
safe recovery (P6).

**Mechanism:**
```go
func startWatchdog(ctx context.Context, lockKey, writerID string, ttl time.Duration) context.CancelFunc {
    ctx, cancel := context.WithCancel(ctx)
    go func() {
        ticker := time.NewTicker(ttl / 3)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                // Extend only if we still own the lock
                // Lua: if GET lockKey == writerID then EXPIRE lockKey ttl
                extended := extendLock(lockKey, writerID, ttl)
                if !extended {
                    return // lock lost, stop extending
                }
            }
        }
    }()
    return cancel
}
```

**Lifecycle:**
1. Start watchdog immediately after lock acquisition (step 1)
2. Watchdog extends TTL every TTL/3
3. Cancel watchdog after lock release (step 6) or on any exit

**Implementation notes:**
- [ ] Use context.WithCancel — cancel on write completion or error
- [ ] Extension Lua must check writer_id (don't extend someone else's lock)
- [ ] Normal writes (~a few ms) finish before watchdog ever fires — near-zero cost
- [ ] Log if watchdog actually fires an extension (indicates slow write, worth investigating)
- [ ] The watchdog goroutine MUST be canceled in the defer/finally that releases the lock

---

### 7b. Process Health Sniffing in Watchdog (OPTIONAL)

**What:** Instead of just checking "is the goroutine alive" (implicit — if the
process dies, the goroutine dies), the watchdog could actively check whether
the writer is making progress.

**Why:** Detects stuck writers (deadlock, infinite loop, blocked on something)
that are alive but not progressing. Standard watchdog only detects dead processes.

**Possible approaches:**
- Writer updates a progress timestamp; watchdog checks it
- Watchdog checks if the writer's DB connection is active
- Watchdog checks if the Redis connection is active

**Concerns:**
- TOCTOU: checking "between" operations gives false positives (writer is between
  Redis DEL and DB UPDATE — not holding either connection, but fine)
- Adds complexity for a rare scenario (stuck but alive processes)
- The TTL already provides a backstop — if the writer is truly stuck, the lock
  eventually expires

**Recommendation:** Start without it. The standard goroutine-based watchdog
handles the common failure case (crash). Add health sniffing only if you observe
stuck-writer issues in production.

- [ ] Decide: implement now or defer?
- [ ] If implementing: progress-timestamp approach is simplest (writer sets
      atomic timestamp before each step, watchdog checks age)

---

## Read Path Components

### 8. Serve-If-Exists / M8

**What:** On a cache GET, if the value exists, serve it immediately — even if a
lock is held. Don't check the lock first.

**Why:** Cache delete is the first step inside the lock (M7). If the cache entry
exists, either:
- The DEL hasn't happened yet → DB unchanged → cached data IS current truth
- The Lua SET already ran (step 5) → cache has new correct data

In both cases, serving the cache is correct. Only block when cache is empty
AND lock is held.

**Implementation (read path):**
```
1. GET cache key
2. HIT? → return immediately (correct, no lock check needed)
3. MISS? → proceed to step 9 (check lock)
```

**Implementation notes:**
- [ ] The read path should attempt GET FIRST, lock check SECOND
- [ ] This is the common fast path — most reads during writes get served here
- [ ] No Lua needed for this step — plain GET
- [ ] A lease value (`L:<token>`) is NOT a hit — decode it as a miss and go to
      step 9. Real values start with the version digits, so a `L:` prefix is
      unambiguous.

---

### 9. Lock Check + Lease Grant (reader miss path)

**What:** When cache is empty (miss), atomically: serve data if it appeared,
wait if a writer holds the lock, wait if another reader holds the lease,
otherwise grant this reader a lease (M13).

**Why:**
- Locked → a writer is in progress and will populate the cache (M9). Waiting
  avoids a DB round trip — after unlock, the reader gets a cache hit.
- Lease → the only way a reader may later populate the cache. Placing the
  token *before* the DB read is what lets the writer's DEL void it.
- Leased by someone else → another reader is already fetching from DB. Waiting
  avoids a stampede (across instances, not just in-process).

**Lua script (`readCheckScript`, atomic check-and-grant):**
```lua
-- KEYS[1] = cache key
-- KEYS[2] = lock key
-- ARGV[1] = lease token (instanceID + counter, generated by the caller)
-- ARGV[2] = LeaseTTL in ms

local v = redis.call('GET', KEYS[1])
if v and string.sub(v, 1, 2) ~= 'L:' then
  return {'HIT', v}              -- real data (M8 — may have been populated while we waited)
end
if redis.call('EXISTS', KEYS[2]) == 1 then
  return {'LOCKED'}              -- writer in progress: wait and retry
end
if v then
  return {'LEASED'}              -- another reader holds the lease: wait and retry
end
redis.call('SET', KEYS[1], 'L:' .. ARGV[1], 'PX', tonumber(ARGV[2]))
return {'GRANTED'}               -- we hold the lease: go to DB (step 10)
```

**Wait mechanism (on `LOCKED` / `LEASED`):**
- Poll with backoff: first retry after ~0.5ms, then 1 → 2 → 4ms (capped), with
  a little jitter. Normal waits (~one DB write) finish within the first few
  short polls; long waits (crashed writer, slow DB) back off instead of
  hammering Redis for the whole LockTTL. Generate a fresh token per attempt, or
  reuse one — either is fine
- Max wait: bound it (`LOCKED`: e.g. LockTTL × 2; `LEASED`: LeaseTTL)
- After max wait: read DB directly and return the result **without caching**
  (no lease → no populate)
- If the lease holder dies, its lease expires and the next retry returns
  `GRANTED` to one waiter, which takes over the fill — waiters don't all fall
  through to DB

**Latency cost of polling:** polls during the block overlap with the writer's
work and are free. The only extra time is the gap between the unlock (or lease
fill) and the next poll that sees it — on average half the current interval,
at most the full interval. That's why the first intervals are kept short.

**Redis load from polling:** only while something is blocked:
`poll rate ≈ (keys blocked now) × (waiters per key) × (1 / interval)`, where
keys blocked now ≈ write rate × block duration. Levers, most effective first:
1. Singleflight (M12) — caps waiters at 1 per key per instance regardless of
   read rate
2. Backoff (above) — long waits stop polling hard
3. Redis Cluster sharding — spreads polling for *different* keys across shards;
   doesn't help a single hot key (singleflight does)

**Optional, not implemented — wake-up signal instead of polling:**
Replaces the polling slack with ~one round trip. More moving parts, so only
worth it if waits are frequent and latency-sensitive.
- Use Redis pub/sub, not `BLPOP` (wakes only one waiter and holds a pooled
  connection per blocked waiter) and not an MQ (ms latency, durable delivery
  isn't needed — a missed wake-up costs one poll).
- One subscriber connection per instance on a single channel (key as payload),
  in-process fan-out to local waiters. `PUBLISH` from inside the writer's
  post-DB Lua, `leaseSetScript`, and `releaseLeaseScript`.
- Waiters register locally **before** calling `readCheckScript` (avoids a missed
  wake-up); keep a long fallback timeout (20–50ms) because pub/sub is
  at-most-once. On Redis 7+ Cluster use sharded pub/sub (`SPUBLISH`).

```go
func (c *Cache) GetUser(ctx context.Context, id string) (*User, error) {
    raw, err := c.rdb.Get(ctx, cacheKey(id)).Result()
    if err == nil && !strings.HasPrefix(raw, "L:") {
        return decode(raw) // hit (M8)
    }
    // redis.Nil or a lease value → miss path

    start := time.Now()
    backoff := c.cfg.PollMin // e.g. 500µs
    for {
        token := newToken()
        res, err := c.readCheck(ctx, cacheKey(id), lockKey(id), token, c.cfg.LeaseTTL)
        if err != nil {
            return c.db.SelectUser(ctx, id) // Redis trouble: read DB, don't cache
        }
        switch res.State {
        case "HIT":
            return decode(res.Value)
        case "GRANTED":
            return c.fetchAndFill(ctx, id, token) // fresh DB SELECT → leaseSet
        case "LOCKED", "LEASED":
            maxWait := c.cfg.LeaseTTL // LEASED
            if res.State == "LOCKED" {
                maxWait = 2 * c.cfg.LockTTL
            }
            if time.Since(start) > maxWait {
                return c.db.SelectUser(ctx, id) // give up waiting: read DB, don't cache
            }
            time.Sleep(backoff + jitter(backoff/4))
            backoff = min(backoff*2, c.cfg.PollMax) // e.g. cap at 4ms
        }
    }
}
```

**Implementation notes:**
- [ ] Lua script returns four states: HIT (serve), LOCKED (wait), LEASED (wait), GRANTED (step 10)
- [ ] Lock is checked before granting — no lease is ever granted while a writer
      holds the lock
- [ ] Capped exponential backoff: PollMin ~0.5ms, doubling, PollMax 2–5ms, ± jitter
- [ ] Set a max wait time — don't block forever
- [ ] After unlock, the first GET in the Lua script should catch the M9 cache hit
- [ ] Wrap the whole miss path (readCheck loop + DB read + fill) in singleflight
      (M12) per key — only one goroutine per instance polls Redis / holds the
      lease; the rest share its result (component 12)

---

### 10. Cache Population on Miss (reader cold path, lease-guarded)

**What:** A reader holding a lease reads DB, then populates the cache only if
the slot still holds its lease (M13).

**Why:** Closes the slow-reader gap that the M5 version compare can't: when the
writer has DELeted the cache (and its write-through failed, or it died after
the DB commit), the slot is empty and a version compare has nothing to compare
against. The lease doesn't compare versions — it checks that **no writer has
touched the key since this reader's DB read began**, because every writer DELs
first and that DEL wipes the lease.

**Flow:**
```
1. DB SELECT → get data with version
2. leaseSetScript: slot == "L:<token>"? → SET data : drop
3. Return data to caller (whether or not it was cached)
```

**Lua script (`leaseSetScript`):**
```lua
-- KEYS[1] = cache key
-- ARGV[1] = lease token
-- ARGV[2] = encoded value ("<version>\n<data>")
-- ARGV[3] = CacheTTL in seconds

if redis.call('GET', KEYS[1]) == 'L:' .. ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
  return 1                        -- populated
end
return 0                          -- lease gone (writer DEL, expiry, or replaced): drop
```

**Why readers need no version compare:** `leaseSetScript` only ever replaces
the reader's own placeholder, which holds no data — it can never overwrite real
data, so "old overwrites new" can't happen. And a successful populate proves no
writer touched the key between grant and populate (a writer that locked after
the grant DELed the lease; a writer holding the lock at grant time prevented
the grant; a writer that finished before the grant is visible to the DB read).
Lease expiry plays no part in correctness — it only frees the slot if a lease
holder dies.

**Requirement — the DB read must be fresh, after the grant:**
The guarantee assumes the SELECT sees everything committed before the grant.
Run it as an autocommit statement, or in a transaction started **after**
`readCheckScript` returned GRANTED. If it runs inside a `REPEATABLE READ`
transaction that began *before* the grant, MVCC can return a snapshot older than
a writer that committed before the grant, and the lease would protect stale
data.

**Implementation notes:**
- [ ] DB SELECT is autocommit, or its transaction begins after GRANTED — never
      reuse a transaction/snapshot opened before the grant
- [ ] Replaces `readerVersionSetScript` (the superseded pre-lease script, see
      Scenario 1) — no version compare, no lock check needed
      (a writer that locked after the grant has DELeted the lease; readCheck
      grants none while locked)
- [ ] A rejected populate (return 0) is normal, not an error — serve the DB
      result, just don't cache it
- [ ] On DB error: return the error and release the lease if it's still ours
      (`releaseLeaseScript`, see §Points to Note N4) so waiters don't sit out
      the full LeaseTTL
- [ ] All scripts (`readCheckScript`, `leaseSetScript`, `releaseLeaseScript`,
      `versionSetScript`) defined once with `redis.NewScript` and invoked with
      `.Run()` (EVALSHA + automatic `NOSCRIPT` → `EVAL` fallback)

---

## Infrastructure Components

### 11. Redis Fallback (degrade to Simple)

**What:** If Redis is down (connection error, timeout), skip the lock and cache
ops. Fall back to DB UPDATE only.

**Why:** Redis dependency shouldn't block writes. Degrade gracefully to the
Simple approach (DB → DEL when Redis returns).

**Implementation:**
```go
func writeUser(ctx context.Context, userID string, data UserData) error {
    lockAcquired, err := acquireLock(ctx, userID)
    if err != nil {
        // Redis down — degrade to Simple
        log.Warn("Redis unavailable, degrading to lockless write", "error", err)
        if err := db.UpdateUser(ctx, userID, data); err != nil {
            return err
        }
        // Try DEL cache (best effort — Redis might still be down)
        _ = cache.Delete(ctx, userID)
        return nil
    }
    // Normal lock stack path...
}
```

**Implementation notes:**
- [ ] Detect Redis down vs lock-held-by-another — different handling
- [ ] Log degradation events (monitoring/alerting)
- [ ] When Redis returns: brief stale window (same as Simple) if the fallback
      DEL succeeded; if it failed, the old entry stays stale until CacheTTL
- [ ] Consider a circuit breaker for Redis to avoid repeated timeout costs

---

### 12. Singleflight / M12 (OPTIONAL but recommended)

**What:** Deduplicate concurrent miss-path work for the same key within one
instance.

**Why:** When N goroutines miss the same key, only 1 should poll Redis / take
the lease / read DB.

**Where to apply:** around the whole miss path — the readCheck loop, the DB
read and the fill (component 9, and its uncached fallback after max wait).

The lease (M13) already stops a stampede across instances; singleflight trims
the remaining in-process duplicates.

**Implementation:**
```go
var sfGroup singleflight.Group

func (c *Cache) getUserMiss(ctx context.Context, userID string) (*User, error) {
    val, err, _ := sfGroup.Do("{user:"+userID+"}", func() (any, error) {
        return c.missPath(ctx, userID) // readCheck loop → fresh DB read → leaseSet
    })
    if err != nil {
        return nil, err
    }
    return val.(*User), nil
}
```

**Implementation notes:**
- [ ] Use `golang.org/x/sync/singleflight`
- [ ] Key should match the cache key scope (per user ID)
- [ ] Shared result means shared error — if the one DB call fails, all waiters
      get the error. This is usually fine (they'd all fail anyway).
- [ ] Per-instance only — each app instance deduplicates independently

---

## Points to Note

### N1. Redis Cluster: hash tags — `{…}`

**What it is:** `{…}` in a key tells Redis Cluster **which part of the key to
hash**. Only the text inside the braces is hashed; the full key name (braces
included) is still what's stored and looked up.

```
{user:123}        → hash("user:123") → slot 5474   ← cache key (data or lease)
{user:123}:lock   → hash("user:123") → slot 5474   ← lock key, same slot → same node
user:123:lock     → hash("user:123:lock") → some other slot, maybe another node
```

**Why (Cluster):** Redis Cluster splits keys into 16384 slots spread across
nodes. A Lua script that touches several keys only works if all of them are in
**one slot** — otherwise Redis rejects it with `CROSSSLOT`, because one node
can't atomically operate on keys held by another. `readCheckScript` touches the
cache key and the lock key, so both carry the `{user:<id>}` tag.
(`leaseSetScript`, `releaseLeaseScript` and `versionSetScript` touch only the
cache key; the pipelined pre/post-DB scripts touch both.)

**Caveats:**
- Tag per user, not per feature — a single shared tag like `{cache}` would put
  every key on one node and hotspot it.
- Single-instance Redis: braces are just characters in the name and do nothing.
  Using them from the start costs nothing and avoids renaming keys if you move
  to Cluster later.

- [ ] Cache and lock keys share a `{user:<id>}` hash tag

#### For Cluster

With the tags in place the same code works with or without Cluster. Moving to
Cluster also needs:

- **Cluster-aware client** — in Go, `redis.NewClusterClient` (or
  `UniversalClient`) instead of a single-node client, so commands are routed to
  the node that owns the key's slot.
- **Script loading per node** — each node has its own script cache, so
  `EVALSHA` can return `NOSCRIPT` on a node that hasn't seen the script.
  go-redis `redis.NewScript(...).Run()` falls back to `EVAL` automatically; with
  raw `EVALSHA`, handle `NOSCRIPT` by retrying with `EVAL`.
- **Failover** — replication is async, so a node failover can lose a recent lock
  or DEL (same residual risk as a single primary + replica; Cluster doesn't make
  it worse).
- **Partial outage** — if one node is down, only users whose keys live on it
  take the Redis-down fallback; everyone else is unaffected.
- **Optional pub/sub wake-up (if ever added)** — use sharded `SPUBLISH` /
  `SSUBSCRIBE` (Redis 7+), which stays within the key's slot.

- [ ] Cluster-aware client
- [ ] Scripts run via `Script.Run` (or `NOSCRIPT` → `EVAL` fallback)

### N2. Non-existent users: cache "not found"

If the DB returns no row, the lease holder has nothing to write. Every read of
that ID would then loop lease → DB → nothing, forever — wasted DB reads and an
easy abuse path (hammer random IDs).

Cache a negative entry through the same `leaseSetScript`, with a short TTL:

```
{user:999} = "0\n"   EX 30      ← version 0, empty body = "known missing"
```

- Decode `0\n` as "not found" (return not-found without touching DB). Keep it
  distinct from the lease (`L:` prefix) and from real data (version ≥ 1).
- If the user is later created, the create path must DEL the key (same as any
  write), which clears the negative entry.

- [ ] Negative entries written via `leaseSetScript`, short TTL (e.g. 30s)
- [ ] Create path DELs the cache key

### N3. Redis eviction can remove the lock

Lock, lease, and data keys all carry TTLs, so under memory pressure a
`volatile-*` or `allkeys-*` policy can evict the **lock** mid-write — a second
writer could then enter. (An evicted lease or data key is harmless: the fill is
dropped, or a reader refills it.)

- [ ] Leave memory headroom (`maxmemory` well above working set)
- [ ] Alert on `evicted_keys` > 0 from `INFO stats`

### N4. Release the lease when the DB read fails — no watchdog

On a DB error (or any error after GRANTED), delete the lease **only if it's
still ours**, so waiters don't sit out the full LeaseTTL:

```lua
-- releaseLeaseScript: KEYS[1] = cache key, ARGV[1] = token
if redis.call('GET', KEYS[1]) == 'L:' .. ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
```

Run it from a `defer` in `fetchAndFill`, skipped once `leaseSetScript` has
succeeded. It's safe to call even after a successful fill — the slot no longer
holds our lease, so it's a no-op.

**Why the lease gets no watchdog (unlike the lock):**

| | Lock | Lease |
|---|---|---|
| Expires while holder still working | **Correctness problem** (P8 — second writer enters) | **No problem** — holder's fill is dropped, data still returned to its caller |
| Holder crashes | Readers blocked up to LockTTL | Waiters blocked up to LeaseTTL, then one gets GRANTED |
| Duration of the work | Varies (writes can be slow) → watchdog needed | Bounded by the DB query timeout |

The watchdog exists to stop expiry mid-work, and lease expiry mid-work is
harmless. So the lease just needs a **short** TTL, not a long TTL plus a
watchdog:

- Process crash (no defer runs): waiters for **that key only** wait ≤ LeaseTTL,
  then one takes over. With LeaseTTL ≈ DB query timeout + margin (e.g.
  300–500ms for a PK SELECT), that's a rare, bounded, per-key delay.
- Error path (process alive): the defer releases immediately.
- A watchdog would add a goroutine per cache miss — misses come in bursts (cold
  start, mass expiry), so that's the worst place to add goroutines.

- [ ] `releaseLeaseScript` (check-and-DEL) in a defer after GRANTED
- [ ] DB query timeout < LeaseTTL (see decision points)
- [ ] No lease watchdog

### N5. Metrics

- [ ] Lease GRANTED / rejected (populate returned 0) / LEASED waits / uncached fallbacks
- [ ] A spike in lease rejects → writes racing reads, or LeaseTTL too short
- [ ] A spike in uncached fallbacks → lease holders dying or DB slower than LeaseTTL

### N6. Testing the races deterministically

The races only show up under specific timing. Don't rely on `time.Sleep` in
tests — add hooks the tests can block on:

```go
type hooks struct {
    afterDBRead    func() // reader: between DB SELECT and leaseSet
    afterDBUpdate  func() // writer: between DB UPDATE and write-through
}
```

A test pauses the reader in `afterDBRead`, runs a full write (or a write whose
write-through is forced to fail), then releases the reader and asserts the
populate returned 0 and the cache holds the new version (or is empty).

- [ ] Hooks in reader and writer paths (nil in production)
- [ ] Each §Reader Cache-Populate scenario has a hook-driven test

---

## Implementation Checklist

### Write path (in order)
- [ ] 1. Acquire lock (SETNX with TTL, unique writer_id)
- [ ] 1b. Start watchdog goroutine (cancel on exit)
- [ ] 2. Save current cache value (GET or GETDEL)
- [ ] 3. Delete cache (DEL — M7 delete-first ordering)
- [ ] 4. DB UPDATE
  - [ ] On fail: attempt cache restoration from saved value → release lock → return error
- [ ] 5. Cache write-through (Lua version-compare SET — M5+M9)
  - [ ] On fail: accept empty cache → release lock → return success (DB write succeeded)
- [ ] 6. Release lock (Lua ownership-check DEL)
- [ ] 6b. Cancel watchdog goroutine
- [ ] All of 1b, 6, 6b in defer/finally to ensure cleanup on any exit

### Read path (in order)
- [ ] 8. GET cache → real data? serve immediately (M8). Lease value = miss.
- [ ] 9. MISS → `readCheckScript` (lock check + lease grant)
  - [ ] HIT → serve
  - [ ] LOCKED / LEASED → poll/wait → retry (bounded)
  - [ ] GRANTED → step 10
  - [ ] MAX WAIT exceeded → DB SELECT, return **without caching**
- [ ] 10. Fresh DB SELECT (the whole miss path 9–10b inside singleflight if M12 implemented)
- [ ] 10b. `leaseSetScript` — populate only if the slot still holds our lease (M13)
- [ ] Return data

### Infrastructure
- [ ] Redis fallback (degrade to Simple on Redis down)
- [ ] Lua scripts via `redis.NewScript(...).Run()` (EVALSHA + NOSCRIPT fallback)
- [ ] Lock TTL configured (benchmarked against write latency)
- [ ] Watchdog interval configured (TTL/3)
- [ ] LeaseTTL configured (DB query timeout + margin, e.g. 300–500ms)
- [ ] Lease token = startup instance ID (`crypto/rand`) + atomic counter
- [ ] Monitoring: lock acquisition failures, watchdog extensions, Redis
      degradation events, cache hit/miss rates during writes, lease rejects
      (a spike means writes racing reads, or LeaseTTL too short)
- [ ] Singleflight around the miss path (optional but recommended)

### Pipelining (if chosen)
- [ ] Pre-DB Lua: SETNX + GET + DEL in one script
  - Handle SETNX failure inside the script (return error, don't proceed)
- [ ] Post-DB Lua: version-compare SET + ownership-check DEL lock in one script
- [ ] Test: pipelined behavior matches naive behavior for all failure paths

### Testing checklist
- [ ] Happy path: write succeeds, cache populated, readers get cache hit
- [ ] DB failure: cache restored (or empty), readers get correct data
- [ ] Redis failure during write: graceful degradation to Simple
- [ ] Writer crash: watchdog stops, TTL expires, readers unblock
- [ ] Concurrent writers: serialized by lock, correct ordering
- [ ] Slow reader (outside lock): lease voided by writer DEL → populate rejected (P2)
- [ ] M8: reader during write gets cache hit (before DEL)
- [ ] M9: reader after unlock gets cache hit (write-through)
- [ ] Lock TTL expiry mid-write: watchdog extends (or if watchdog dead, TTL expires safely)
- [ ] Lua script correctness: version compare edge cases (no key, equal version, higher version, lease value)
- [ ] Scenario 1: slow reader during writer's DB→write-through gap → lease rejected (see §Reader Cache-Populate below)
- [ ] Scenario 2: slow reader after writer's write-through failure → lease rejected (see §Reader Cache-Populate below)
- [ ] Writer process killed after DB commit: readers wait out lock TTL, slow reader rejected, next reader leases and caches N+1
- [ ] Writer process killed before DB commit: next reader leases and caches N (no lost cache time)
- [ ] Reader stalls past LeaseTTL: populate rejected, another reader can lease
- [ ] N concurrent misses: exactly one GRANTED, rest LEASED → one DB SELECT
- [ ] Lease holder's DB SELECT fails: waiters unblock after LeaseTTL (or immediately if the lease is released)

---

## Reader Cache-Populate Failure Modes

Before M13, readers populated the cache via the M5 version compare, which
stops a slow reader from overwriting newer data. Two scenarios existed where
that protection was insufficient. Both are on the reader's cache-populate path —
the code that runs after a cache miss and DB read, and writes the result back
to cache.

**Resolution: both are closed by M13 (reader lease) — Option E below.** The
earlier fixes and options are kept as the record of what was considered.

### Scenario 1: Stale write while lock is held (FIXED — now by lease)

**The race:**
1. Reader: cache miss, no lock → DB SELECT → gets version N
2. Writer: lock → DEL cache → DB UPDATE (N→N+1) — cache is now empty
3. Slow reader: versionSet(N) → cache empty, no version to compare → **writes
   stale N**
4. Writer: write-through N+1 → N+1 > N → overwrites → correct (brief window)

**Fix:** Use a separate Lua script for reader cache-populate that atomically
checks the lock before writing:

```lua
-- readerVersionSetScript
-- KEYS[1] = cache key, KEYS[2] = lock key
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1  -- lock held by writer, skip
end
-- normal version compare + SET follows
```

Implementation notes:
- Separate Lua script for reader (checks lock) vs writer (no lock check)
- Writer's write-through uses `versionSetScript` (original, no lock check)
- Reader's cache-populate uses `readerVersionSetScript` (lock-aware)
- Both loaded via SCRIPT LOAD at startup, invoked via EVALSHA
- Atomic in Redis — no TOCTOU gap

> **Superseded:** with M13 the reader's lease is voided by the writer's DEL, so
> `leaseSetScript` rejects this write without any lock check.
> `readerVersionSetScript` is replaced by `leaseSetScript` (component 10).

### Scenario 2: Stale write after write-through failure (RESOLVED — Option E chosen)

**The race:**
1. Reader: cache miss, no lock → DB SELECT → gets version N
2. Writer: lock → DEL → DB UPDATE (N→N+1) → write-through **FAILS** → releases
   lock
3. Slow reader: readerVersionSet(N) → no lock (released) → cache empty →
   **writes stale N**

The scenario 1 fix doesn't help — the lock has been released.

**Possible fixes (A–D considered, E chosen):**

#### Option A: Accept it
- Redis was already failing (write-through is a Lua EVALSHA — if it failed,
  Redis is degraded)
- Self-heals: next reader → DB → N+1 → overwrites stale N
- Staleness window: until next DB-sourced cache populate
- (historical) Considered whether this was acceptable for the workload — not needed

#### Option B: Shortened lock TTL on write-through failure
- On write-through failure, PEXPIRE lock to `ReadOpTimeout` instead of DEL
- ReadOpTimeout = total budget for one read operation (cache GET + DB SELECT +
  cache populate) — this is the maximum time a slow reader could still be
  in-flight
- Slow reader's readerVersionSet sees the lock → skips the stale write
- After ReadOpTimeout, lock expires, cache empty, next reader populates from DB

```go
// In Write(), after write-through failure:
if err := ls.versionSet(ctx, ck, version, encoded); err != nil {
    // Shorten lock instead of releasing — keep the version barrier
    ls.rdb.PExpire(ctx, lk, ls.cfg.ReadOpTimeout)
    skipRelease = true  // defer must not DEL the lock
}
```

- Add `ReadOpTimeout` to Config
- Modify Write() to shorten lock TTL on write-through failure
- Modify the defer to conditionally skip lock release
- Tradeoff: readers blocked for up to ReadOpTimeout on this (rare) failure

#### Option C: Tombstone instead of DEL
- Replace the DEL in step 3 of the write path with a **tombstone**: a cache
  entry that has the current version but no data
- Format: `<version>\n` (version prefix, newline, empty data body)
- Acts as a version barrier: slow reader's version N is rejected by the Lua
  compare (`N >= N`)
- Read path decodes tombstone as a miss (empty data → `ok=false` in
  `decodeValue`)
- Writer's write-through overwrites tombstone with real data (N+1 > N)
- If write-through fails: tombstone stays, slow reader rejected, next reader
  hits DB → N+1 → overwrites tombstone

```
Write path with tombstone:
  1. Lock
  2. Save cache value → extract version N
  3. Write tombstone: SET key "<N>\n" EX <LockTTL>   ← replaces DEL
  4. DB UPDATE → N+1
  5. Write-through: SET key "<N+1>\n<data>" EX <CacheTTL>  ← overwrites tombstone
  6. Release lock
```

Implementation notes:
- Tombstone TTL = LockTTL (NOT CacheTTL) — crash recovery: if writer
      crashes before DB UPDATE, DB has N, tombstone has N, reader tries
      readerVersionSet(N) → `N >= N` → rejected. Tombstone must expire quickly
      so cache can be repopulated. LockTTL (e.g. 2s) not CacheTTL (e.g. 1h).
- Version source: from saved cache value (step 2). If cache was empty, need
      either an extra DB read (`SELECT row_version`) or fallback to option B.
- `decodeValue` must return `ok=false` when data portion is empty (treat
      tombstone as miss). Already natural: `len(data) == 0 → ok=false`.
- M8 (serve-if-exists): GET cache → tombstone → decoded as miss → proceeds
      to lock check. Correct.
- Writer's write-through uses plain `versionSet` (no lock check), and N+1 >
      N passes the compare, overwriting the tombstone.
- Tradeoff: no blocking, no timing dependency. But: tombstone-aware decode,
      Lua doesn't change (version compare already handles it), need version
      upfront.

#### Option D: Reader lock-wait evidence (speculative)
- Idea: reader can only write to cache if it observed a lock-wait phase
  (proving it arrived after a writer started), then saw the lock release, then
  read DB → its data must be at least as fresh as the writer's
- Reader that went straight to DB (no lock seen) uses version compare as usual
- **Unresolved problems:**
  - Multiple writers: reader waited for Writer A (v5→v6), but Writer B (v6→v7)
    started after. Reader's DB read may return v6, stale relative to B's v7.
  - Lock identity: reader needs to know WHICH writer it waited for. Requires
    tracking writer_id through the read path.
  - TOCTOU: lock could change hands between reader's wait-end and its
    cache-populate.
  - Does not fix scenario 2 directly: writer released the lock (write-through
    failed), reader sees no lock → no "evidence" to use.
- **STATUS (historical): needed further discussion. Interesting direction but significant
      nuances with multiple concurrent writers.** Not pursued — Option E gets
      the "my DB read came after the writer" proof from a token instead.

#### Option E: Reader lease (M13) — CHOSEN

- On a miss, the reader places `L:<token>` into the empty slot (component 9),
  reads DB, then populates only if the slot still holds its token
  (component 10)
- The writer's existing DEL (component 3) wipes the lease, so the slow reader
  in scenario 2 is rejected — whether the lock is still held or not, and
  whether the writer failed on the error path or the process died

```
1. Reader: miss → GRANTED lease L1 → DB SELECT → version N → stalls
2. Writer: lock → DEL (L1 gone) → DB UPDATE (N→N+1) → write-through FAILS → unlock
3. Slow reader: leaseSet(L1) → slot empty, not L1 → REJECTED ✓
4. Next reader: GRANTED L2 → DB → N+1 → leaseSet(L2) → cached ✓
```

Why E over the others:
- vs A: no stale window at all
- vs B: no timing bound, no reader blocking on failure, and covers process death
  (a dead writer can't shorten its lock)
- vs C: no version needed up front, and crash-before-DB doesn't lose cache time
  (tombstone{N} rejects the correct N; a lease doesn't care about versions)
- vs D: no lock-history reasoning, works with any number of writers
- Bonus: one DB read per cold miss across all instances (P3)

Implementation notes:
- [ ] `readCheckScript` grants leases (component 9)
- [ ] `leaseSetScript` replaces `readerVersionSetScript` (component 10)
- [ ] Decode `L:` values as a miss everywhere the cache is read (component 8)
- [ ] Writer path unchanged — the DEL in component 3 is the invalidation
- [ ] Remove the Option B `PExpire` / `skipRelease` logic if it was added — not needed
- [ ] Residual (out of scope): writer paused past its lock TTL → fence the DB
      write with `UPDATE … WHERE id = ? AND row_version = ?`
