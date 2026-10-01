# Full Lock Stack — Implementation Component Guide

A practical checklist for implementing the full lock stack
(M2b+M3+M5+M7+M8+M9+M10). Each component is listed with what it does, why it
exists, what it depends on, and implementation notes for Go.

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

- [ ] **Reader wait mechanism?**
  - Poll/retry (simple, wastes some Redis ops)
  - Lua check-and-get (one round trip, atomic)
  - Recommended: Lua script (check lock + GET cache in one call)

- [ ] **Process health sniffing in watchdog?** (optional, see component 7b)

---

## Write Path Components

### 1. Lock Acquisition (M2b)

**What:** Acquire a per-key Redis lock with TTL before any DB/cache ops.

**Why:** Serializes writers to prevent concurrent writer race (P7). Prevents
readers from seeing intermediate state (P1).

**Key:** `user:<id>:lock`

**Redis command:** `SET user:<id>:lock <writer_id> EX <TTL> NX`
- `NX`: only set if not exists (acquire only if free)
- `EX <TTL>`: auto-expire (crash safety)
- `<writer_id>`: unique value (e.g. UUID or goroutine ID) — needed to ensure
  only the lock owner can release it

**On failure (lock held by another writer):**
- Retry with backoff, or return error/queue
- Do NOT proceed without the lock

**On failure (Redis down):**
- Degrade to Simple: DB UPDATE → DEL cache (skip lock, accept ms stale)
- See component 12 (fallback)

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

**Redis command:** `GET user:<id>` (or `GETDEL user:<id>` on Redis 6.2+ to
combine with step 3)

**What to save:** The full cached value (including version if using
version-in-value). Store in a local variable — only needed for the duration of
this write operation.

