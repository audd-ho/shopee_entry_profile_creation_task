# DB + Cache Consistency — Presentation Reference

How to keep a cache (Redis) and database (MySQL) consistent under concurrent
reads and writes. Covers the problems, the mechanisms to solve them, and which
combinations work best for which workloads.

---

## 1. The Core Problems

When a system uses both a DB (source of truth) and a cache (fast reads), any
write must update both. Since they can't be updated atomically (two separate
systems), a window exists where they disagree. Every problem below stems from
this fundamental constraint.

| ID | Problem | What happens | Staleness impact |
|---|---|---|---|
| P1 | Stale read during write | Reader gets old cache while DB is being updated | Short — ms, self-healing |
| P2 | Slow-reader overwrite | Slow reader writes stale data to cache after a newer write completed | Long — until TTL or next write |
| P3 | DB stampede | All readers hit DB at once after cache is cleared | Load issue, not staleness |
| P4 | Cache not functioning | Cache constantly bypassed, every reader hits DB | Correct but slow |
| P5 | Orphaned state | Version/DB/cache disagree after partial failure | Until next successful write |
| P6 | Reader starvation | Readers blocked indefinitely | Availability, not staleness |
| P7 | Concurrent writer race | Two writers update cache in wrong order | Long — until TTL or next write |
| P8 | Lock expires mid-write | Lock disappears while write still in progress | Varies |
| P9 | Staleness guarantee | Can a reader ever see data not matching DB? | Aggregate of P1, P2, P5, P7 |

---

## 2. The Mechanisms

### M1: DB→delete (no lock)

Writer updates DB first, then deletes cache. Next reader repopulates.

```
Write: DB UPDATE → DEL cache
Read:  GET cache → hit? return. miss? → DB SELECT → SET cache → return.
```

- Simplest possible approach
- DEL is idempotent — safe under retries and concurrent writers
- Small stale window (P1) between DB commit and DEL (~0.1-0.5ms)

### M2a: Lock, manual unlock only

Per-key Redis lock. Writer acquires before any DB/cache ops, releases after.

```
Write: SETNX lock → cache/DB ops → DEL lock
Read:  lock exists? wait. gone? normal read.
```

- Prevents P1 (stale during write) and P7 (concurrent writer race)
- **Risk:** writer crash = lock held forever = readers starve (P6)

### M2b: Lock with TTL

Same as M2a but lock auto-expires (`SET lock EX 5 NX`).

- Fixes P6: crash → lock expires → readers unblock
- **Risk:** TTL shorter than write time → lock expires mid-write (P8)

### M2c: Lock in the slot (writer lease) — variant of M2b

The lock lives in the cache key itself: the writer puts `LW:<writer_id>` (TTL +
watchdog) into the slot that holds data or a reader lease (`LR:<token>`).

```
Acquire (Lua): slot LW:* → wait; else save old data, SET LW:<id> PX <TTL>   (= lock + invalidate)
Finish (Lua):  version-compare SET new data                                 (= write-through + unlock)
```

- Not a new problem-fixing mechanism — a different place for the lock
- Fewer steps and failure modes, single-key scripts; a slot only locks itself
- **Implemented** — this is how `internal/lockstack` stores its lock (§9 follow-ups)

### M3: Watchdog + TTL

Background goroutine periodically extends the lock TTL while the writer is
alive. If writer crashes, watchdog stops, TTL expires.

```
Writer:   SET lock EX 5 NX → do work → DEL lock
Watchdog: every 1s → writer alive? EXPIRE lock 5. dead? stop.
          extensions clipped so the lock expires WatchdogTTL after the write started.
```

- Fixes both P6 (starvation) and P8 (premature expiry)
- Normal writes (~2-5ms) finish before watchdog ever fires — near-zero cost
- **WatchdogTTL caps the watchdog itself.** A *hung* writer keeps its process
  (and the watchdog) alive, so an uncapped watchdog extends forever and readers
  starve. With the cap, each extension is `min(LockTTL, deadline − now)`, so
  **WatchdogTTL is the total hold** — the lock expires exactly WatchdogTTL after
  the write started (or at the request's deadline, if earlier — the lease is
  clipped to it). Set it well above the slowest real write but at most the
  request timeout (4s with a 2s LockTTL; the HTTP side gives up at 5s)

### M4: Version-in-key

Cache key includes a version: `user:<id>:v<N>`. Different versions = different
keys. A write to `v5` can never overwrite `v6`.

```
Old: user:123:v5 → {data}
New: user:123:v6 → {data}    (different key, no collision)
```

- Prevents P2 (slow-reader overwrite) by construction
- Reader must know the version to form the key (from session, DB)
- Awkward with locks: reader after unlock doesn't know new version

### M5: Version-in-value + Lua

Plain cache key, version embedded in value. Lua script compares version
atomically before writing to cache.

```
Read:  GET user:<id> → plain GET, no compare
Cache write (after DB read):
  Lua: if current.version >= new_version → reject. else → SET.
```

- Prevents P2 on the cold path only (cache population) — but not a slow reader
  writing into an **empty** slot (nothing to compare; see §9)
- No read-path overhead — comparison only on cache writes
- Works naturally with locks (plain keys, no version in key name)

### M6: CAS — version counter (INCR → DB → SET)

*Naming:* earlier drafts called M6 "CAS/lease". It is not a lease — it is a
version counter with compare-and-set — and is unrelated to M13 (reader lease).

Version counter in Redis, incremented before DB write. Every read compares
cache version against counter.