**Implementation notes:**
- [ ] Store result in local variable (not shared state)
- [ ] Handle cache miss (key didn't exist) — nothing to save, nothing to restore
- [ ] If using pipelining: this is inside the pre-DB Lua script

---

### 3. Cache Deletion — Delete-First Ordering (M7)

**What:** Delete the cache entry BEFORE updating the DB.

**Why:** Safety net. If the DB UPDATE fails after this point, cache is empty →
readers fall through to DB → get correct (old) data. Never stale. Compare to
DB-first: if DB succeeds but cache SET fails → stale.

**Redis command:** `DEL user:<id>`

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

**Redis command:** `SET user:<id> <saved_value>` (with the old TTL if applicable)

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
- M5 (version-in-value): Lua version compare prevents a slow reader (outside
  any lock) from overwriting this newer data later (P2).

**Lua script:**
```lua
-- KEYS[1] = cache key (user:<id>)
-- ARGV[1] = new version
-- ARGV[2] = serialized new data (JSON, msgpack, etc.)

local current = redis.call('GET', KEYS[1])
if current then
  local current_version = -- parse version from current
  if current_version >= tonumber(ARGV[1]) then
    return 0  -- reject: current is same or newer
  end
end
redis.call('SET', KEYS[1], ARGV[2])
return 1  -- success
```

**On failure (Redis error, Lua error):**
- Cache stays empty (DEL already ran in step 3). Readers hit DB. Correct.
- The DEL-first ordering (M7) is the safety net here.
- Release lock and return success (the DB write succeeded — that's what matters).

**Implementation notes:**
- [ ] Lua script should be loaded once (`SCRIPT LOAD`) and called via `EVALSHA`
- [ ] Version must be embedded in the cached value
- [ ] The Lua compare must handle: key doesn't exist (always write), key exists
      with lower version (write), key exists with same/higher version (reject)
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

---

### 9. Lock Check + Wait (reader blocked path)

**What:** When cache is empty (miss), check if a lock is held. If locked, wait
for it to release. If not locked, proceed to DB.

**Why:** If locked, a writer is in progress and will populate the cache (M9).
Waiting avoids a DB round trip — after unlock, the reader gets a cache hit.

**Lua script (atomic check-and-get):**
```lua
-- KEYS[1] = cache key
-- KEYS[2] = lock key

-- Try cache first (M8 — may have been populated while we were waiting)
local data = redis.call('GET', KEYS[1])
if data then
  return data
end

-- Cache miss — check lock
if redis.call('EXISTS', KEYS[2]) == 1 then
  return '__LOCKED__'  -- signal to caller: wait and retry
end

return nil  -- no cache, no lock → normal miss, go to DB
```

**Wait mechanism (on LOCKED response):**
- Poll: sleep briefly (e.g. 1-2ms), retry the Lua script
- Max retries: bound the wait (e.g. TTL × 2) to prevent infinite blocking
- After max retries: fall through to DB (correct, just slower)

**Implementation notes:**
- [ ] Lua script returns three states: data (serve), LOCKED (wait), nil (DB miss)
- [ ] Poll interval should be short but not spin-loop (1-2ms is reasonable)
- [ ] Set a max wait time — don't block forever
- [ ] After unlock, the first GET in the Lua script should catch the M9 cache hit
- [ ] Consider singleflight (M12) on the DB fallback path (after max retries)

---

### 10. Cache Population on Miss (reader cold path)

**What:** When a reader misses cache and no lock is held, read from DB and
populate cache with version-compare Lua (M5).

**Why:** Standard cache-aside repopulation. The version-compare Lua prevents
a slow reader from overwriting newer data (P2).

**Flow:**
```
1. DB SELECT → get data with version
2. Lua SET with version compare (same script as step 5)
3. Return data to caller
```

**Implementation notes:**
- [ ] Use the SAME Lua script as step 5 (write-through) for the version compare
- [ ] Consider singleflight (M12) on the DB SELECT — if N readers all miss
      simultaneously, only one does the DB read
- [ ] If the Lua SET is rejected (newer version already cached), that's fine —
      serve the data you got from DB, the cache is already correct

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
- [ ] When Redis returns: cache may be stale (same as Simple). First readers
      repopulate. Brief stale window, self-healing.
- [ ] Consider a circuit breaker for Redis to avoid repeated timeout costs

---

### 12. Singleflight / M12 (OPTIONAL but recommended)

**What:** Deduplicate concurrent DB reads for the same key.

**Why:** When cache is empty and N readers all miss, only 1 should hit DB.

**Where to apply:**
- Reader cold path (step 10): DB SELECT on cache miss
- Reader blocked path fallback (step 9): if max wait exceeded, DB SELECT

**Implementation:**
```go
var sfGroup singleflight.Group

func getUser(ctx context.Context, userID string) (*User, error) {
    val, err, _ := sfGroup.Do("user:"+userID, func() (any, error) {
        return db.SelectUser(ctx, userID)
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
- [ ] 8. GET cache → HIT? serve immediately (M8)
- [ ] 9. MISS → Lua check lock
  - [ ] LOCKED → poll/wait → retry (bounded)
  - [ ] NOT LOCKED → step 10
  - [ ] MAX WAIT exceeded → step 10 (fall through to DB)
- [ ] 10. DB SELECT (with singleflight if M12 implemented)
- [ ] 10b. Lua version-compare SET to populate cache (M5)
- [ ] Return data

### Infrastructure
- [ ] Redis fallback (degrade to Simple on Redis down)
- [ ] Lua scripts loaded via SCRIPT LOAD / EVALSHA
- [ ] Lock TTL configured (benchmarked against write latency)
- [ ] Watchdog interval configured (TTL/3)
- [ ] Monitoring: lock acquisition failures, watchdog extensions, Redis
      degradation events, cache hit/miss rates during writes
- [ ] Singleflight on DB read paths (optional but recommended)

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
- [ ] Slow reader (outside lock): Lua version-compare rejects stale write (P2)
- [ ] M8: reader during write gets cache hit (before DEL)
- [ ] M9: reader after unlock gets cache hit (write-through)
- [ ] Lock TTL expiry mid-write: watchdog extends (or if watchdog dead, TTL expires safely)
- [ ] Lua script correctness: version compare edge cases (no key, equal version, higher version)
- [ ] Scenario 1: slow reader stale write during writer's DB→write-through gap (see §Reader Cache-Populate below)
- [ ] Scenario 2: slow reader stale write after writer's write-through failure (see §Reader Cache-Populate below)

---

## Reader Cache-Populate Failure Modes

The version-compare Lua script (M5) prevents a slow reader from overwriting
newer data in cache. But two scenarios exist where the protection is
insufficient. These affect the reader's `readFromDB` path — the code that runs
after a cache miss, DB read, and attempts to write the result back to cache.

### Scenario 1: Stale write while lock is held (FIXED)

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
- [ ] Separate Lua script for reader (checks lock) vs writer (no lock check)
- [ ] Writer's write-through uses `versionSetScript` (original, no lock check)
- [ ] Reader's cache-populate uses `readerVersionSetScript` (lock-aware)
- [ ] Both loaded via SCRIPT LOAD at startup, invoked via EVALSHA
- [ ] Atomic in Redis — no TOCTOU gap

### Scenario 2: Stale write after write-through failure (OPEN — needs design decision)

**The race:**
1. Reader: cache miss, no lock → DB SELECT → gets version N
2. Writer: lock → DEL → DB UPDATE (N→N+1) → write-through **FAILS** → releases
   lock
3. Slow reader: readerVersionSet(N) → no lock (released) → cache empty →
   **writes stale N**

The scenario 1 fix doesn't help — the lock has been released.

**Possible fixes (choose one before implementing):**

#### Option A: Accept it
- Redis was already failing (write-through is a Lua EVALSHA — if it failed,
  Redis is degraded)
- Self-heals: next reader → DB → N+1 → overwrites stale N
- Staleness window: until next DB-sourced cache populate
- [ ] Decide: acceptable for this workload?

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

- [ ] Add `ReadOpTimeout` to Config
- [ ] Modify Write() to shorten lock TTL on write-through failure
- [ ] Modify the defer to conditionally skip lock release
- [ ] Tradeoff: readers blocked for up to ReadOpTimeout on this (rare) failure

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
- [ ] Tombstone TTL = LockTTL (NOT CacheTTL) — crash recovery: if writer
      crashes before DB UPDATE, DB has N, tombstone has N, reader tries
      readerVersionSet(N) → `N >= N` → rejected. Tombstone must expire quickly
      so cache can be repopulated. LockTTL (e.g. 2s) not CacheTTL (e.g. 1h).
- [ ] Version source: from saved cache value (step 2). If cache was empty, need
      either an extra DB read (`SELECT row_version`) or fallback to option B.
- [ ] `decodeValue` must return `ok=false` when data portion is empty (treat
      tombstone as miss). Already natural: `len(data) == 0 → ok=false`.
- [ ] M8 (serve-if-exists): GET cache → tombstone → decoded as miss → proceeds
      to lock check. Correct.
- [ ] Writer's write-through uses plain `versionSet` (no lock check), and N+1 >
      N passes the compare, overwriting the tombstone.
- [ ] Tradeoff: no blocking, no timing dependency. But: tombstone-aware decode,
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
- [ ] **STATUS: needs further discussion. Interesting direction but significant
      nuances with multiple concurrent writers.**