```
Write: v = INCR version → DB UPDATE … WHERE row_version < v → SET cache {v, data}
Read:  MGET version cache → match? return. mismatch? → DB.
Fill:  DB SELECT {data, row_version r} → cache only if r == counter
```

- Only single mechanism that prevents both P1 and P2 without a lock
- Read-path overhead: version compare on every read
- Cache degrades under write pressure (constant mismatch → DB fallback, P4)
- Orphaned INCR on failure (P5) — self-heals on next successful write
- Requirements (each prevents a stale path):
  - **Conditional DB update** (`WHERE row_version < v`) — INCR order ≠ commit order
  - **One `MGET`**, not two GETs — two GETs can straddle a write
  - **Readers fill by the DB row's `row_version`** — not the counter they saw
  - Rare: **counter going backwards** (Redis losing recent writes) — failover
    → stale until next write attempt (the lock stack shares this); restart /
    eviction → writes rejected, cache unusable. Seed from DB or random, never evict

### M7: Delete-first ordering (inside lock)

Ordering decision: inside a lock, delete cache BEFORE DB update.

```
Inside lock: DEL cache → DB UPDATE → unlock
If DB fails: cache empty → readers hit DB → correct (not stale)
```

- Safety net: any failure after DEL leaves cache empty, never stale (unless a
  slow reader refills the empty slot — closed by M13, §9)
- Compare to DB-first inside lock: DB succeeds, SET fails → stale

### M8: Serve-if-exists optimization

If cache entry exists during a lock, serve it — DB hasn't changed yet (because
cache delete is the first step inside the lock).

```
Reader during write:
  GET cache → HIT? serve immediately (correct — DB unchanged)
  GET cache → MISS? check lock → wait
```

- Reduces unnecessary blocking
- Readers only block when they must (cache miss + lock held)

### M9: Write-through inside lock

After DB update (inside lock), populate cache before unlocking. Readers after
unlock get a cache hit instead of all hitting DB.

```
Inside lock: DEL cache → DB UPDATE → SET cache {new data} → unlock
```

- Prevents P3 (DB stampede) — cache populated before readers unblock
- Safe inside lock (writers serialized — no concurrent writer race)
- DEL first (M7) is the safety net: if SET fails, cache empty, correct

### M10: Invalidate-on-write (lazy loading)

Delete cache after write. Next reader pays repopulation cost.

```
Write: DB UPDATE → DEL cache
```

- Lazy: first reader repopulates
- DEL idempotent → safe under concurrency, no P7
- Standard cache-aside pattern

### M11: Write-through (no lock, eager loading)

Update cache with new data after write. No lock.

```
Write: DB UPDATE → SET cache {new data}
```

- Eager: writer pays cost, readers benefit immediately
- **Unsafe:** concurrent writers can SET in wrong order (P7)

### M12: Singleflight (request coalescing)

In-process deduplication: concurrent requests for the same key in one app
instance share one call (DB SELECT or Redis poll).

- Mitigates P3: N concurrent misses → 1 DB read **per instance** (not across instances)
- Shared result means shared error
- Composable with any approach; no write-path impact

### M13: Reader lease — CHOSEN

On a cache miss, the reader places a unique token into the empty slot *before*
reading the DB. It may populate the cache only if the slot still holds that
token. The writer's DEL-first step (M7) wipes the token.

```
Read hit:  plain GET (a value starting "L:" is a lease → treat as miss)
Read miss: readCheck Lua → HIT (serve) | LOCKED (wait) | LEASED (wait)
                          | GRANTED (it has SET cache "L:<token>" PX <LeaseTTL>)
           on GRANTED → fresh DB SELECT
           → leaseSet Lua: cache == "L:<token>"? → SET cache {data} EX <CacheTTL> : drop
Write:     lock → DEL cache (voids leases) → DB UPDATE → SET cache → unlock
```

LeaseTTL = how long a lease lives (DB query timeout + margin, e.g. 300–500ms).
CacheTTL = normal lifetime of a cached value.

- Rule: **readers never write into an empty slot, only into their own lease**
- Closes the gap M5 can't: slow reader refilling the empty slot after a failed writer
- Token: random instance ID + atomic counter; must never repeat
- Bonus: other readers wait on the lease → one DB read per cold miss (P3)
- The "lease" from Facebook's memcache paper

---

## 3. Mechanism Comparison Table

| Mechanism | Fixes | Introduces | Read cost | Write cost | Failure |
|---|---|---|---|---|---|
| M1: DB→delete | — | P1 (ms), P3 | 1 GET | 1 DEL | DEL idempotent |
| M2a: Lock manual | P1, P7 | P6 (starvation) | Lock check | SETNX + DEL lock | Held forever on crash |
| M2b: Lock TTL | P1, P7, P6 | P8 (mid-expire) | Lock check | SETNX EX + DEL lock | Expires safely |
| M2c: Lock in slot (writer lease) | P1, P7, P6 | Slot locks only itself | Prefix check | Acquire Lua + finish Lua | Expires safely |
| M3: Watchdog | P6, P8 | Complexity | None extra | Periodic extends (total hold ≤ WatchdogTTL) | TTL expires safely (crash, or hang at WatchdogTTL) |
| M4: Version-in-key | P2 | Need version for key | None if version known | None extra | Old keys linger |
| M5: Version-in-value Lua | P2 (not into an empty slot) | Lua complexity | None (plain GET) | Lua on population | Atomic, safe |
| M6: CAS (version counter) | P1, P2 | P4, P5 | Compare EVERY read (1 MGET) | INCR + SET (+ conditional DB update) | Orphaned version |
| M7: Delete-first | Safe failure order | — | None | None extra | Cache empty = correct |
| M8: Serve-if-exists | Unnecessary blocking | — | None extra | None | — |
| M9: Write-through in lock | P3 (stampede) | Lock ~0.2ms longer | None | 1 extra SET | SET fails → empty |
| M10: Invalidate (lazy) | P7 (DEL idempotent) | P1, P3 | None | 1 DEL | Safe |
| M11: Write-through (eager) | P3 | P7 (writer race) | None | 1 SET | Wrong order = stale |
| M12: Singleflight | P3 (per instance) | Shared errors | None extra | None | One call's error reaches all waiters |
| M13: Reader lease | P2 incl. write-into-empty slot, P3 on cold miss | Lease grant on miss | None on hit; no extra RT on miss vs lock stack (M5) (grant is inside readCheck) | None (DEL voids leases) | Lease expires → fill dropped, re-lease |

---

## 4. Key Combinations

Three named combinations are referenced throughout this document. Each is a
specific set of mechanisms chosen to work together:

| Name | Mechanisms | What it is |
|---|---|---|
| **Simple** | M1 + M10 + M4 | DB→delete + invalidate-on-write + version-in-key. No lock, no Lua, no watchdog. Accepts a small ms stale window. |
| **CAS** | M6 | CAS (version counter). Version counter + compare on every read. No lock. Zero stale by detecting mismatches on the read path. |
| **Lock stack + lease** — **chosen for zero stale** | M2c + M3 + M8 + M9 + M10 + M13 (M5 on the writer's SET only) | Writer lease in the cache key as the lock (TTL + watchdog) — acquiring it is the delete-first invalidation — + serve-if-exists + write-through that also unlocks + invalidate-on-write + reader lease. Zero stale by preventing inconsistency on the write path, and leases stop slow readers from refilling the cache with old data. (Earlier form: M2b + M7 with a separate lock key — superseded.) |

### Simple: M1 + M10 + M4 (DB→delete + version-in-key)

```
Write: DB UPDATE → DEL user:<id>:v<old>
Read:  GET user:<id>:v<N> → hit? return. miss? → DB → SET user:<id>:v<N>
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Small window (ms, self-healing) |
| P2: Slow-reader overwrite | Fixed by construction (different keys) |
| P3: DB stampede | Small (only on miss) |
| P7: Concurrent writers | Safe (DEL idempotent) |
| Read path (no write) | 1 GET |
| Write path | DB + 1 DEL |
| Redis write dependency | **No** — writes succeed without Redis |
| Complexity | Low |
| Staleness guarantee | ms stale possible (P1), self-healing |

### CAS (version counter): M6

```
Write: v = INCR user:<id>:version → UPDATE … SET row_version = v WHERE row_version < v
       → SET user:<id> {v, data}
Read:  MGET user:<id>:version user:<id> → match? return. mismatch? → DB.
Fill:  DB SELECT {data, row_version r} → Lua SET only if r == counter
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (mismatch detected → DB) — needs one `MGET`, not two GETs |
| P2: Slow-reader overwrite | Fixed (fill only if the DB row's `row_version` == counter) |
| P3: DB stampede | Worst (all mismatched readers hit DB) |
| P4: Cache functioning | Broken under write pressure |
| P5: Orphaned state | Until next successful write |
| P7: Concurrent writers | Fixed by the conditional DB update; the older write is lost (retry or drop) |
| Read path (no write) | 1 MGET + version compare |
| Write path | INCR + conditional DB UPDATE + SET |
| Redis write dependency | **Yes** — INCR before DB |
| Complexity | Medium on paper; close to the lock stack once all the requirements are in |
| Staleness guarantee | No stale reads; P5 = DB fallback on failure. Rare: Redis failover losing recent writes (shared with the lock stack) |

### Lock stack + lease: M2c + M3 + M8 + M9 + M10 + M13 (chosen)

```
Write: Lua acquire: SET cache "LW:<id>" (lock + invalidate; waits only on another LW:)
       → DB UPDATE → Lua SET cache (version compare) — replacing LW: is the unlock
Read:  GET cache → hit? return (even during lock — M8).
       miss? → Lua: locked? wait. leased by another? wait. else → take lease.
       lease → DB → Lua: still my lease? SET cache. else drop.
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (readers blocked or served valid cache) |
| P2: Slow-reader overwrite | Fixed (reader lease — covers the empty-slot case too) |
| P3: DB stampede | Fixed (cache populated before unlock; lease on cold miss) |
| P7: Concurrent writers | Fixed (lock serializes) |
| P6: Starvation | Fixed (watchdog + TTL) |
| Read path (no write) | 1 GET (plain, no compare) |
| Read path (during write, cache exists) | 1 GET → cache hit (serve-if-exists) |
| Read path (during write, cache miss) | Lua check → block → cache hit after unlock |
| Read path (cold miss, no write) | Lua lease grant → DB → Lua lease SET |
| Write path | Acquire Lua (`LW:` = lock + invalidate) + DB + finish Lua (SET = unlock) |
| Redis write dependency | **Yes** — but degrades to simple approach if Redis down |
| Complexity | High |
| Staleness guarantee | **No stale reads** |

---

## 5. Combination Matrix — Problems Solved

| Combination | P1 | P2 | P3 | P4 | P5 | P6 | P7 | P8 | Staleness |
|---|---|---|---|---|---|---|---|---|---|
| M1+M10 (DB→delete) | ms window | Vulnerable | Small | Works | No | No | Safe | N/A | ms + long (P2) |
| M1+M10+M4 (+ version-in-key) | ms window | **Fixed** | Small | Works | No | No | Safe | N/A | **ms only** |
| M6 (CAS) | **Fixed** (MGET) | **Fixed** | **Worst** | **Broken** | Until next write | No | Fixed (conditional DB update) | N/A | **None; P5 fallback** |
| M11 (write-through no lock) | **Fixed** | Vulnerable | **Fixed** | Works | No | No | **Vulnerable** | N/A | Long (P7) |
| M2b+M3+M7+M10 (lock+watchdog) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | P2 only |
| M2b+M3+M5+M7+M10 (+ version) | **Fixed** | **Fixed** † | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** † |
| Lock stack (M5): M2b+M3+M5+M7+M8+M9+M10 | **Fixed** | **Fixed** † | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** † |
| Lock stack + lease, separate lock key (M2b form — superseded) | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** ‡ |
| **Lock stack + lease (chosen): M2c+M3+M8+M9+M10+M13, writer lease as the lock — implemented** | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** ‡ |

† Gap: slow reader refilling the empty slot after a failed writer — M5 has
nothing to compare against. M13 closes it (§9).

‡ In normal operation. Residuals: the late writer (commit after the lock
expired, no write-through) and a Redis failover losing recent writes (shared
with CAS) — §9 follow-ups.

---

## 6. Read/Write Path Costs

| Combination | Read (no write) | Read (write, cache exists) | Read (write, cache miss) | Cold miss (N readers) | Write |
|---|---|---|---|---|---|
| Simple (M1+M10+M4) | 1 GET | 1 GET (ms stale possible) | 1 GET miss → DB | N DB reads | DB + 1 DEL |
| CAS (M6) | 1 MGET + compare | 1 MGET + compare → DB | 1 MGET + compare → DB | N DB reads | INCR + DB + SET |
| Lock stack + lease | 1 GET | 1 GET → **cache hit** | Lua → block → poll → **cache hit** | **1 DB read** (lease holder), rest wait | Acquire Lua (`LW:` = lock + invalidate) + DB + finish Lua (SET = unlock) |

Per-reader cost during a write. Variables: n_r / n_d = network round trip to
Redis / DB, u = DB UPDATE processing, s = DB SELECT processing, S = poll slack
(gap between the unlock and a waiting reader's next poll):

| | Formula |
|---|---|
| Lock stack, blocked | 3/2 n_r + 1/2 n_d + 1/2 u + S (S = poll slack) |
| CAS (MGET, row-lock wait) | n_r + 3/2 n_d + 1/2 u + s |
| CAS (MGET, MVCC) | n_r + n_d + s |

Write cost (p / l = Redis simple-op / Lua processing, both tiny):

| | Formula | Redis round trips |
|---|---|---|
| Simple | n_r + p + n_d + u | 1 |
| CAS | 2n_r + 2p + n_d + u | 2 |
| **Lock stack + lease, M2c (chosen)** | **2n_r + 2l + n_d + u** — acquire Lua (lock + invalidate) + DB + finish Lua (write-through = unlock) | 2 |
| Lock stack, separate lock key, naive | 5n_r + 4p + l + n_d + u | 5 |

- M2c costs the same as CAS on writes (2l − 2p ≈ 0)
- Extras on the chosen path cost nothing normally: the watchdog only extends if
  a write outlives ~LockTTL/3; the deadline/budget check is in-process; a
  deadlock retry adds n_d + u only when MySQL rolls the write back
- Cold miss, lease holder: 3n_r + p + 2l + n_d + s (one Redis round trip more
  than Simple); waiters ≈ half the holder's fill + S — and **1 DB read total**
  instead of N
- Fallbacks: a reader that gives up waiting pays its wait + an uncached
  n_d + s; a lease-holder read slower than LeaseTTL drops its fill and lets
  another reader read again (only when the DB is that slow)

- vs CAS with row-lock wait: lock stack wins per reader when n_d + s > 1/2 n_r + S
  (DB far, Redis close); at low load with n_r ≈ n_d it's roughly a tie
- vs CAS with MVCC: CAS can be faster per reader at low load
- **As readers scale, lock stack wins:** every CAS reader is a DB query, so DB
  latency (s, and u for writers) rises with load; lock stack readers add zero DB
  load and their cost stays flat. MVCC removes the row-lock wait, not the DB trip
- The lock stack's real advantages: **zero DB load from readers during writes,
  M8 hits, cache already populated after the write, fresh data** (the CAS MVCC
  trip returns the last committed value — not stale, but the same data the cache
  already had)

---

## 7. Why the Lock's Overhead Is Minor

### Readers wait the same time either way

```
Without lock: reader waits for DB row lock → DB SELECT → return
              Total: ~2-10ms (wait + DB read)

With lock:    reader waits for Redis lock → GET cache → return
              Total: ~1.5-5.5ms (wait + cache read)
```

The wait is bounded by the DB write time in both cases. The lock moves the
wait from the expensive place (DB queue, ~1-5ms per read) to the cheap place
(Redis, ~0.1-0.5ms per read). Same wait, better outcome (locking reads; see MVCC
note below).

**In the MVCC scenario (plain, fresh SELECT):**
- The no-lock reader doesn't wait on the row lock — it gets the last committed
  value (not stale, but the same data the cache already had) in one DB round
  trip (can be faster per reader at low load)
- But every such reader is a DB query → DB load and latency grow with readers
- Lock stack reader: zero DB load, gets the **new** value, cost flat as readers scale
- Poll slack (minor): the lock stack reader notices the unlock on its next poll
  (≈ ½ the poll interval)

### Writer serialization is not "double"

Writers already serialize on the DB row lock. The Redis lock adds ~0.2-1ms
(~5-20% overhead on a ~5ms DB write). Per-user lock scope
(`{user:<id>}:lock`) means different users don't block each other.

### Redis dependency is real but manageable

```
try:    acquire lock → DEL → DB → SET → release
catch:  Redis down → fallback to DB UPDATE only (degrades to Simple)
```

During Redis outage: writes succeed, reads hit DB. When Redis returns: brief
stale window (same as Simple) if the fallback DEL succeeded; if it failed, stale
until CacheTTL. Zero-stale guarantee lost temporarily, not
permanently.

---

## 8. Race Condition Scenarios

### Simple (M1+M10+M4): slow-reader overwrite — FIXED by version-in-key

```
1. Reader A: GET user:<id>:v5 → miss → DB → v5 data → hangs
2. Writer:   DB (v5→v6) → DEL user:<id>:v5
3. Reader B: GET user:<id>:v6 → miss → DB → v6 → SET user:<id>:v6
4. Reader A: wakes → SET user:<id>:v5 (DIFFERENT KEY — harmless)
✓ Correct. v6 key untouched.
```

### Simple (M1+M10+M4): stale during write — small window

```
1. Writer: DB UPDATE (v5→v6) committed. DEL not yet sent.
2. Reader: GET user:<id>:v5 → HIT (old data) ← stale for ~0.1ms
3. Writer: DEL user:<id>:v5
4. Next reader: miss → DB → v6 → correct
✓ Self-heals in ms.
```

### CAS (M6): orphaned INCR — self-heals

```
1. Writer: INCR version → v6 → DB UPDATE fails
   State: counter=v6, DB=v5, cache={v5}. Mismatch.
2. All readers: v6 != v5 → DB fallback. Cache useless.
3. Next writer: INCR → v7, DB succeeds, SET cache {v7}.
✓ Cache works again. Gap at v6 is harmless.
```

### CAS (M6): cache broken under write pressure

```
Writes every few ms. Version counter jumps constantly.
Every reader: MGET version + cache → mismatch → DB.
Cache serves zero hits. All traffic goes to DB.
✗ Cache is fiction. DB load = no-cache load.
```

### CAS (M6): pitfalls and fixes

| Scenario | What goes wrong | Fix |
|---|---|---|
| INCR order ≠ commit order | Older write commits last; cache/counter say newer → stale | Conditional update `WHERE row_version < v` (older write lost → retry/drop) |
| Reader fills with the counter it saw | Pre-commit (MVCC) data stored under the new version → stale | Fill only if DB row's `row_version` == counter |
| Two GETs straddle a write | Counter read before INCR, cache after commit → old value returned | One atomic `MGET` |
| Counter goes backwards (rare) | Failover: replica lost the last INCR + SET → counter matches old cache → stale until next write attempt (lock stack has the same failover stale). Restart/eviction: counter restarts low → writes rejected, cache unusable | Seed a missing counter from the DB or a random value; never evict it; `WAIT` / AOF narrow the loss |
| Writer SETs after a 0-row UPDATE | After a Redis rollback the next INCR reuses a version the DB already has; UPDATE matches 0 rows; SET labels uncommitted data with a matching version → stale | SET only when exactly 1 row was affected |
| Writer SETs on an unknown outcome | Timeout mid-COMMIT, SET anyway → match on data the DB may have rolled back | SET only on confirmed success (a late commit is filled by the next reader) |
| Fallback read hits a lagging replica | Reader served old data (not cached, but served) | Fallback reads go to the primary |
| MySQL loses a committed write (shared by all designs) | Primary failover drops an acknowledged commit → counter/cache serve data the DB no longer has | Semi-synchronous replication |

With both stores durable and these rules followed, CAS can't serve stale data:
a commit to v needs the INCR to v first and the counter only moves forward.
Its stale paths are a store losing acknowledged writes (shared by all designs)
or a broken rule.

### Write-through no lock (M11): concurrent writer race — VULNERABLE

```
1. Writer A: DB UPDATE (data=A) → SET cache = A (delayed by network)
2. Writer B: DB UPDATE (data=B) → SET cache = B
3. Writer A's SET arrives late → SET cache = A (overwrites B)
   DB has B. Cache has A. STALE.
✗ Until TTL or next write.
```

### Lock stack (M5): slow reader into empty slot — VULNERABLE

```
1. Reader A: miss, no lock → DB → v5 → stalls
2. Writer:   lock → DEL cache → DB (v5→v6) → Lua SET FAILS (or process dies) → lock gone
3. Reader A: wakes → no lock, cache EMPTY → M5 has nothing to compare → SET v5
   DB has v6. Cache has v5. STALE.
✗ Until next DB-sourced populate or CacheTTL.
```

### Lock stack + lease: same race — FIXED

```
1. Reader A: miss, no lock → takes lease L1 → DB → v5 → stalls
2. Writer:   lock → DEL cache (L1 gone) → DB (v5→v6) → Lua SET FAILS → lock gone
3. Reader A: wakes → Lua: cache == L1? No (empty) → DROP
4. Reader B: miss → takes lease L2 → DB → v6 → Lua: cache == L2? Yes → SET v6
✓ Correct. Holds whether the lock is still held or not, and whether the writer
  failed on the error path or its process died.
```

### Lock stack + lease: all scenarios handled

```
Concurrent writers: serialized by lock. ✓
Slow reader: lease voided by writer's DEL → populate dropped. ✓
Writer crash after DB: lock TTL → readers wait → slow reader dropped → next reader leases v6. ✓
Writer crash before DB: lock TTL → next reader leases → caches v5 (correct, no lost cache time). ✓
Writer hangs (alive, stuck): lock expires WatchdogTTL after the write started → readers unblock. ✓
Network error (SET fails): DEL already ran → cache empty → next reader leases → DB. ✓
During write (cache exists): serve-if-exists → correct (DB unchanged). ✓
During write (cache miss): blocked → unlock → cache hit (M9). ✓
Cold miss, many readers: one lease holder reads DB, rest wait. ✓
Redis down: fallback to Simple. ✓
```

---

## 9. Closing the Last Gap — Why a Reader Lease

Gap in the lock stack with M5: a slow reader refilling the **empty** slot (left
by DEL-first) with old data after the writer's cache write fails or it dies
post-commit. Stale here = old data that **outlives the write**.

### Two kinds of writer failure

| Failure | Lock afterwards | Slow reader must stall for | Writer knows if DB committed? |
|---|---|---|---|
| Error path (SET fails, process alive, deferred unlock) | Released immediately | ~one write | Yes |
| Process death (SIGKILL, OOM, unrecovered panic) | Held until TTL | Rest of the TTL | No — it's dead |

### Options considered

| Option | Stale possible? | Cost | Timing-dependent? |
|---|---|---|---|
| Accept it | Yes, until next DB populate / CacheTTL | None | — |
| Short TTL on reader-written entries | Yes, bounded by that TTL | Constant extra DB reads on every key, all the time | Yes |
| Shorten lock to read timeout on failure | No, if the reader finishes in time | Readers blocked on failure; a dead process can't shorten anything | Yes |
| Tombstone {version} instead of DEL | No | Crash-before-DB: tombstone rejects the correct version → cache useless until it expires; needs version up front | Yes |
| **Reader lease (M13)** | **No** | **No extra round trip vs lock stack (M5) — one more SET inside the miss-path Lua** | **No** |

- Tombstone can't tell "crash before DB" from "crash after DB"
- A version asks "is my data newer?"; the lease asks "did any writer touch this
  key since my DB read?" — the actual question

### Lease design rules — what each one prevents

| Rule | Prevents |
|---|---|
| Reader writes only over its **own lease** — never over data, never into an empty slot | Old data overwriting new, and old data filling an empty slot. This is why readers need no version compare |
| Writer keeps M5's version compare on its SET | A writer paused past its lock TTL overwriting a newer writer's value (the writer's SET writes over data) |
| **One key** for lease and data → the writer's single DEL clears either | A separate lease key needs two DELs and a two-key check; missing one reopens the gap |
| **Fresh DB read after the grant** (autocommit, or a transaction begun after GRANTED) | A `REPEATABLE READ` snapshot from before the grant returning data older than a committed write, which the lease would then protect |
| Correctness from the DEL, not from lease expiry | Relying on timing. `LeaseTTL` only frees the slot if a holder dies |
| **No lease watchdog**; short `LeaseTTL`, above the DB query timeout | Waiters stuck behind a dead holder (bounded by LeaseTTL). A timeout above LeaseTTL means nothing caches while the DB is slow |
| **Plain GET on hits**, Lua only on a miss | Paying the fixed Lua cost (~2–5µs vs ~1µs) on every read, on the single shared Redis thread |
| Waiters poll with **capped exponential backoff** (0.5 → 1 → 2 → 4ms + jitter) + singleflight | Large post-unlock slack (early polls are short) and Redis overload during long waits (later polls back off, one poller per key per instance) |
| **Cache "not found"** briefly through the lease | Every read of a missing ID hitting DB forever (cache penetration / abuse) |
| Memory headroom + alert on evictions | Eviction removing the **lock** mid-write, letting a second writer in |
| Hash tags `{user:<id>}` / `{user:<id>}:lock` (separate-lock-key form only) | Cluster errors: the miss-path Lua touches both keys, which must share a slot. Not needed with the chosen M2c form — every script touches one key |

Optional, not implemented: pub/sub wake-up instead of polling (not `BLPOP`, not
an MQ) — more moving parts than this scale needs.

### Residual risks (out of scope here)

- Writer paused past its lock TTL — the late writer (follow-ups below): shared
  timeout + MySQL lock-wait timeouts (fix 1) remove its likely path; binlog
  invalidation is the general fix. A fence isn't needed for blind writes
- Redis failover losing a recent DEL (async replication)

### Writer side follow-ups

**Stopping the writer, not just the lock.** `WatchdogTTL` stops the lock being
extended; a shared timeout context stops the writer too:

| Mechanism | Covers | Leaves |
|---|---|---|
| Shared timeout context (watchdog + DB call) — **chosen** | Writer hung on a DB call | Statement already queued in MySQL; frozen process (two clocks) |
| Fix 1: MySQL `lock_wait_timeout` / `innodb_lock_wait_timeout` (1s) below `MinWriteBudget` (1.5s), which is checked before each DB attempt — **required with the shared context** | Statement queued behind a lock: fails inside MySQL before the Redis lock expires | Slow execution after getting the lock |
| Fix 2: delayed cache DEL on an unknown outcome (optional) | Anything that still commits late | Process dying before the DEL fires |
| Separate-process supervisor that kills the writer (option) | Writer that ignores its context | Same as the shared context |

- A context error during COMMIT = **unknown outcome** → don't restore the old
  cache value; leave the slot empty
- **Values that nest:** HTTP 5s > TCP request timeout 4s (the whole op; the
  lease is clipped to it) ≥ WatchdogTTL 4s (fallback cap) > MinWriteBudget 1.5s
  (least time left to *start* a DB attempt) > MySQL lock-wait 1s (MySQL's
  minimum). The TCP server refuses to start otherwise
- **Retries:** only deadlock victims (1213), max 2, each gated by
  MinWriteBudget. Lock-wait timeouts (the row stays locked), unknown outcomes
  and definite failures are returned as errors; a bad connection before
  anything was sent is already retried by the DB pool
- **Network faults:** dropped packets → just latency (TCP retransmits).
  Connection broken *before* sending → safe retry; broken *after* sending or a
  silent partition (ends at the deadline) → unknown outcome, no retry, cache
  left empty
- **The shared context alone is worse**: the writer gives up, a queued
  statement commits later, and nobody repairs the cache. Fix 1 is what makes it
  safe
- "Already sent" and "two clocks" are general to any client timeout / lease
  lock — no timeout design removes them

**The late writer — the last residual.** Writer DELs, its lock expires, a
reader leases and caches the then-current N (correct), then the writer's commit
N+1 lands and the writer doesn't write-through → stale until next write /
CacheTTL.
- Not a slow reader: the reader's data was correct when cached; no reader-side
  check (M5, M13) can see a future commit — not their fault
- In one sentence: a write executing in MySQL after its lock expired, with no
  write-through after it
- Paths: statement queued in MySQL (per incident if the writer gave up —
  removed by fix 1) and a process freeze ¹ (practically zero)
- Not unique to the lock stack: Simple faces it too ("delayed double delete" is
  its standard fix — same idea as fix 2). CAS handles it: its INCR stays, and
  the late commit writes its own `row_version`, so the next reader's fill
  matches. A CAS-style `P:<version>` marker for the lock stack was considered
  and ruled out — the writer doesn't know the new version up front
- General fix: binlog-driven invalidation (Canal / Debezium) — out of scope

¹ A freeze stops the process's timers too, so the deadline can't fire during
it, and the lock expires after LockTTL of freeze. On resume the writer either
finds the deadline passed and doesn't send, or sends and then runs its
write-through (repairs). Only a freeze in the microseconds between the deadline
check and the packet leaving gives lasting stale — fix 2 covers it.

**Expiring vs never-expiring "write in progress" marker:**

| Marker | Late writer | Writer dies mid-write |
|---|---|---|
| Expires (lock stack's lock TTL) | ✗ can be outlived → stale possible | ✓ self-heals after TTL |
| Never expires (CAS's INCR; no-TTL lock) | ✓ readers refuse to fill | ✗ CAS: cache unusable until next write; no-TTL lock: key locked forever |

- Safety vs liveness: release-only-by-holder blocks forever on a crash;
  auto-release can be outlived. Every watchdog/supervisor/heartbeat is a
  timeout in disguise — it can't tell dead from frozen
- Way out: make a late holder harmless at the resource (fence, binlog
  invalidation), not a better lock

**The fence — not needed here.** If the slot was empty at acquire there is no
version to capture; a per-key Redis fence counter works (0 extra RT, extra key
+ column, fail-closed on counter loss), a later SELECT isn't a real fence.
Nickname/avatar are blind writes — any commit order is legal, the DB is the
truth — and the fence doesn't cover the late writer anyway. Use it only for
read-modify-write updates.

**Writer lease in the slot — M2c (implemented).** Replace the lock key with `LW:<id>`
in the cache slot (readers keep `LR:<token>`):
- A variant of the lock (where it lives), not a new mechanism: same P1–P8
  results, same residuals
- Acquire = invalidate, finish = unlock → 2 Lua calls, two failure modes gone,
  every script single-key (Cluster-friendly)
- Writers overwrite `LR:` (readers never block writers), wait only on `LW:`
- Finish step must overwrite a stray `LR:` — don't require ownership
- Like packing a lock bit into the same word as the data in C++: still a lock,
  just colocated

**Multi-key writes (with M2c).** Acquire writer leases in one fixed order
(A → B → C), one deadline from the first acquire, abort + restore on timeout.
No deadlock; readers of untaken slots still hit. Needs every writer of those
keys to follow the order, accepts per-key visibility, and one DB transaction
(not across shards).

**Heavy writes.** The cache can't help a key under constant writes (any
design). Readers aren't starved: after their max wait they read the DB (MVCC →
last committed, correct, uncached). Lever: a short max wait (currently
2 × LockTTL — tune later). Options: coalesce queued last-writer-wins writes;
serving the old value during a write is rejected (breaks strict zero-stale).

**Guarantee and failure model.** No design
prevents every fault:

| Kind of fault | Preventable? |
|---|---|
| Implementation rules (SET only on confirmed 1-row update, primary reads, MGET, no restore on unknown outcome) | Yes — code correctness |
| A store losing acknowledged writes (Redis/MySQL failover, no fsync) | At the store, not the cache design (semi-sync, `WAIT`, AOF) — costs write latency |
| Timing between machines (late execution, frozen process, two clocks) | Not by any lock/timeout — only binlog invalidation or no cache |

Lock stack + lease: **no stale** under crashes, Redis/DB errors and timeouts,
concurrent writers, slow readers, Redis restart, writers hung on DB calls.
**Accepted, one key until its next write or CacheTTL:** a store losing
acknowledged writes on failover; a write executing after its lock expired
(main path removed by fix 1). The claim is "zero stale under this failure
model", not "zero stale, full stop".

**CAS vs lock stack, on staleness alone:** both share the Redis-failover stale;
CAS avoids the late writer; the lock stack recovers cleanly from a Redis
restart (CAS rejects writes until its counter catches up). On everything else
the lock stack wins. Neither reaches absolute zero — that takes no cache, or
binlog invalidation.

---

## 10. Best Fit by Workload

| Workload | Best | Runner-up | Worst |
|---|---|---|---|
| High read, low write | **Simple** — cache works, ms stale barely matters, no Redis write dep | CAS — zero stale, low overhead when writes rare | Write-through no lock — P7 risk |
| Low read, high write | **Simple** — cache rarely useful, minimal ops | Lock stack + lease — serializes cleanly | CAS — INCR+SET overhead, cache never serves hits |
| High read+write, ms stale OK | **Simple** — ms stale self-healing, lock overhead not justified | Lock stack + lease — correct but heavy | CAS — cache broken under writes, DB hammered |
| High read+write, zero stale | **Lock stack + lease** — zero stale + cache works + no stampede | CAS — zero stale, no lock (but its own complexity), cache degrades | Write-through no lock — P7 = long stale |
| Zero stale, no lock | **CAS** — only zero-stale without locks (still needs conditional update, MGET, row-version fill) | Simple — ms stale, not zero | Lock stack + lease — has locks (unwanted) |
| Redis outage tolerance | **Simple** — writes don't depend on Redis | Lock stack + lease with fallback | CAS — INCR before DB = writes blocked |

---

## 11. The Tradeoff Landscape

```
                    Zero staleness
                         ▲
                         │
         M6 CAS ────────┤──────── Lock stack + lease
         (no lock, but   │         (complex,
          its own        │
          complexity,    │
          cache degrades │          cache works,
          under writes)  │          DB protected)
                         │
                         │
         Simple ─────────┤
         (M1+M10+M4)    │
         (simplest,      │
          ms stale,      │
          always works)  │
                         │
                         └──────────────────────► Complexity
```

| | Simple | CAS | Lock stack + lease |
|---|---|---|---|
| Staleness | ms, self-healing | None | None |
| Cache under writes | Invalidated, first reader repopulates | Broken (constant mismatch) | Works (write-through inside lock) |
| DB load during writes | Medium (readers fall through) | Highest (all readers hit DB) | Lowest (readers get cache) |
| Read-path overhead | None | Compare every read | None |
| Redis write dependency | No | Yes | Yes (degrades gracefully) |
| Complexity | Low | Medium (more than it looks) | High |
| Best for | Most systems | Zero stale without locks | Zero stale + cache works under pressure |

The final question is always: **is eliminating a millisecond self-healing stale
window worth the complexity?**

- For most systems, M1+M10+M4 is the answer.
- For zero-stale without a lock (but with its own complexity), CAS.
- For zero-stale with cache that actually works under pressure, the lock stack + lease — and its overhead is minor compared to the DB write that dominates the path regardless.

---

### Note: Redis Cluster hash tags — `{…}`

**What it is:** `{…}` in a key tells Redis Cluster **which part of the key to
hash**. Only the text inside the braces is hashed; the full key name (braces
included) is still what's stored and looked up.

```
{user:123}        → hash("user:123") → slot 5474
{user:123}:lock   → hash("user:123") → slot 5474   ← same slot → same node
user:123:lock     → hash("user:123:lock") → some other slot, maybe another node
```

**Why (Cluster):** Redis Cluster splits keys into 16384 slots spread across
nodes. A Lua script, `MGET` or `MULTI` that touches several keys only works if
all of them are in **one slot** — otherwise Redis rejects it with `CROSSSLOT`,
because one node can't atomically operate on keys held by another. So:
- Lock stack + lease, chosen M2c form: the lock is a writer lease in the cache
  key, so every script touches one key → no tag needed (braces kept for stable
  keys). The separate-lock-key form (M2b) touched both → `{user:<id>}` and
  `{user:<id>}:lock`
- CAS: the `MGET` reads the counter and the cache entry →
  `{user:<id>}:version` and `{user:<id>}`
- Simple (M1+M10+M4): each operation touches one key → no tag needed;
  `user:<id>:v<N>` stays as is

**Caveats:**
- Tag per user, not per feature — a single shared tag like `{cache}` would put
  every key on one node and hotspot it.
- Single-instance Redis (or primary + replicas, no Cluster): braces are just
  characters in the name and do nothing. Using them from the start costs
  nothing and avoids renaming keys if you move to Cluster later.

---

#### Current entry task: user management system
- The current scale is ok with the (M1+M10+M4) approach of DB+delete(invalidation on write) + version in key of :v<row_version>
- The roi is not worth the effort of all the moving parts to develop, the updates and changes for the users on a user_id level is only for nickname and avatar, so it is unlikely for them to trigger the stale state which is query cache before cache invalidate (after db update), needs the nickname and avatar to change for the SAME USER within a second for this to happen, unless it is a network or some other failure which is rare and this stale level is for a user so the roi is not worth it
- More specifically, on when Simple can serve stale data: two updates within a
  second are not required. What is required is a **read** of that user's old
  cache key landing in the gap between the DB commit and the cache DEL — about
  one Redis round trip (~0.1–0.5ms same-DC) — which then self-heals once the DEL
  lands. The longer case is the DEL itself failing (Redis error or writer crash
  between DB commit and DEL): the old key can then be served until its CacheTTL
  expires, or until readers move to the new version key (version-in-key, M4).
- If zero stale is required, the chosen design is the lock stack + lease with the lock as a writer lease in the cache key (M2c+M3+M8+M9+M10+M13) — see §4, §9
- **What the codebase implements:** the lock stack + lease in that M2c form (`internal/lockstack`) — the zero-stale design was built, not Simple. The ROI points above are why Simple would also have been acceptable at this scale.
