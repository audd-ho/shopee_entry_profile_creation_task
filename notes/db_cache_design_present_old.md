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

Same as M2a but lock auto-expires (`SET lock EX <TTL> NX`).
TTL is configurable — set based on expected write latency (e.g. 500ms–2s).

- Fixes P6: crash → lock expires → readers unblock
- **Risk:** TTL shorter than write time → lock expires mid-write (P8)

### M3: Watchdog + TTL

Background goroutine periodically extends the lock TTL while the writer is
alive. If writer crashes, watchdog stops, TTL expires.

```
Writer:   SET lock EX <TTL> NX → do work → DEL lock
Watchdog: every <TTL/3> → writer alive? EXPIRE lock <TTL>. dead? stop.
(TTL and watchdog interval are configurable — tune based on observed write latency.)
```

- Fixes both P6 (starvation) and P8 (premature expiry)
- Normal writes (~2-5ms) finish before watchdog ever fires — near-zero cost

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

- Prevents P2 on the cold path only (cache population)
- No read-path overhead — comparison only on cache writes
- Works naturally with locks (plain keys, no version in key name)

### M6: CAS/lease (INCR → DB → SET)

Version counter in Redis, incremented before DB write. Every read compares
cache version against counter.

```
Write: INCR version → DB UPDATE → SET cache {version, data}
Read:  GET version → GET cache → match? return. mismatch? → DB.
```

- Only single mechanism that prevents both P1 and P2 without a lock
- Read-path overhead: version compare on every read
- Cache degrades under write pressure (constant mismatch → DB fallback, P4)
- Orphaned INCR on failure (P5) — self-heals on next successful write

### M7: Delete-first ordering (inside lock)

Ordering decision: inside a lock, delete cache BEFORE DB update.

```
Inside lock: DEL cache → DB UPDATE → unlock
If DB fails: cache empty → readers hit DB → correct (not stale)
```

- Safety net: any failure after DEL leaves cache empty, never stale
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

### M12: Request coalescing (singleflight)

When multiple goroutines/threads request the same key concurrently, only one
executes the underlying call (DB SELECT, cache GET, etc.). The rest wait for
that single result and share it.

```
N concurrent readers for key X, all cache miss:
  Without M12: N DB SELECTs
  With M12:    1 DB SELECT, N-1 wait and share the result
```

**Go:** `sync/singleflight` (stdlib `x/sync`). Keyed deduplication — concurrent
calls with the same key coalesce into one in-flight call.

```go
var group singleflight.Group

func GetUser(id string) (*User, error) {
    val, err, _ := group.Do("user:"+id, func() (any, error) {
        return db.SelectUser(id)
    })
    return val.(*User), err
}
```

**Java:** `CompletableFuture` with `ConcurrentHashMap.computeIfAbsent` — store a
future per key, subsequent callers join the existing future.

```java
ConcurrentHashMap<String, CompletableFuture<User>> flights = new ConcurrentHashMap<>();

CompletableFuture<User> getUser(String id) {
    return flights.computeIfAbsent("user:" + id, k ->
        CompletableFuture.supplyAsync(() -> db.selectUser(id))
            .whenComplete((r, e) -> flights.remove(k))
    );
}
```

**Python (asyncio):** similar pattern with a dict of `asyncio.Future`.

```python
_flights: dict[str, asyncio.Future] = {}

async def get_user(user_id: str) -> User:
    key = f"user:{user_id}"
    if key in _flights:
        return await _flights[key]
    fut = asyncio.get_event_loop().create_future()
    _flights[key] = fut
    try:
        result = await db.select_user(user_id)
        fut.set_result(result)
        return result
    except Exception as e:
        fut.set_exception(e)
        raise
    finally:
        _flights.pop(key, None)
```

**C++:** `std::shared_future` with a `std::unordered_map` protected by a mutex.
First caller creates the promise and launches the work; subsequent callers get
a shared future to the same result.

```cpp
std::mutex mu;
std::unordered_map<std::string, std::shared_future<User>> flights;

std::shared_future<User> getUser(const std::string& id) {
    std::string key = "user:" + id;
    std::lock_guard<std::mutex> lock(mu);
    auto it = flights.find(key);
    if (it != flights.end()) return it->second;

    auto fut = std::async(std::launch::async, [key, &id]() {
        auto result = db::selectUser(id);
        {
            std::lock_guard<std::mutex> lock(mu);
            flights.erase(key);
        }
        return result;
    }).share();
    flights[key] = fut;
    return fut;
}
```

**What it is and isn't:**

- Mitigates P3 (stampede) — collapses N concurrent DB reads into 1
- Does NOT fix P3 completely — the 1 remaining reader still hits DB, which is
  far slower than a cache hit. Reduces the multiplier, not the latency.
- Composable with any combination — not specific to CAS or the lock stack. Can
  coalesce DB reads, cache reads, or both.
- Application-level, in-process only — each instance coalesces independently.
  With K app instances, up to K DB reads still fire (1 per instance).

---

## 3. Mechanism Comparison Table

| Mechanism | Fixes | Introduces | Read cost | Write cost | Failure |
|---|---|---|---|---|---|
| M1: DB→delete | — | P1 (ms), P3 | 1 GET | 1 DEL | DEL idempotent |
| M2a: Lock manual | P1, P7 | P6 (starvation) | Lock check | SETNX + DEL lock | Held forever on crash |
| M2b: Lock TTL | P1, P7, P6 | P8 (mid-expire) | Lock check | SETNX EX + DEL lock | Expires safely |
| M3: Watchdog | P6, P8 | Complexity | None extra | Periodic extends | TTL expires safely |
| M4: Version-in-key | P2 | Need version for key | None if version known | None extra | Old keys linger |
| M5: Version-in-value Lua | P2 | Lua complexity | None (plain GET) | Lua on population | Atomic, safe |
| M6: CAS/lease | P1, P2 | P4, P5 | Compare EVERY read | INCR + SET | Orphaned version |
| M7: Delete-first | Safe failure order | — | None | None extra | Cache empty = correct |
| M8: Serve-if-exists | Unnecessary blocking | — | None extra | None | — |
| M9: Write-through in lock | P3 (stampede) | Lock ~0.2ms longer | None | 1 extra SET | SET fails → empty |
| M10: Invalidate (lazy) | P7 (DEL idempotent) | P1, P3 | None | 1 DEL | Safe |
| M11: Write-through (eager) | P3 | P7 (writer race) | None | 1 SET | Wrong order = stale |
| M12: Singleflight | P3 (mitigates) | Per-instance only | None | None | Shared error propagation |

---

## 4. Key Combinations

Three named combinations are referenced throughout this document. Each is a
specific set of mechanisms chosen to work together:

| Name | Mechanisms | What it is |
|---|---|---|
| **Simple** | M1 + M10 + M4 | DB→delete + invalidate-on-write + version-in-key. No lock, no Lua, no watchdog. Accepts a small ms stale window. |
| **CAS** | M6 | CAS/lease. Version counter + compare on every read. No lock. Zero stale by detecting mismatches on the read path. |
| **Full stack** (or "lock stack") | M2b + M3 + M5 + M7 + M8 + M9 + M10 | Lock with TTL + watchdog + version-in-value Lua + delete-first ordering + serve-if-exists + write-through inside lock + invalidate-on-write. Zero stale by preventing inconsistency on the write path. |

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

### CAS/lease: M6

```
Write: INCR user:<id>:version → DB UPDATE → SET user:<id> {version, data}
Read:  GET version → GET cache → match? return. mismatch? → DB.
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (mismatch detected → DB) |
| P2: Slow-reader overwrite | Fixed (version compare rejects stale writes) |
| P3: DB stampede | Worst (all mismatched readers hit DB) |
| P4: Cache functioning | Broken under write pressure |
| P5: Orphaned state | Until next successful write |
| Read path (no write) | 1 GET + version compare |
| Write path | INCR + DB + SET |
| Redis write dependency | **Yes** — INCR before DB |
| Complexity | Medium |
| Staleness guarantee | No stale reads; P5 = DB fallback on failure |

### Full lock stack: M2b + M3 + M5 + M7 + M8 + M9 + M10

```
Write: SETNX lock → DEL cache → DB UPDATE → Lua SET cache → DEL lock
Read:  GET cache → hit? return (even during lock — M8).
       miss? → Lua check lock → locked? wait → unlocked? cache hit or DB.
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (readers blocked or served valid cache) |
| P2: Slow-reader overwrite | Fixed (Lua version compare on population) |
| P3: DB stampede | Fixed (cache populated before unlock) |
| P7: Concurrent writers | Fixed (lock serializes) |
| P6: Starvation | Fixed (watchdog + TTL) |
| Read path (no write) | 1 GET (plain, no compare) |
| Read path (during write, cache exists) | 1 GET → cache hit (serve-if-exists) |
| Read path (during write, cache miss) | Lua check → block → cache hit after unlock |
| Write path | SETNX + DEL + DB + Lua SET + DEL lock |
| Redis write dependency | **Yes** — but degrades to simple approach if Redis down |
| Complexity | High |
| Staleness guarantee | **No stale reads** |

---

## 4b. Flow Diagrams — Write Path + Reader Behavior

Visual flows for each approach: what the writer does, what happens at each step
if it succeeds or fails, and how readers arriving during the write are handled.

### CAS/lease (M6) — Write + Reader Flow

```
WRITER                                          READER (during write)
──────                                          ──────────────────────
    │                                               │
    ▼                                               ▼
┌─────────────┐                              ┌──────────────┐
│ INCR version│                              │ GET version   │
│ → v(N+1)    │                              │ GET cache data│
└──────┬──────┘                              └──────┬───────┘
       │                                            │
       │ Redis has version=v(N+1)                   ▼
       │ DB still has v(N)                   ┌──────────────┐
       │ Cache still has v(N)                │ version match?│
       │                                     └──┬───────┬───┘
       ▼                                        │       │
┌──────────────┐                            YES │       │ NO (v(N+1) ≠ v(N))
│ DB UPDATE    │                                │       │
│ v(N) → v(N+1)│                                ▼       ▼
└──────┬───────┘                         ┌──────────┐ ┌────────────────┐
       │                                 │ Return   │ │ DB SELECT      │
       │ ← FAIL?                         │ cache hit│ │ (row lock wait │
       │    ▼                            └──────────┘ │  if not MVCC,  │
       │  version=v(N+1)                              │  or MVCC: get  │
       │  DB=v(N) (unchanged)                         │  v(N) = same   │
       │  cache=v(N)                                  │  as cache had) │
       │  → orphaned version (P5)                     └───────┬────────┘
       │  → ALL readers: mismatch → DB                        │
       │  → until next successful write                       ▼
       │                                              ┌────────────────┐
       ▼ SUCCESS                                      │ Return DB data │
┌──────────────┐                                      │ (correct, but  │
│ SET cache    │                                      │  wasted trip   │
│ {v(N+1),data}│                                      │  if MVCC)      │
└──────┬───────┘                                      └────────────────┘
       │
       │ ← FAIL?
       │    ▼
       │  version=v(N+1), DB=v(N+1), cache=v(N)
       │  → orphaned (same as DB fail from reader view)
       │  → ALL readers: mismatch → DB
       │
       ▼ SUCCESS
  version=v(N+1), DB=v(N+1), cache=v(N+1)
  → readers: match → cache hit ✓

DURING WRITE (INCR to SET): ~80-90% of time
  → version ≠ cache → ALL readers bounce to DB
  → cache is bypassed, not corrected
```

### Full Lock Stack (naive) — Write + Reader Flow

```
WRITER                                        READER (during write)
──────                                        ──────────────────────
    │                                             │
    ▼                                             ▼
┌──────────────┐                           ┌──────────────┐
│ SETNX lock   │                           │ GET cache    │
│ (n_r + p)    │                           └──────┬───────┘
└──────┬───────┘                                  │
       │                                          ▼
       │ ← FAIL (lock held)?               ┌───────────┐
       │    → wait/retry or                 │ cache HIT?│
       │      degrade to Simple             └──┬─────┬──┘
       │                                       │     │
       ▼ ACQUIRED                          YES │     │ NO (empty)
┌──────────────┐                               │     │
│ GET cache    │                               ▼     ▼
│ (save old    │                        ┌─────────┐ ┌──────────────┐
│  value)      │                        │ Serve   │ │ Check lock   │
│ (n_r + p)    │                        │ cache   │ │ (Lua script) │
└──────┬───────┘                        │ (M8) ✓  │ └──────┬───────┘
       │                                │ correct │        │
       ▼                                │ old data│        ▼
┌──────────────┐                        └─────────┘ ┌───────────┐
│ DEL cache    │                                    │ locked?   │
│ (n_r + p)    │                                    └──┬─────┬──┘
└──────┬───────┘                                       │     │
       │                                           YES │     │ NO
       │ cache now EMPTY                               │     │
       │ (readers after this: blocked)                 ▼     ▼
       │                                        ┌──────────┐ ┌──────────┐
       ▼                                        │ WAIT     │ │ DB SELECT│
┌──────────────┐                                │ (block   │ │ → SET    │
│ DB UPDATE    │                                │  until   │ │ cache    │
│ (n_d + u)    │                                │  unlock) │ │ (normal  │
└──────┬───────┘                                └────┬─────┘ │ miss)    │
       │                                             │       └──────────┘
       │ ← FAIL?                                     │
       │    ▼                                        │
       │  try restore: SET cache {old value}         │
       │  → unlock                                   │
       │  → readers: cache hit (restored) ✓          │
       │  or if restore fails:                       │
       │  → readers: miss → DB (correct) ✓           │
       │                                             │
       ▼ SUCCESS                                     │
┌──────────────┐                                     │
│ Lua SET cache│                                     │
│ {v(N+1),data}│                                     │
│ (n_r + l)    │                                     │
└──────┬───────┘                                     │
       │                                             │
       │ ← FAIL?                                     │
       │    → cache empty (DEL already ran)          │
       │    → unlock                                 │
       │    → readers: miss → DB (correct) ✓         │
       │                                             │
       ▼ SUCCESS                                     │
┌──────────────┐                                     │
│ DEL lock     │                                     │
│ (n_r + p)    │◄────────────────────────────────────┘
└──────┬───────┘          readers unblock here
       │                  → GET cache → HIT (M9) ✓
       ▼
  DONE. cache=v(N+1), DB=v(N+1), lock=free

DURING WRITE:
  cache exists (before DEL) → M8: serve immediately ✓
  cache empty + locked       → block, wait for unlock
  after unlock               → cache hit (M9 populated) ✓
  → cache WORKS throughout (serves or blocks-then-serves)
```

### Full Lock Stack (pipelined) — Write + Reader Flow

```
WRITER                                        READER (same as naive)
──────                                        ──────────────────────
    │                                         (reader flow identical
    ▼                                          to naive — M8/block/M9
┌───────────────────────┐                      behavior unchanged)
│ PRE-DB LUA SCRIPT     │
│ (single round trip)   │
│                       │
│  SETNX lock           │
│  → FAIL? degrade      │
│  GET cache (save old) │
│  DEL cache            │
│                       │
│ (n_r + l)             │
└───────────┬───────────┘
            │
            ▼
┌───────────────────────┐
│ DB UPDATE             │
│ (n_d + u)             │
└───────────┬───────────┘
            │
            │ ← FAIL? restore from saved value (same as naive)
            │
            ▼ SUCCESS
┌───────────────────────┐
│ POST-DB LUA SCRIPT    │
│ (single round trip)   │
│                       │
│  Version-compare SET  │
│  cache {v(N+1), data} │
│  DEL lock             │
│                       │
│ (n_r + l)             │
└───────────┬───────────┘
            │
            ▼
  DONE. 2 Redis RTs + 1 DB RT (vs 5+1 naive)

  Same correctness, same reader behavior.
  Difference: writer network cost only.
  
  Write cost:  2n_r + 2l + n_d + u  (pipelined)
           vs  5n_r + 4p + l + n_d + u  (naive)
  Savings:     3n_r + 4p - l ≈ 3n_r
```

### Side-by-side: what readers experience during a write

```
              CAS                          Lock Stack
              ───                          ──────────
Reader        GET version + GET cache      GET cache
arrives  ──►  version mismatch?            cache exists?
              │                            │
         YES (during write, ~80-90%)  YES (before DEL or after SET)
              │                            │
              ▼                            ▼
         DB SELECT                    SERVE CACHE (M8)
         (n_d + s, wasted             (n_r + p, correct)
          if MVCC)                         ✓ done
              │
              ▼                       NO (cache empty, locked)
         Return old data                   │
         (same as cache had)               ▼
              ✓ correct but             WAIT for unlock
                wasted trip             (avg 1/2 write time)
                                           │
                                           ▼
                                      GET CACHE (M9)
                                      (n_r + p, new data)
                                           ✓ done

         After write:                 After write:
         SET cache finally            cache already populated
         matches version              by M9 inside lock
         → next reader: hit           → next reader: hit ✓
         (until next INCR)
```

---

## 5. Combination Matrix — Problems Solved

| Combination | P1 | P2 | P3 | P4 | P5 | P6 | P7 | P8 | Staleness |
|---|---|---|---|---|---|---|---|---|---|
| M1+M10 (DB→delete) | ms window | Vulnerable | Small | Works | No | No | Safe | N/A | ms + long (P2) |
| M1+M10+M4 (+ version-in-key) | ms window | **Fixed** | Small | Works | No | No | Safe | N/A | **ms only** |
| M6 (CAS/lease) | **Fixed** | **Fixed** | **Worst** | **Broken** | Until next write | No | N/A | N/A | **None; P5 fallback** |
| M11 (write-through no lock) | **Fixed** | Vulnerable | **Fixed** | Works | No | No | **Vulnerable** | N/A | Long (P7) |
| M2b+M3+M7+M10 (lock+watchdog) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | P2 only |
| M2b+M3+M5+M7+M10 (+ version) | **Fixed** | **Fixed** | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** |
| Full stack (+M8+M9) | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** |
| | | | | | | | | | |
| *+M12 (singleflight) variants:* | | | | | | | | | |
| M1+M10+M4+M12 (Simple+SF) | ms window | **Fixed** | **Mitigated** | Works | No | No | Safe | N/A | **ms only** |
| M6+M12 (CAS+SF) | **Fixed** | **Fixed** | **Mitigated** | **Broken** | Until next write | No | N/A | N/A | **None; P5 fallback** |
| Full stack+M12 | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** |

---

## 6. Read/Write Path Costs

| Combination | Read (no write) | Read (write, cache exists) | Read (write, cache miss) | Write |
|---|---|---|---|---|
| Simple (M1+M10+M4) | 1 GET | 1 GET (ms stale possible) | 1 GET miss → DB ~1-5ms | DB + 1 DEL |
| CAS (M6) | 1 GET + compare | 1 GET + compare → DB | 1 GET + compare → DB | INCR + DB + SET |
| Full stack | 1 GET | 1 GET → **cache hit** | Lua → block → **cache hit** | SETNX + DEL + DB + Lua SET + DEL lock |
| | | | | |
| *+M12 (singleflight) variants:* | | | | |
| Simple+M12 | 1 GET | 1 GET (ms stale possible) | 1 GET miss → **1 shared DB** per instance ~1-5ms | DB + 1 DEL |
| CAS+M12 | 1 GET + compare | 1 GET + compare → **1 shared DB** per instance | 1 GET + compare → **1 shared DB** per instance | INCR + DB + SET |
| Full stack+M12 | 1 GET | 1 GET → **cache hit** | Lua → block → **cache hit** (M12 as safety net on fallback) | SETNX + DEL + DB + Lua SET + DEL lock |

Key difference: during writes, the full stack serves cache hits where the
others serve DB round trips. After writes, the full stack's cache is already
populated where the others require the first reader to repopulate.

---

## 7. Best Fit by Workload

| Workload | Best | Runner-up | Worst |
|---|---|---|---|
| High read, low write | **Simple** — cache works, ms stale barely matters, no Redis write dep | CAS — zero stale, low overhead when writes rare | Write-through no lock — P7 risk |
| Low read, high write | **Simple** — cache rarely useful, minimal ops | Lock stack — serializes cleanly | CAS — INCR+SET overhead, cache never serves hits |
| High read+write, ms stale OK | **Simple** — ms stale self-healing, lock overhead not justified | Full stack — correct but heavy | CAS — cache broken under writes, DB hammered |
| High read+write, zero stale | **Full stack** — zero stale + cache works + no stampede | CAS — zero stale, no lock complexity, but cache degrades | Write-through no lock — P7 = long stale |
| Zero stale, no lock complexity | **CAS** — only zero-stale without locks | Simple — ms stale, not zero | Full stack — has locks (unwanted) |
| Redis outage tolerance | **Simple** — writes don't depend on Redis | Full stack with fallback | CAS — INCR before DB = writes blocked |

---

## 8. Why the Lock's Overhead Is Minor

### Readers wait the same time either way

```
Without lock: reader waits for DB row lock → DB SELECT → return
              Total: ~2-10ms (wait + DB read)

With lock:    reader waits for Redis lock → GET cache → return
              Total: ~1.5-5.5ms (wait + cache read)
```

The wait is bounded by the DB write time in both cases. The lock moves the
wait from the expensive place (DB queue, ~1-5ms per read) to the cheap place
(Redis, ~0.1-0.5ms per read). Same wait, better outcome.

### Writer serialization is not "double"

Writers already serialize on the DB row lock. The Redis lock adds ~0.5-1ms
(~10-20% overhead on a ~5ms DB write). Per-user lock scope
(`user:<id>:lock`) means different users don't block each other.

### Redis dependency is real but manageable

```
try:    acquire lock → DEL → DB → SET → release
catch:  Redis down → fallback to DB UPDATE only (degrades to Simple)
```

During Redis outage: writes succeed, reads hit DB. When Redis returns: brief
stale window (same as Simple). Zero-stale guarantee lost temporarily, not
permanently.

---

## 9. Race Condition Scenarios

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
Every reader: GET version → GET cache → mismatch → DB.
Cache serves zero hits. All traffic goes to DB.
✗ Cache is fiction. DB load = no-cache load.
```

### CAS (M6): back-of-envelope — why DB fallback dominates during writes

The CAS write path is: `INCR version` → `DB UPDATE` → `SET cache {version, data}`.

Between the INCR and the SET, the version counter is ahead of the cache data.
Any reader during this window sees a mismatch and falls through to DB.

The DB UPDATE is the bottleneck — it dwarfs Redis INCR and SET latencies. A
rough breakdown of a typical write (~5ms total):

```
INCR version:    ~0.1-0.5ms   (Redis round trip)
DB UPDATE:       ~3-5ms        (query + commit + fsync)
SET cache:       ~0.1-0.5ms   (Redis round trip)

DB UPDATE is ~80-90% of total write time.
```

During that 80-90%, the version counter and cache data disagree. Every reader
for that key during this window sees a mismatch and falls through to DB.

But that DB fallback is also not a clean SELECT — the writer holds a row lock
on the same row. The reader's SELECT must wait for the writer's transaction to
commit before it can read. So the reader pays:

```
CAS reader during write:
  GET version + GET cache     ~0.2-1ms  (two Redis round trips)
  → mismatch detected
  → DB SELECT                 blocked behind writer's row lock (~remaining write time)
  → row lock released         → SELECT completes (~0.5-1ms for the read itself)
  Total: ~3-6ms (dominated by row lock wait)

Compare — full lock stack reader during write:
  GET cache → HIT (serve-if-exists, M8) → return
  Total: ~0.1-0.5ms
  OR
  GET cache → MISS → Redis lock wait → unlock → cache HIT (M9) → return
  Total: ~write time + 0.1-0.5ms (same wait, but ends with cache hit not DB read)
```

The key insight: CAS achieves zero staleness by **abandoning the cache whenever
correctness is in question**. The cache is a fair-weather optimization — it works
when no writes are happening, and turns off when they are. During any single
write, ~80-90% of concurrent readers for that key fall through to DB and wait
behind the writer's row lock. A DB row lock is being held either way (CAS or
lock stack), but CAS ends with a DB read while the lock stack ends with a cache
hit.

For low-write-rate keys (e.g. user profile updated a few times a day), this
window is a few milliseconds a few times a day — the cache works 99.99% of the
time. For high-write-rate keys, CAS degrades to effectively no cache.

**M12 (singleflight) mitigates but does not eliminate this.** With singleflight,
the N concurrent readers coalesce into 1 DB read instead of N. The DB load
drops from N reads to 1, but that 1 reader still pays the full row-lock-wait +
DB-read latency. The N-1 waiters share the result but still wait the same
duration. Singleflight reduces DB connection pressure, not reader latency.

### Write-through no lock (M11): concurrent writer race — VULNERABLE

```
1. Writer A: DB UPDATE (data=A) → SET cache = A (delayed by network)
2. Writer B: DB UPDATE (data=B) → SET cache = B
3. Writer A's SET arrives late → SET cache = A (overwrites B)
   DB has B. Cache has A. STALE.
✗ Until TTL or next write.
```

### Full stack: all scenarios handled

```
Concurrent writers: serialized by lock. ✓
Slow reader: Lua rejects stale write. ✓
Writer crash: watchdog stops → TTL expires → cache empty → DB. ✓
Network error (SET fails): DEL already ran → cache empty → DB. ✓
During write (cache exists): serve-if-exists → correct (DB unchanged). ✓
During write (cache miss): blocked → unlock → cache hit (M9). ✓
Redis down: fallback to Simple. ✓
```

### Full lock stack: failure at every step of the write path

The write path has 5 steps. At each step, the writer can fail (crash, Redis
down, DB down, network error). Below is what happens at each failure point,
how the system recovers, and what readers experience.

**Reference: the normal (happy) write path and its latency:**

```
Step 1: SETNX lock EX <TTL> NX       ~0.1-0.5ms
Step 2: GET cache (save old value)   ~0.1-0.5ms
        DEL cache                    ~0.1-0.5ms
Step 3: DB UPDATE                    ~3-5ms
Step 4: Lua SET cache {new data}     ~0.1-0.5ms
Step 5: DEL lock                     ~0.1-0.5ms
                                     ─────────
Normal total:                        ~3.5-7ms
Normal reader (no write):            ~0.1-0.5ms (1 GET, cache hit)
```

**Note on step 2 — saving old value before DEL:**

Standard Redis DEL returns the count of deleted keys (0 or 1), NOT the deleted
value. To enable cache restoration on DB failure, the writer should GET the
value before DEL (two commands, but safe inside the lock — no race). Or use
GETDEL (Redis 6.2+) which atomically returns and deletes in one command.

```
Option A (any Redis):  val = GET cache → DEL cache    (2 round trips, ~0.2-1ms)
Option B (Redis 6.2+): val = GETDEL cache              (1 round trip, ~0.1-0.5ms)
```

This captured value is the fallback: if DB UPDATE fails, the writer can restore
the cache to its pre-write state rather than leaving it empty.

---

**Failure 1: SETNX lock fails**

```
Cause: Redis down, or lock already held by another writer.

State after failure:
  Lock:  not acquired (or held by someone else)
  Cache: unchanged (correct)
  DB:    unchanged (correct)

Writer recovery:
  Redis down → degrade to Simple: DB UPDATE → DEL cache (if Redis comes back)
  Lock held  → wait/retry, or fail the write. Other writer finishes normally.

Reader impact: none — cache and DB are consistent, normal reads continue.

Writer latency:  ~0ms (immediate failure) or retry overhead
Reader latency:  ~0.1-0.5ms (normal cache hit, unaffected)
Staleness:       None
```

**Failure 2: DEL cache fails (after lock acquired)**

```
Cause: Redis connection lost between SETNX and DEL.

State after failure:
  Lock:  acquired (but Redis may be down, so lock visibility unclear)
  Cache: still has old data (DEL never executed)
  DB:    unchanged

Writer recovery:
  Redis is down → cannot DEL cache, cannot DEL lock.
  Degrade: proceed with DB UPDATE only (Simple mode).
  Watchdog cannot extend lock → TTL expires → lock auto-releases.

Reader impact during recovery:
  If cache still readable:  serve old data (correct — DB unchanged)
  If Redis fully down:      readers fall through to DB (correct)

Writer latency:  ~0.1-0.5ms (SETNX) + failure detection
Reader latency:  ~0.1-0.5ms (cache hit, old = correct) or ~1-5ms (DB if Redis down)
Staleness:       None during failure. Brief window possible after Redis returns
                 (same as Simple — self-healing).
```

**Failure 3: DB UPDATE fails (after DEL cache succeeded)**

```
Cause: DB down, constraint violation, deadlock, timeout.

State after failure:
  Lock:  held (writer still alive, watchdog extending)
  Cache: empty (was deleted in step 2)
  DB:    unchanged (old data, update failed)

Writer recovery:
  Saved old value from step 2 → SET cache {old data, old version} → DEL lock
  If restore SET also fails → DEL lock, cache stays empty.

  With saved value:     lock → DEL → DB fail → SET old → unlock
  Without saved value:  lock → DEL → DB fail → unlock (cache empty)

Reader impact during recovery (lock still held):
  Cache empty + lock held → blocked (M8 has nothing to serve)
  After unlock with restored cache → cache hit (old correct data)
  After unlock without restored cache → cache miss → DB read (old correct data)

  Note: the DB read after unlock is a clean SELECT — the writer's DB transaction
  already rolled back, so no row lock wait. Just 1 DB access, not 2.

Writer latency:  ~0.5-1.5ms (SETNX + GET + DEL) + DB fail time + ~0.1-0.5ms restore
Reader latency:
  Block time: ~(writer's DB fail time, ~1-5ms for timeout, or <1ms for constraint)
  Best:   block → cache hit ~0.1-0.5ms (restored cache)
          Total: ~block + 0.1-0.5ms ≈ 1 DB access + cache read
  Worst:  block → DB SELECT ~1-5ms (fallback restore also failed, cache empty)
          Total: ~block + 1-5ms ≈ 2 DB access durations (~2-10ms)
  +M12:   worst case → 1 shared DB read per instance instead of N

Staleness:       None. DB unchanged, cache either restored to correct old data
                 or empty (readers get correct data from DB).
```

**Failure 4: Lua SET cache fails (after DB UPDATE succeeded)**

```
Cause: Redis down after DB commit, Lua error, network timeout.

State after failure:
  Lock:  held (or expired if Redis down)
  Cache: empty (deleted in step 2, new SET failed)
  DB:    has new data (committed)

Writer recovery:
  Retry SET once. If retry fails → DEL lock (or let TTL expire), accept empty cache.
  DO NOT restore old value — DB has new data, restoring old cache = stale.

Reader impact during recovery (lock still held):
  Cache empty + lock held → blocked
  After unlock → cache miss → DB read (correct new data)

  Note: DB UPDATE already committed → row lock released. Reader's SELECT is
  a clean read (~1-5ms), no row lock wait. The block time on the Redis lock
  IS the wait for the writer's DB — readers don't also wait on DB row lock.

Reader impact after lock expires (Redis down):
  Cache empty → DB read (correct new data)

Writer latency:  ~3.5-6.5ms (SETNX + GET + DEL + DB) + retry ~0.1-0.5ms + DEL lock
Reader latency:
  Block time: ~(remaining lock time after DB committed)
  After unlock: DB SELECT ~1-5ms (cache empty)
  Total: ~block + 1-5ms ≈ up to 2 DB access durations (~2-10ms)
  +M12: 1 shared DB read per instance instead of N

Staleness:       None. Cache empty, DB has correct new data. Readers always
                 get correct data from DB. First successful reader repopulates cache.
```

**Failure 5: DEL lock fails (after everything else succeeded)**

```
Cause: Redis blip, network error on the final command.

State after failure:
  Lock:  still held (DEL failed)
  Cache: has new correct data (Lua SET succeeded)
  DB:    has new correct data (committed)

Writer recovery:
  Retry DEL lock once. If retry fails → watchdog stops → TTL expires → auto-release.

Reader impact while lock held:
  GET cache → HIT (new correct data) → serve immediately (M8)
  Readers are unaffected — cache has the right data.

Writer latency:  ~3.5-7ms (full path) + retry ~0.1-0.5ms
Reader latency:  ~0.1-0.5ms (cache hit, correct data, unblocked by M8)
Staleness:       None. Everything is consistent. Lock is a stale artifact,
                 expires via TTL. No reader impact.
```

**Failure 6: Writer crash (at any point after lock acquired)**

```
Cause: process killed, OOM, panic.

State after failure:
  Lock:  held, no writer to release it
  Cache: depends on where crash happened (old data / empty / new data)
  DB:    depends on where crash happened (old / new)

Writer recovery:
  None — writer is dead. Watchdog goroutine is also dead (same process).
  Lock TTL expires (configurable, e.g. 500ms–2s). System self-heals.

Timeline:
  Crash during step 2 (DEL):   cache may be old or empty, DB unchanged → correct either way
  Crash during step 3 (DB):    cache empty, DB rolled back (uncommitted) → correct
  Crash during step 4 (SET):   cache empty, DB committed new data → correct (readers hit DB)
  Crash after step 4 (before unlock): cache has new data, DB has new data → correct

Reader impact while lock held (up to TTL expiry):
  Cache exists → M8 serves it (correct, whatever version is there)
  Cache empty → blocked until TTL expires → DB read (correct)

Reader latency:
  Best:   ~0.1-0.5ms (cache hit via M8 — cache exists, correct data)
  Worst:  blocked up to TTL → cache miss → DB SELECT ~1-5ms
          Total: ~TTL + 1-5ms (TTL dominates)
  +M12:   after TTL expiry, 1 shared DB read per instance

Staleness:       None. At every crash point, cache is either correct or empty.
                 Empty → DB is source of truth → correct.
```

**Summary: full lock stack failure paths**

Reader latency has three cases:
- **Best\***: reader arrives after the writer unlocks (rare — DB access is
  ~80-90% of lock hold time, so most readers arrive during the block)
- **Average**: reader arrives mid-block (most likely)
- **Worst**: reader arrives as the block starts (full wait)

```
                                                                                       Cache
Scenario          Cache     DB       Stale?   Best *         Average        Worst       works?
─────────────────────────────────────────────────────────────────────────────────────────────────
Normal (no error)
  no write        correct   same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes
  write, cache hit correct  same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes (M8)
  write, cache miss empty   same     No       ~0.1-0.5ms     ~0.1-0.5ms    block+0.1ms Yes (M9)

Failure paths
1. SETNX fail     correct   same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes
2. DEL cache fail correct   same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~1-5ms †    Yes/DB †
3. DB UPDATE fail rest/empty same    No       ~0.1-0.5ms *   block+0.1ms   block+1-5ms No ‡
4. Lua SET fail   empty     new      No       ~1-5ms *       block+1-5ms   block+1-5ms No ‡
5. DEL lock fail  correct   new      No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes (M8)
6. Writer crash   varies    varies   No       ~0.1-0.5ms *   TTL/2+1-5ms   TTL+1-5ms   Varies

*  Rare: reader arrives after writer unlocks. DB access is ~80-90% of lock hold
   time, so most readers hit the block → average case is the common one.
†  Redis down — readers fall through to DB.
‡  "block" is the Redis lock wait, but its duration varies by scenario:

   Normal (cache miss): block ≈ full write time (~1-5ms DB + ~0.1-0.5ms Lua SET).
     Reader waits for the entire write to finish including cache write-through (M9).
     After unlock: cache hit (~0.1ms). This is the longest "happy" block.

   3. DB UPDATE fail: block ≈ DB fail time + restore attempt.
     Fast fail (constraint error): block <1ms. Timeout: block ~1-5ms.
     After unlock: cache hit if restored (~0.1ms), or DB SELECT if not (~1-5ms).

   4. Lua SET fail: block ≈ DB UPDATE time (~1-5ms) + SET fail time.
     DB already committed by this point. After unlock: DB SELECT (~1-5ms).

   6. Writer crash: block ≈ up to TTL (configurable). Watchdog is dead, lock expires.
     After TTL: cache hit if populated before crash, or DB SELECT if not.

   Worst total (any failure) = block + DB SELECT ≈ 2 DB accesses (~2-10ms).
   Exception: crash = TTL + DB SELECT (TTL dominates).
   TTL is configurable — tune based on observed write latency and acceptable
   crash-recovery delay (e.g. 500ms–2s for fast writes, longer for slow ones).
All worst cases with DB reads mitigated by M12: N DB reads → 1 per instance.
```

The invariant: **at no failure point does a reader ever see stale data.** The
cache is either correct (serve it), or empty (fall through to DB, which is
always the source of truth). The delete-first ordering (M7) guarantees this —
cache is destroyed before DB changes, so any intermediate state has either
old-correct-cache or no-cache, never new-DB-with-old-cache.

---

### CAS: failure at every step of the write path

**Reference: the normal (happy) write path:**

```
Step 1: INCR version               ~0.1-0.5ms
Step 2: DB UPDATE                   ~3-5ms
Step 3: SET cache {version, data}   ~0.1-0.5ms
                                    ─────────
Normal total:                       ~3.5-6ms
Normal reader (no write):           ~0.2-1ms (GET version + GET cache + compare)
```

---

**Failure 1: INCR version fails**

```
Cause: Redis down.

State after failure:
  Version counter: unchanged
  Cache:           unchanged (old data, old version — consistent)
  DB:              unchanged

Writer recovery:
  Cannot proceed — INCR is required before DB (version must be incremented
  to signal readers). Write is blocked.
  Unlike the lock stack, CAS has a hard Redis dependency on the write path.

Reader impact: none — version and cache match, normal cache hits.

Writer latency:  N/A (write blocked, returns error)
Reader latency:  ~0.2-1ms (normal, unaffected)
Staleness:       None — nothing changed.
Note:            This is the Redis write dependency cost. Lock stack degrades
                 to Simple; CAS blocks entirely.
```

**Failure 2: DB UPDATE fails (after INCR succeeded)**

```
Cause: DB down, constraint violation, deadlock.

State after failure:
  Version counter: incremented to v(N+1) — orphaned (P5)
  Cache:           has old data with version v(N)
  DB:              unchanged (old data)

Writer recovery:
  None immediate. The orphaned INCR cannot be rolled back (no "DECR to undo"
  — another writer may have INCRed again concurrently).

Reader impact — two phases:

  DURING the DB UPDATE attempt (writer still in-progress, holding row lock):
    GET version → v(N+1). GET cache → v(N). Mismatch → DB SELECT.
    But writer holds row lock → reader's SELECT blocked behind it.
    Reader pays: version check (~0.2-1ms) + row lock wait (~1-5ms) + SELECT (~1-5ms)
    ≈ version check + 2 DB access durations (~2.2-11ms)

  AFTER the DB UPDATE fails (writer done, row lock released, orphaned version):
    Same mismatch → DB SELECT, but no row lock → clean SELECT.
    Reader pays: version check (~0.2-1ms) + SELECT (~1-5ms) = ~1.2-6ms
    This persists until next successful write resyncs the version.

  Cache is useless in both phases — version permanently behind counter.

Recovery timeline:
  Next successful write: INCR → v(N+2), DB UPDATE, SET cache {v(N+2), data}.
  Cache works again. The gap at v(N+1) is harmless.

Reader latency:
  During write attempt: ~0.2-1ms + row lock wait + SELECT ≈ 2 DB accesses (~2.2-11ms)
  After failure (sustained): ~0.2-1ms + SELECT ≈ 1 DB access (~1.2-6ms)
  +M12: DB reads coalesced to 1 per instance, but per-reader latency unchanged

Staleness:       None — readers always get correct DB data on mismatch.
Duration:        Cache useless until next successful write (could be seconds,
                 minutes, or hours depending on write frequency).
```

**Failure 3: SET cache fails (after DB UPDATE succeeded)**

```
Cause: Redis down after DB commit, network error.

State after failure:
  Version counter: v(N+1) (incremented)
  Cache:           old data with version v(N) (SET never happened)
  DB:              new data (committed)

This is identical to Failure 2 from the reader's perspective — version mismatch
forces DB fallback. But with one difference: the DB UPDATE already committed,
so the row lock is released. Readers hit DB without row lock wait.

Writer recovery:
  Retry SET once. If retry fails, accept orphaned state (same as Failure 2).

Reader impact:
  Version mismatch → DB fallback → correct new data from DB.
  No row lock wait — DB transaction already committed.

Reader latency:
  ~0.2-1ms (version check) + DB SELECT ~1-5ms = ~1.2-6ms (1 DB access, no lock wait)
  +M12: 1 shared DB read per instance

Staleness:       None — mismatch always triggers DB read.
Duration:        Until retry succeeds, or next successful write resyncs.
```

**Failure 4: Writer crash (at any point)**

```
Crash during step 1 (INCR):
  Nothing changed or INCR completed but writer died before DB.
  If INCR completed → orphaned version (same as Failure 2).
  If INCR didn't complete → nothing changed.

Crash during step 2 (DB UPDATE):
  INCR done, DB transaction rolled back (uncommitted), row lock released.
  Orphaned version → same as Failure 2 (after-failure phase: clean SELECT).

Crash after step 2 (DB committed, before SET):
  INCR done, DB committed, row lock released, cache stale.
  Same as Failure 3 (no row lock wait).

In all cases: version mismatch → readers fall through to DB → correct data.
Row lock is always released (crash → DB rolls back uncommitted transactions).

Reader latency:
  During crash (if writer's DB UPDATE still in-flight): row lock wait + SELECT
    ≈ 2 DB accesses (~2.2-11ms)
  After crash (transaction resolved): clean SELECT ≈ 1 DB access (~1.2-6ms)
  +M12: 1 shared DB read per instance
```

**Summary: CAS failure paths**

Reader latency has three cases:
- **Best\***: reader arrives after the writer's DB transaction resolves (rare —
  DB access is ~80-90% of write time, so most readers arrive during the row lock)
- **Average**: reader arrives mid-write (most likely — blocked behind row lock)
- **Worst**: reader arrives as the DB UPDATE starts (full row lock wait)

CAS has an additional dimension the lock stack doesn't: **sustained impact**.
After a failure with an orphaned version, EVERY reader pays DB latency until
the next successful write — not just readers caught during the failure.

Write path flows: INCR version → DB UPDATE → SET cache data.
Columns follow this order so each failure shows where propagation stopped.

```
                                                                                                     Cache
Scenario          Version  DB (data)  Cache data  Stale?  Best *         Average         Worst       works?
──────────────────────────────────────────────────────────────────────────────────────────────────────────────
Normal (no error)
  no write        v(N)     v(N)       v(N)        No      ~0.2-1ms       ~0.2-1ms       ~0.2-1ms    Yes
  write ongoing   v(N+1)   updating   v(N)        No      ~1.2-6ms *     rlk+1-5ms      rlk+1-5ms   No ‡

Failure paths (propagation stopped at ↓)
1. INCR fail      v(N)     v(N)       v(N)        No      ~0.2-1ms       ~0.2-1ms       ~0.2-1ms    Yes
                  ↑ failed here — nothing changed
2. DB UPDATE fail v(N+1) † v(N)       v(N)        No      ~1.2-6ms *     rlk+1-5ms      rlk+1-5ms   No ‡‡
                           ↑ failed here — version ahead, DB and cache still at v(N)
3. SET cache fail v(N+1) † v(N+1)     v(N)        No      ~1.2-6ms       ~1.2-6ms       ~1.2-6ms    No ‡‡
                                      ↑ failed here — version and DB at v(N+1), cache stuck at v(N)
4. Writer crash   varies   varies     varies      No      ~0.2-1ms *     rlk+1-5ms      rlk+1-5ms   No ‡‡
                  ↑ depends on where crash happened — see failure 1/2/3 above

† Orphaned — version counter is ahead of cache data (and possibly DB).
  Version mismatch forces every reader to DB regardless of cache contents.

*  Rare: reader arrives after DB transaction resolves (committed or rolled back).
   Row lock released → clean SELECT (~1-5ms), no lock wait.
   DB access is ~80-90% of write time, so most readers hit the row lock.
‡  "rlk" = row lock wait. Unlike the lock stack's Redis lock, this is a DB-level
   block — the reader's SELECT is queued behind the writer's UPDATE on the DB server.

   Normal (write ongoing): rlk ≈ writer's remaining DB UPDATE time (~1-5ms).
     Reader's SELECT blocked on DB row lock → lock released → SELECT completes.
     Total: version check (~0.2-1ms) + rlk (~1-5ms) + SELECT (~1-5ms) ≈ 2 DB accesses.
     No cache fallback — version mismatch always forces DB.

   2. DB UPDATE fail: rlk ≈ DB fail time.
     Fast fail (constraint error): rlk <1ms. Timeout: rlk ~1-5ms.
     After row lock released: clean SELECT (~1-5ms).

   4. Writer crash (during DB): rlk ≈ until DB rolls back transaction.
     DB detects dead connection → rollback → row lock released.
     Faster than lock stack's TTL wait (DB timeout vs configurable TTL).

   Worst total = version check + rlk + SELECT ≈ 2 DB accesses (~2.2-11ms).

‡‡ Sustained: after failure, orphaned version means ALL readers pay DB latency
   (~1.2-6ms per read, clean SELECT, no row lock) until next successful write.
   Duration depends on write frequency — seconds for hot keys, hours for cold.
   Lock stack does NOT have this: cache recovers immediately after failure.

+M12: DB reads coalesced to 1 per instance, but per-reader latency unchanged.
```

**Key difference from the lock stack:**

The lock stack's failure modes mostly leave readers with a cache hit (~0.1-0.5ms)
or a brief block followed by a cache hit. Worst case is block (~1-5ms) + DB
SELECT (~1-5ms) ≈ 2 DB accesses — but only for the readers caught during that
one failure. After recovery, cache is restored or repopulated by the first reader.

CAS's failure modes (2, 3, 4) all leave the cache **permanently useless** until
the next successful write. Every reader pays DB latency (~1.2-6ms) for the
entire duration — and during the write attempt itself, readers behind the row
lock pay ~2 DB accesses (~2.2-11ms). For a key that writes infrequently, an
orphaned INCR could mean minutes or hours of cache-less reads.

```
Lock stack failure recovery:
  During: readers blocked ~1-5ms (Redis lock, not DB row lock)
  After:  cache hit (restored/M9) or 1 clean DB SELECT
  Worst:  block + SELECT ≈ 2 DB accesses, one-time
  Then:   cache works again

CAS failure recovery:
  During: readers pay ≈ 2 DB accesses (row lock wait + SELECT)
  After:  readers pay ≈ 1 DB access (clean SELECT, sustained)
  Worst:  every reader pays DB until next successful write
  Duration: write_interval (seconds for hot keys, hours for cold keys)
```

Both guarantee zero staleness on failure. The differences are:
1. **During failure:** lock stack readers wait on Redis (~fast), CAS readers wait
   on DB row lock (~slow). Both ≈ 2 DB access worst case, but lock stack best
   case is cache hit, CAS always hits DB.
2. **After failure:** lock stack cache recovers immediately (restored or
   repopulated). CAS cache stays broken until next successful write.

### Heavy writes: CAS state divergence vs lock stack serialization

Under sustained writes to the same key, CAS's three components (version counter,
DB, cache data) can each be at a different version simultaneously. The lock stack
serializes writes, so only one is ever in-flight.

**CAS under heavy writes (3 writers in quick succession):**

```
Write path: INCR version → DB UPDATE → SET cache data
Columns: Version | DB (data) | Cache data

Time   Event                        Version   DB        Cache data   Readers see
─────────────────────────────────────────────────────────────────────────────────
t0     Steady state                  v5        v5        v5           cache hit ✓
t1     Writer A: INCR                v6        v5        v5           mismatch → DB
t2     Writer B: INCR                v7        v5        v5           mismatch → DB
t3     Writer A: DB UPDATE (v5→v6)   v7        v6        v5           mismatch → DB
t4     Writer C: INCR                v8        v6        v5           mismatch → DB
t5     Writer A: SET cache           v8        v6        v6           mismatch → DB *
t6     Writer B: DB UPDATE (v6→v7)   v8        v7        v6           mismatch → DB
t7     Writer B: SET cache           v8        v7        v7           mismatch → DB *
t8     Writer C: DB UPDATE (v7→v8)   v8        v8        v7           mismatch → DB
t9     Writer C: SET cache           v8        v8        v8           cache hit ✓

* Writer A's SET wrote v6 to cache, but version counter is already at v8.
  The cache was "correct" for a fraction of a ms before Writer B's INCR
  made it stale again. Cache serves zero useful hits from t1 to t9.

All three components at different versions (t4-t5):
  Version = v8, DB = v6, Cache = v5
  Three-way divergence. Every reader falls through to DB.
```

Total time with useless cache: t1 to t9 ≈ 3 full write cycles.
During this entire window, every reader for this key pays DB latency.
With N writers overlapping: cache is useless for N write cycles.

**Lock stack under heavy writes (same 3 writers):**

```
Write path: SETNX lock → DEL cache → DB UPDATE → SET cache → DEL lock
Columns: Lock | DB (data) | Cache data

Time   Event                        Lock       DB        Cache data   Readers see
─────────────────────────────────────────────────────────────────────────────────
t0     Steady state                  free       v5        v5           cache hit ✓
t1     Writer A: SETNX lock         A holds    v5        v5           cache hit (M8) ✓
t2     Writer B: SETNX → blocked    A holds    v5        v5           cache hit (M8) ✓
t3     Writer C: SETNX → blocked    A holds    v5        v5           cache hit (M8) ✓
t4     Writer A: DEL cache          A holds    v5        empty        blocked (lock)
t5     Writer A: DB UPDATE (v5→v6)  A holds    v6        empty        blocked (lock)
t6     Writer A: SET cache          A holds    v6        v6           cache hit (M8) ✓
t7     Writer A: DEL lock           free       v6        v6           cache hit ✓
t8     Writer B: SETNX lock         B holds    v6        v6           cache hit (M8) ✓
       ... Writer B runs same cycle: DEL → DB (v6→v7) → SET → unlock
t12    Writer B: DEL lock           free       v7        v7           cache hit ✓
t13    Writer C: SETNX lock         C holds    v7        v7           cache hit (M8) ✓
       ... Writer C runs same cycle
t17    Writer C: DEL lock           free       v8        v8           cache hit ✓

At every point: DB and cache are either both at same version, or cache is
empty and readers are blocked (briefly). Never three-way divergence.
```

Lock serializes writers. Each writer completes its full cycle before the next
starts. Cache serves hits for most of the timeline — readers only block during
the brief DEL→SET window within each write (~1-5ms per write).

**The difference under heavy writes:**

```
                     CAS                          Lock stack
─────────────────────────────────────────────────────────────
Component sync       3-way divergence possible    2 states max (correct or empty)
Cache during writes  useless (entire write chain) serves hits (M8) except brief gap
Reader path          DB every time                cache hit most of the time
DB load              N readers × M writes         ~0 (readers get cache)
Total write time     overlapped (concurrent)      serialized (sequential)
Write throughput     higher (concurrent)           lower (serial per key)
```

The lock stack trades write throughput for cache availability. Under heavy
writes to the same key, CAS processes writes faster (concurrent) but the cache
is fiction — the lock stack is slower (serial) but readers never notice because
the cache actually works.

---

## 10. The Tradeoff Landscape

```
                    Zero staleness
                         ▲
                         │
         M6 CAS ────────┤──────── Full lock stack
         (simple,        │         (complex,
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

| | Simple | CAS | Full stack |
|---|---|---|---|
| Staleness | ms, self-healing | None | None |
| Cache under writes | Invalidated, first reader repopulates | Broken (constant mismatch) | Works (write-through inside lock) |
| DB load during writes | Medium (readers fall through) | Highest (all readers hit DB) | Lowest (readers get cache) |
| Read-path overhead | None | Compare every read | None |
| Redis write dependency | No | Yes | Yes (degrades gracefully) |
| Complexity | Low | Medium | High |
| Best for | Most systems | Zero stale without locks | Zero stale + cache works under pressure |

The final question is always: **is eliminating a millisecond self-healing stale
window worth the complexity?**
- For most systems, M1+M10+M4 is the answer.
- For zero-stale without complexity, CAS.
- For zero-stale with cache that actually works under pressure, the full lock stack — and its overhead is minor compared to the DB write that dominates the path regardless.


---

## 11. Why Locks Work Here (When They Don't in Low-Level Systems)

In C++ and low-level systems programming, locks are avoided on hot paths.
Mutexes operate in the microsecond range (~1-10μs), atomics in nanoseconds
(~1-100ns) — a 100-1000× difference. On a hot loop doing millions of ops/sec,
that gap is the difference between viable and bottleneck. This is why lock-free
data structures, CAS instructions, and atomics dominate performance-critical
C++ code.

**This calculus is completely different in the DB+cache domain.**

The bottleneck here is not the lock — it's the DB access (~1-5ms). Everything
else is noise by comparison:

```
Operation                  Latency         Relative to DB
──────────────────────────────────────────────────────────
C++ mutex lock/unlock      ~1-10μs         0.001-0.01×
C++ atomic CAS             ~1-100ns        0.0001-0.001×
Redis SETNX (lock)         ~0.1-0.5ms      0.02-0.5×
Redis GET/SET/DEL          ~0.1-0.5ms      0.02-0.5×
DB SELECT (simple PK)      ~1-5ms          1× (baseline)
DB UPDATE (commit+fsync)   ~1-5ms          1×
```

The Redis lock adds ~0.1-0.5ms to a path that already takes ~1-5ms for the DB
operation. That's ~5-20% overhead, not the 100-1000× penalty that makes locks
prohibitive in C++.

**The system already has locks at every layer:**

```
Layer          Lock mechanism             Granularity
─────────────────────────────────────────────────────
DB (MySQL)     InnoDB row locks           per-row
Redis          single-threaded event loop forced ordering on all ops
Application    Redis SETNX (lock stack)   per-key
```

The DB already serializes concurrent writes to the same row via row locks.
Redis already forces ordering via its single-threaded execution model — every
command is atomic, and Lua scripts execute without interleaving. The application-
level Redis lock (SETNX) is not introducing a new concept — it's extending the
DB's existing per-row serialization outward to cover the cache update as well.

**What the Redis lock actually does:**

It extends the DB's row lock scope from "just the DB write" to "DB write + cache
update." Without it, there's a gap between the DB commit and the cache update
where the two systems disagree. The Redis lock closes that gap by preventing
readers from seeing the intermediate state.

```
Without Redis lock:
  DB row lock:     [====DB UPDATE====]
  Cache coherent:  [===]              [====]     ← gap between DB commit and cache update
  Readers:              ↑ stale window here

With Redis lock:
  Redis lock:      [============================]
  DB row lock:         [====DB UPDATE====]
  Cache coherent:  [============================] ← covered by Redis lock
  Readers:         cache hit or blocked — never stale
```

The Redis lock is fast enough (~0.1-0.5ms) relative to the DB (~1-5ms) that
adding it doesn't meaningfully change the total write latency. It's adding a
~0.1ms lock around a ~5ms operation — the same as adding a ~1μs mutex around a
~50μs operation in C++, which nobody would think twice about.

**Both approaches are serial at the DB — the lock stack just extends it.**

CAS is not truly concurrent for writes to the same key. The DB row lock still
serializes writers — Writer B's UPDATE blocks behind Writer A's UPDATE regardless
of whether a Redis lock exists. The only part that's "concurrent" in CAS is the
Redis layer (INCR can overlap), and that concurrency is exactly what creates the
version divergence that breaks the cache.

```
CAS write serialization:
  Writer A: INCR (concurrent) → DB UPDATE [====row lock====] → SET
  Writer B: INCR (concurrent) ──────────→ DB UPDATE [====row lock====] → SET
            ↑ concurrent here              ↑ serial here (DB row lock)
  Redis: concurrent INCRs → version divergence → cache broken
  DB:    serial anyway (row lock)

Lock stack write serialization:
  Writer A: [=====Redis lock=====DB UPDATE====SET cache=====] → unlock
  Writer B: ──────────────────────────────────────────────────→ [=====...]
            ↑ serial here (Redis lock)    ↑ serial here too (same lock scope)
  Redis: serial (lock) → no divergence → cache works
  DB:    serial anyway (row lock, but now covered by Redis lock)
```

The lock stack extends the DB's existing serialization outward by ~1ms of Redis
operations (SETNX + DEL lock). That's it. The writes were already serial at the
DB — the lock stack just makes the cache consistent with that serialization.

**The cost of that extension:**

```
CAS write:        INCR (~0.1ms) + DB UPDATE (~1-5ms) + SET (~0.1ms) = ~1.2-5.2ms
Lock stack write: SETNX (~0.1ms) + DEL (~0.1ms) + DB UPDATE (~1-5ms)
                  + SET (~0.1ms) + DEL lock (~0.1ms) = ~1.4-5.4ms

Difference: ~0.2ms per write (~4-15% overhead on the write path)
```

For that ~0.2ms per write, readers get cache hits instead of DB fallbacks.
On a read-heavy workload (which most systems are), this is overwhelmingly
favorable — a small write overhead buys a large read benefit:

```
100 reads + 1 write per second, same key:

CAS:        write ~1.2-5.2ms, reads during write all hit DB (~1.2-6ms each)
Lock stack: write ~1.4-5.4ms, reads during write hit cache (~0.1-0.5ms each)

Extra write cost:  ~0.2ms (one time)
Read savings:      ~1-5.5ms × (readers during write) (every time)
```

**Serial is not "bad" — it depends on what you're serializing on.**

Redis itself is single-threaded — every command is serial. It's the fastest
component in the stack. The DB is serial per-row via row locks. Adding one
more serial layer at Redis speed (~0.1ms per op) around DB speed operations
(~1-5ms) is a minor extension of an already-serial path, not a new bottleneck.

**This is why the lock stack is a viable option in this domain, where locks
would be unthinkable on the equivalent hot path in systems programming.** The
lock operates at Redis speed, wrapping DB speed operations. The ratio is
favorable. In C++, locks operate at mutex speed, wrapping atomic-speed
operations — the ratio is prohibitive.

---

## 12. Network Latency Caveat

The latency numbers used throughout this document (~1-5ms for DB, ~0.1-0.5ms
for Redis) are **application-observed round-trip times** — they include network
transport between the application server and the service, not just the
operation itself.

### What the numbers actually measure

**Redis (~0.1-0.5ms observed):**

- Actual operation (GET, SET, DEL): microseconds (in-memory, single-threaded)
- Network RTT (same datacenter): ~0.1-0.5ms
- Redis latency is **almost entirely network**. The operation itself is negligible.

**MySQL (observed latency for PK operations):**

- SELECT and UPDATE processing times are hardware-dependent (NVMe vs SSD vs
  HDD, MySQL flush settings, buffer pool hit rate) — benchmark your setup.
- Network RTT (same datacenter): ~0.1-0.5ms
- MySQL is a **mix** — processing is real (especially writes with fsync), and
  network adds on top. Both components matter.

### Where network latency changes the analysis

The lock stack write path does **5 Redis round trips** (SETNX, GET save old,
DEL cache, Lua SET, DEL lock). CAS does **2** (INCR, SET). Simple does **1**
(DEL).

| Network scenario | Redis RTT | Lock stack (5 RTs) | CAS (2 RTs) | Simple (1 RT) |
|---|---|---|---|---|
| Same machine (localhost) | ~0.01-0.05ms | ~0.05-0.25ms | ~0.02-0.1ms | ~0.01-0.05ms |
| Same datacenter | ~0.1-0.5ms | ~0.5-2.5ms | ~0.2-1ms | ~0.1-0.5ms |
| Cross-datacenter | ~1-10ms | ~5-50ms | ~2-20ms | ~1-10ms |
| Cross-region | ~10-100ms | ~50-500ms | ~20-200ms | ~10-100ms |

Same-DC is the standard assumption and where the analysis holds. The lock
stack's ~0.5-2.5ms Redis overhead is minor relative to ~1-5ms DB. But cross-DC,
the 5 round trips balloon to where Redis overhead **dominates** the write path
— the "Redis is fast relative to DB" assumption breaks.

### Mitigations — pipelining and Lua batching to amortize network latency

The 5 individual Redis round trips in the lock stack can be reduced by batching
multiple commands into fewer network round trips:

**Pre-DB phase** (can pipeline into 1 RT):

```
Pipeline: SETNX lock + GET cache (save old) + DEL cache → 1 round trip
```

**Post-DB phase** (can combine into 1 Lua script, 1 RT):

```
Lua: SET cache {new data, version} + DEL lock → 1 round trip
```

With both optimizations: lock stack drops from **5 RTs to 2 RTs**.

| Network scenario | Redis RTT | Lock stack (5 RTs) | Lock stack pipelined (2 RTs) | CAS (2 RTs) |
|---|---|---|---|---|
| Same datacenter | ~0.1-0.5ms | ~0.5-2.5ms | ~0.2-1ms | ~0.2-1ms |
| Cross-datacenter | ~1-10ms | ~5-50ms | ~2-20ms | ~2-20ms |
| Cross-region | ~10-100ms | ~50-500ms | ~20-200ms | ~20-200ms |

With pipelining, the lock stack's network overhead matches CAS's — both 2 RTs.
The lock stack's extra Redis operations (SETNX, GET, DEL, SET, DEL lock vs
INCR, SET) are batched into the same round trips, so the network cost is
amortized.

Note: CAS can also pipeline its 2 read-path operations (GET version + GET
cache) into 1 RT. But the write path (INCR before DB, SET after DB) can't
be pipelined since the DB operation sits between them.

**The core conclusion:** the analysis throughout this document holds for
same-datacenter deployments. Cross-DC Redis is a different architecture that
would change the tradeoffs — at that point, even the Simple approach's single
RT matters, and the lock stack needs pipelining to remain viable.

### Breakeven analysis: when does CAS beat the lock stack?

**Variables:**

| Variable | What | Typical range (benchmark your system) |
|---|---|---|
| n_r | Network RTT to Redis | 0.01-100ms |
| n_d | Network RTT to DB | 0.01-100ms |
| p | Redis simple op processing (GET/SET/DEL) | ~0.005ms |
| l | Redis Lua script processing | ~0.03ms |
| u | DB UPDATE processing (incl. fsync) | hardware-dependent |
| s | DB SELECT processing | hardware-dependent |

Round trip costs:
- Redis simple: n_r + p
- Redis Lua: n_r + l
- DB UPDATE: n_d + u
- DB SELECT: n_d + s

**Write path total latency:**

```
Simple:              DB(n_d+u) + DEL(n_r+p)                                  = n_r + p + n_d + u
CAS:                 INCR(n_r+p) + DB(n_d+u) + SET(n_r+p)                    = 2n_r + 2p + n_d + u
Lock stack pipelined: Pre-DB Lua(n_r+l) + DB(n_d+u) + Post-DB Lua(n_r+l)    = 2n_r + 2l + n_d + u
Lock stack naive:     SETNX+GET+DEL(3(n_r+p)) + DB(n_d+u) + LuaSET+DELlock(n_r+l + n_r+p) = 5n_r + 4p + l + n_d + u
```

**Reader costs — no write vs during write (separate calculations):**

No write happening:
- Lock stack: n_r + p (cache hit)
- CAS: 2(n_r + p) (version check + cache GET, match)
- Simple: n_r + p (cache hit)

During write — reader arrives at a random point, average wait = 1/2 of blocked period:

Lock stack reader (cache miss, blocked):
- Blocked window (after DEL cache): DB UPDATE + Lua SET = (n_d + u) + (n_r + l)
- Average wait: 1/2(n_d + u + n_r + l)
- After unlock: cache hit (M9): n_r + p
- **Total: 1/2(n_d + u + n_r + l) + (n_r + p) = 3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u**

CAS reader (version mismatch → DB fallback):
- Version check + cache GET: 2(n_r + p)
- DB SELECT blocked behind row lock, average wait: 1/2(n_d + u)
- Then SELECT executes: n_d + s
- **Total: 2(n_r + p) + 1/2(n_d + u) + (n_d + s) = 2n_r + 2p + 3/2 n_d + 1/2 u + s**

Simple reader:
- n_r + p (cache hit, stale — DB committed, DEL not yet sent)

**Note — M8 (serve-if-exists) best case:** Lock stack readers arriving before
the DEL cache or after the Lua SET cache (but before DEL lock) get a cache
hit at n_r + p with no blocking. The formula above models the average blocked
case. M8 makes the real-world average better than the formula suggests.

**Total system cost per write cycle (write + R readers during write):**

```
Approach         Write cost                + R × (reader during write)
───────────────────────────────────────────────────────────────────────────
Simple           n_r + p + n_d + u         + R(n_r + p)
Lock pipelined   2n_r + 2l + n_d + u       + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
Lock naive       5n_r + 4p + l + n_d + u   + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
CAS              2n_r + 2p + n_d + u       + R(2n_r + 2p + 3/2 n_d + 1/2 u + s)
```

Per-reader difference: CAS reader costs (n_d + s) more than lock stack reader —
CAS pays both the row lock wait AND a full DB round trip, lock stack only waits
then gets a cache hit.

**Concrete examples — total system cost with actual values:**

Setup 1: Same datacenter (n_r = n_d = 0.3ms, p = 0.005ms, l = 0.03ms, u = 2ms, s = 0.3ms)

```
Approach         Write     Reader(write)  R=0    R=1    R=5    R=20
─────────────────────────────────────────────────────────────────────
Simple           2.6ms     0.3ms          2.6ms  2.9ms  4.1ms  8.7ms
Lock pipelined   3.0ms     1.6ms          3.0ms  4.6ms  11.1ms 35.4ms
Lock naive       3.9ms     1.6ms          3.9ms  5.5ms  12.0ms 36.3ms
CAS              2.9ms     2.4ms          2.9ms  5.3ms  14.7ms 50.1ms
```

Setup 2: Redis local, DB remote (n_r = 0.01ms, n_d = 5ms, p = 0.005ms, l = 0.03ms, u = 2ms, s = 0.3ms)

```
Approach         Write     Reader(write)  R=0    R=1    R=5    R=20
─────────────────────────────────────────────────────────────────────
Simple           7.0ms     0.02ms         7.0ms  7.0ms  7.1ms  7.3ms
Lock pipelined   7.1ms     3.5ms          7.1ms  10.6ms 24.8ms 77.8ms
Lock naive       7.1ms     3.5ms          7.1ms  10.6ms 24.8ms 77.8ms
CAS              7.0ms     8.8ms          7.0ms  15.9ms 51.2ms 183.6ms
```

Setup 3: Redis remote, DB local (n_r = 10ms, n_d = 0.1ms, p = 0.005ms, l = 0.03ms, u = 2ms, s = 0.3ms)

```
Approach         Write     Reader(write)  R=0    R=1    R=5    R=20
─────────────────────────────────────────────────────────────────────
Simple           12.1ms    10.0ms         12.1ms 22.1ms 62.1ms 212.2ms
Lock pipelined   22.2ms    16.1ms         22.2ms 38.2ms 102.5ms 343.6ms
Lock naive       52.2ms    16.1ms         52.2ms 68.2ms 132.5ms 373.5ms
CAS              22.1ms    21.5ms         22.1ms 43.6ms 129.4ms 451.3ms
```

Setup 4: Localhost (n_r = n_d = 0.02ms, p = 0.005ms, l = 0.03ms, u = 2ms, s = 0.3ms)

```
Approach         Write     Reader(write)  R=0    R=1    R=5    R=20
─────────────────────────────────────────────────────────────────────
Simple           2.0ms     0.03ms         2.0ms  2.1ms  2.2ms  2.5ms
Lock pipelined   2.1ms     1.1ms          2.1ms  3.2ms  7.4ms  23.3ms
Lock naive       2.2ms     1.1ms          2.2ms  3.2ms  7.5ms  23.4ms
CAS              2.1ms     1.4ms          2.1ms  3.5ms  9.0ms  29.7ms
```

Observations:
- **Setup 1 (same-DC):** Lock pipelined beats CAS from R≥1. Lock naive beats
  CAS at R≥2. DB cost (u=2ms) dominates write path for all approaches.
- **Setup 2 (Redis local):** Lock stack's Redis overhead is negligible (~0.01ms
  per RT). CAS readers pay full remote DB cost (~8.8ms each). Lock stack wins
  massively — at R=20, CAS costs 2.4× more.
- **Setup 3 (Redis remote):** Lock stack's 5 naive Redis RTs cost ~50ms. CAS
  write path is cheaper (22ms vs 52ms naive). But CAS readers cost 21.5ms each
  vs lock's 16.1ms — lock stack still wins per-reader. Naive lock stack needs
  R≈75 to catch up; pipelined lock stack beats CAS from R≈2.
- **Setup 4 (localhost):** DB processing (u=2ms) overwhelms everything. All
  approaches within ~1ms of each other at low R.

**Breakeven: naive lock stack vs CAS (full formula):**

Lock < CAS when: (5+R)n_r + (4+R)p + l + n_d + u < (2+2R)n_r + (2+2R)p + (1+R)n_d + u + Rs

Simplifying (dropping p, l as small):
**(3-R)n_r < R(n_d + s)**
**R > 3n_r / (n_r + n_d + s)**

| Scenario | n_r | n_d | Breakeven R |
|---|---|---|---|
| n_r = n_d (same network) | 0.3ms | 0.3ms | R > ~1.0 → **R ≥ 2** |
| n_r << n_d (Redis local, DB remote) | 0.01ms | 5ms | R > ~0.006 → **R ≥ 1** |
| n_r >> n_d (Redis remote, DB local) | 10ms | 0.1ms | R > ~75 |

Key insight: when Redis is closer than DB (common — Redis sidecar/same-host),
the lock stack wins even more easily. When Redis is much further than DB (unusual),
CAS can win if concurrent readers are low.

**When is Simple the best? Always — if staleness is acceptable.** Simple has
the lowest total cost at every n_r, n_d, and R.

**Approximation regimes (when terms drop):**

- **p, l << n_r << u** (same-DC, typical): p and l drop. Formulas simplify to
  coefficients of n_r, n_d, and u only.
- **n_r ≈ p** (localhost): can't drop p, but all Redis terms are tiny vs DB.
  Everything ≈ n_d + u. DB dominates all paths equally.
- **n_r >> u** (cross-region): u drops from comparison. Differences purely in
  n_r coefficients.

**Caveat: MVCC and row lock waits.**

The CAS reader formula includes a 1/2(n_d + u) row lock wait term. This assumes
the reader's DB SELECT blocks behind the writer's UPDATE. In MySQL InnoDB with
default isolation (REPEATABLE READ), a plain SELECT uses MVCC — it reads the
last committed snapshot **without blocking** on row locks.

If MVCC applies:
- CAS reader cost drops to: 2(n_r + p) + (n_d + s) (no wait term)
- But the reader gets v(N) data — the same data the cache already had
- The DB trip was wasted: CAS detected a version mismatch (INCR ran ahead),
  reader went to DB, DB returned the same old committed data the cache had
- This is the P4 problem made concrete: CAS bypasses correct cache data
  during in-flight writes

Whether the row lock wait applies depends on your SELECT type:
- Plain SELECT: MVCC, no wait, but wasted DB trip returning old data
- SELECT FOR UPDATE / locking reads: waits for row lock, gets new data after commit

Either way, the CAS reader pays the DB round trip that lock stack readers avoid.
The row lock wait term changes the exact cost but not the structural advantage.

**Note: longer RTT worsens the problems themselves.**

- CAS: longer n_r = longer mismatch window = more readers fall through to DB
- Simple: longer n_r = longer stale window (DB commit to DEL delivery)
- Lock stack: longer n_r = longer lock hold time, but readers still get cache hit after

---

## 13. Approach Ranking (After Full Analysis)

After the full analysis — failure paths, latency formulas, network caveats,
breakeven math — the ranking:

**1. Simple (M1+M10+M4) — best for most systems.**

Lowest total cost at every n_r, n_d, and R. No Redis write dependency. Simplest
to implement and operate. The ms stale window is self-healing and requires the
same user to trigger concurrent updates within sub-second for staleness to even
occur. Unless zero staleness is a hard requirement, this is the answer.

**2. Full lock stack, pipelined (M2b+M3+M5+M7+M8+M9+M10, with Lua batching).**

Zero staleness with cache that actually works during writes. Write cost matches
CAS (both 2 Redis RTs). Readers get cache hits instead of DB fallbacks. The
pipelining (pre-DB Lua + post-DB Lua) amortizes the Redis network overhead
to match CAS's 2 RTs.

Best when: zero staleness required AND read traffic during writes is non-trivial.

**3. Full lock stack, naive (same mechanisms, no pipelining).**

Same zero-staleness guarantee, same reader benefits. Write path costs 3n_r more
than pipelined (5 Redis RTs vs 2). Simpler to implement — no Lua batching for
the pre-DB phase, just sequential Redis commands.

Still beats CAS at R ≥ 2 concurrent readers during a write, because the
per-reader savings (≈ n_d + s — avoiding a full DB round trip) compound faster
than the 3n_r extra write cost.

Best when: want the lock stack's guarantees without the Lua pipelining complexity,
and your key has at least modest read traffic during writes.

**4. CAS/lease (M6).**

Zero staleness by detecting and bypassing stale cache — readers fall through
to DB on version mismatch. The cache works when no writes are happening but
becomes fiction under write pressure (version divergence, all readers hit DB).

CAS only wins over the naive lock stack when:
- R ≤ 1 concurrent readers during writes (nobody reads during writes)
- OR n_r >> n_d (Redis much further than DB — unusual deployment)

Both conditions are rare. If R ≤ 1, zero staleness likely isn't needed. If
n_r >> n_d, reconsider your Redis placement.

**Why CAS ranks last despite lower complexity:**

- CAS is not truly lock-free — DB row lock serializes writers regardless
- CAS achieves zero staleness by **abandoning the cache**; the lock stack
  achieves it by **keeping the cache correct**
- Under heavy writes, CAS has three-way version divergence (version counter,
  DB, cache data all at different versions); the lock stack has two states
  (correct or empty, never stale)
- With MVCC, CAS readers go to DB and get the same data the cache already
  had — the DB trip is wasted
- The lock stack extends the DB's existing serialization by ~n_r of Redis
  operations, a fraction of the DB cost that buys cache correctness for readers

### Does MVCC change the ranking?

With MVCC (plain SELECT, no row lock wait), the CAS reader cost drops:

```
CAS reader (MVCC):          2(n_r + p) + (n_d + s)       = 2n_r + 2p + n_d + s
Lock stack reader (blocked): 1/2(n_d + u + n_r + l) + (n_r + p) = 3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u
```

CAS reader is faster per-reader when: **n_r + n_d + 2s < u** (dropping small p, l)

**Example** (illustrative — all values are assumptions, benchmark your system):
At same-DC (n_r ≈ 0.3ms, n_d ≈ 0.3ms, s ≈ 0.3ms, u ≈ 2ms):
1.2 < 2 — condition holds. CAS reader ~1.2ms vs lock stack blocked reader ~1.6ms.

In this example, MVCC gives CAS ~0.4ms per-reader advantage over blocked lock
stack readers. **The exact gap depends entirely on your n_r, n_d, s, u values.**
The condition n_r + n_d + 2s < u may not hold on all hardware — fast storage
(low u) or high-latency network (high n_r, n_d) can flip it.

But this doesn't change the ranking:

1. **M8 covers many lock stack readers for free.** Readers arriving before DEL
   cache or after Lua SET cache get n_r + p ≈ 0.3ms — much faster than CAS's
   ~1.2ms. The blocked case (~1.6ms) is the worst case for lock stack, not the
   only case.

2. **After the write, lock stack cache is populated (M9).** Every subsequent
   reader gets n_r + p. CAS version mismatch persists until SET cache completes
   — during heavy writes, subsequent readers keep hitting DB too. The per-reader
   advantage during one write doesn't compound.

3. **CAS's faster trip was wasted.** The CAS reader paid ~1.2ms to get the same
   old data the cache already had (MVCC returns last committed version, which is
   what the cache had). The lock stack reader paid ~1.6ms but got new correct
   data from cache after unlock. One trip contributed useful work, the other
   didn't.

MVCC shaves latency off individual CAS readers during the blocked window, but
the lock stack's total system cost remains lower because M8 serves some readers
instantly, M9 benefits all readers after the write, and CAS's faster DB trip
returned redundant data.

---

#### Current entry task: user management system
- The current scale is ok with the (M1+M10+M4) approach of DB+delete(invalidation on write) + version in key of :v<row_version>
- The roi is not worth the effort of all the moving parts to develop, the updates and changes for the users on a user_id level is only for nickname and avatar, so it is unlikely for them to trigger the stale state which is query cache before cache invalidate (after db update), needs the nickname and avatar to change for the SAME USER within a second for this to happen, unless it is a network or some other failure which is rare and this stale level is for a user so the roi is not worth it

---

## 12. Lock Stack — Reader Cache-Populate Failure Modes

Two scenarios where a slow reader can write stale data to cache despite the
lock stack's protections. Both involve readers that started a DB read before or
during a write, and attempt to cache-populate after the writer has modified DB.

### Scenario 1: Stale write while writer is between DB UPDATE and write-through (FIXED)

```
Reader: miss → DB → gets N → slow
Writer: lock → DEL → DB (N→N+1) → [hasn't written N+1 to cache yet]
Reader: versionSet(N) → cache empty → writes stale N
Writer: write-through N+1 → overwrites → correct again (brief stale window)
```

**Fix:** Reader's cache-populate Lua script atomically checks the lock before
writing. Lock held → skip write. Atomic in Redis (no TOCTOU). Writer uses the
original script (no lock check — it holds the lock).

### Scenario 2: Stale write after writer's write-through fails (OPEN)

```
Reader: miss → DB → gets N → slow
Writer: lock → DEL → DB (N→N+1) → write-through FAILS → releases lock
Reader: versionSet(N) → no lock → cache empty → writes stale N
```

Lock is released, so the scenario 1 fix doesn't help. **Needs further design.**

**Possible fixes (under discussion):**

| Option | How it works | Tradeoff |
|---|---|---|
| A: Accept | Self-heals on next DB-sourced cache populate. Redis was already failing. | Brief stale window (until next read repopulates from DB) |
| B: Shortened lock TTL | On write-through failure, shorten lock to ReadOpTimeout instead of releasing. Slow reader sees lock → skips. | Readers blocked for ReadOpTimeout on failure (rare) |
| C: Tombstone | Replace DEL with a versioned tombstone (`<version>\n`, no data). Version barrier rejects slow readers. Decoded as miss by readers. TTL = LockTTL for crash recovery. | No blocking. But: needs version upfront, tombstone-aware decode, extra DB read if cache was empty |
| D: Lock-wait evidence | Reader only overwrites if it observed a lock-wait (proving it arrived after the writer). | Breaks with multiple writers — which writer did the reader wait for? Lock identity, TOCTOU issues. Needs further discussion. |

**Option C (tombstone)** is the most correct (no timing dependency, no blocking)
but adds complexity. **Option B** is simpler but timing-dependent. **Further
discussion needed** on whether the lock-wait evidence approach (D) can be made
to work with multiple concurrent writers.






@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@







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

### M3: Watchdog + TTL

Background goroutine periodically extends the lock TTL while the writer is
alive. If writer crashes, watchdog stops, TTL expires.

```
Writer:   SET lock EX 5 NX → do work → DEL lock
Watchdog: every 1s → writer alive? EXPIRE lock 5. dead? stop.
```

- Fixes both P6 (starvation) and P8 (premature expiry)
- Normal writes (~2-5ms) finish before watchdog ever fires — near-zero cost

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

- Prevents P2 on the cold path only (cache population)
- No read-path overhead — comparison only on cache writes
- Works naturally with locks (plain keys, no version in key name)

### M6: CAS/lease (INCR → DB → SET)

Version counter in Redis, incremented before DB write. Every read compares
cache version against counter.

```
Write: INCR version → DB UPDATE → SET cache {version, data}
Read:  GET version → GET cache → match? return. mismatch? → DB.
```

- Only single mechanism that prevents both P1 and P2 without a lock
- Read-path overhead: version compare on every read
- Cache degrades under write pressure (constant mismatch → DB fallback, P4)
- Orphaned INCR on failure (P5) — self-heals on next successful write

### M7: Delete-first ordering (inside lock)

Ordering decision: inside a lock, delete cache BEFORE DB update.

```
Inside lock: DEL cache → DB UPDATE → unlock
If DB fails: cache empty → readers hit DB → correct (not stale)
```

- Safety net: any failure after DEL leaves cache empty, never stale
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

---

## 3. Mechanism Comparison Table

| Mechanism | Fixes | Introduces | Read cost | Write cost | Failure |
|---|---|---|---|---|---|
| M1: DB→delete | — | P1 (ms), P3 | 1 GET | 1 DEL | DEL idempotent |
| M2a: Lock manual | P1, P7 | P6 (starvation) | Lock check | SETNX + DEL lock | Held forever on crash |
| M2b: Lock TTL | P1, P7, P6 | P8 (mid-expire) | Lock check | SETNX EX + DEL lock | Expires safely |
| M3: Watchdog | P6, P8 | Complexity | None extra | Periodic extends | TTL expires safely |
| M4: Version-in-key | P2 | Need version for key | None if version known | None extra | Old keys linger |
| M5: Version-in-value Lua | P2 | Lua complexity | None (plain GET) | Lua on population | Atomic, safe |
| M6: CAS/lease | P1, P2 | P4, P5 | Compare EVERY read | INCR + SET | Orphaned version |
| M7: Delete-first | Safe failure order | — | None | None extra | Cache empty = correct |
| M8: Serve-if-exists | Unnecessary blocking | — | None extra | None | — |
| M9: Write-through in lock | P3 (stampede) | Lock ~0.2ms longer | None | 1 extra SET | SET fails → empty |
| M10: Invalidate (lazy) | P7 (DEL idempotent) | P1, P3 | None | 1 DEL | Safe |
| M11: Write-through (eager) | P3 | P7 (writer race) | None | 1 SET | Wrong order = stale |

---

## 4. Key Combinations

Three named combinations are referenced throughout this document. Each is a
specific set of mechanisms chosen to work together:

| Name | Mechanisms | What it is |
|---|---|---|
| **Simple** | M1 + M10 + M4 | DB→delete + invalidate-on-write + version-in-key. No lock, no Lua, no watchdog. Accepts a small ms stale window. |
| **CAS** | M6 | CAS/lease. Version counter + compare on every read. No lock. Zero stale by detecting mismatches on the read path. |
| **Full stack** (or "lock stack") | M2b + M3 + M5 + M7 + M8 + M9 + M10 | Lock with TTL + watchdog + version-in-value Lua + delete-first ordering + serve-if-exists + write-through inside lock + invalidate-on-write. Zero stale by preventing inconsistency on the write path. |

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

### CAS/lease: M6

```
Write: INCR user:<id>:version → DB UPDATE → SET user:<id> {version, data}
Read:  GET version → GET cache → match? return. mismatch? → DB.
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (mismatch detected → DB) |
| P2: Slow-reader overwrite | Fixed (version compare rejects stale writes) |
| P3: DB stampede | Worst (all mismatched readers hit DB) |
| P4: Cache functioning | Broken under write pressure |
| P5: Orphaned state | Until next successful write |
| Read path (no write) | 1 GET + version compare |
| Write path | INCR + DB + SET |
| Redis write dependency | **Yes** — INCR before DB |
| Complexity | Medium |
| Staleness guarantee | No stale reads; P5 = DB fallback on failure |

### Full lock stack: M2b + M3 + M5 + M7 + M8 + M9 + M10

```
Write: SETNX lock → DEL cache → DB UPDATE → Lua SET cache → DEL lock
Read:  GET cache → hit? return (even during lock — M8).
       miss? → Lua check lock → locked? wait → unlocked? cache hit or DB.
```

| Aspect | Value |
|---|---|
| P1: Stale during write | Fixed (readers blocked or served valid cache) |
| P2: Slow-reader overwrite | Fixed (Lua version compare on population) |
| P3: DB stampede | Fixed (cache populated before unlock) |
| P7: Concurrent writers | Fixed (lock serializes) |
| P6: Starvation | Fixed (watchdog + TTL) |
| Read path (no write) | 1 GET (plain, no compare) |
| Read path (during write, cache exists) | 1 GET → cache hit (serve-if-exists) |
| Read path (during write, cache miss) | Lua check → block → cache hit after unlock |
| Write path | SETNX + DEL + DB + Lua SET + DEL lock |
| Redis write dependency | **Yes** — but degrades to simple approach if Redis down |
| Complexity | High |
| Staleness guarantee | **No stale reads** |

---

## 5. Combination Matrix — Problems Solved

| Combination | P1 | P2 | P3 | P4 | P5 | P6 | P7 | P8 | Staleness |
|---|---|---|---|---|---|---|---|---|---|
| M1+M10 (DB→delete) | ms window | Vulnerable | Small | Works | No | No | Safe | N/A | ms + long (P2) |
| M1+M10+M4 (+ version-in-key) | ms window | **Fixed** | Small | Works | No | No | Safe | N/A | **ms only** |
| M6 (CAS/lease) | **Fixed** | **Fixed** | **Worst** | **Broken** | Until next write | No | N/A | N/A | **None; P5 fallback** |
| M11 (write-through no lock) | **Fixed** | Vulnerable | **Fixed** | Works | No | No | **Vulnerable** | N/A | Long (P7) |
| M2b+M3+M7+M10 (lock+watchdog) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | P2 only |
| M2b+M3+M5+M7+M10 (+ version) | **Fixed** | **Fixed** | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** |
| Full stack (+M8+M9) | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **None** |

---

## 6. Read/Write Path Costs

| Combination | Read (no write) | Read (write, cache exists) | Read (write, cache miss) | Write |
|---|---|---|---|---|
| Simple (M1+M10+M4) | 1 GET | 1 GET (ms stale possible) | 1 GET miss → DB ~1-5ms | DB + 1 DEL |
| CAS (M6) | 1 GET + compare | 1 GET + compare → DB | 1 GET + compare → DB | INCR + DB + SET |
| Full stack | 1 GET | 1 GET → **cache hit** | Lua → block → **cache hit** | SETNX + DEL + DB + Lua SET + DEL lock |

Key difference: during writes, the full stack serves cache hits where the
others serve DB round trips. After writes, the full stack's cache is already
populated where the others require the first reader to repopulate.

---

## 7. Best Fit by Workload

| Workload | Best | Runner-up | Worst |
|---|---|---|---|
| High read, low write | **Simple** — cache works, ms stale barely matters, no Redis write dep | CAS — zero stale, low overhead when writes rare | Write-through no lock — P7 risk |
| Low read, high write | **Simple** — cache rarely useful, minimal ops | Lock stack — serializes cleanly | CAS — INCR+SET overhead, cache never serves hits |
| High read+write, ms stale OK | **Simple** — ms stale self-healing, lock overhead not justified | Full stack — correct but heavy | CAS — cache broken under writes, DB hammered |
| High read+write, zero stale | **Full stack** — zero stale + cache works + no stampede | CAS — zero stale, no lock complexity, but cache degrades | Write-through no lock — P7 = long stale |
| Zero stale, no lock complexity | **CAS** — only zero-stale without locks | Simple — ms stale, not zero | Full stack — has locks (unwanted) |
| Redis outage tolerance | **Simple** — writes don't depend on Redis | Full stack with fallback | CAS — INCR before DB = writes blocked |

---

## 8. Why the Lock's Overhead Is Minor

### Readers wait the same time either way

```
Without lock: reader waits for DB row lock → DB SELECT → return
              Total: ~2-10ms (wait + DB read)

With lock:    reader waits for Redis lock → GET cache → return
              Total: ~1.5-5.5ms (wait + cache read)
```

The wait is bounded by the DB write time in both cases. The lock moves the
wait from the expensive place (DB queue, ~1-5ms per read) to the cheap place
(Redis, ~0.1-0.5ms per read). Same wait, better outcome.

### Writer serialization is not "double"

Writers already serialize on the DB row lock. The Redis lock adds ~0.5-1ms
(~10-20% overhead on a ~5ms DB write). Per-user lock scope
(`user:<id>:lock`) means different users don't block each other.

### Redis dependency is real but manageable

```
try:    acquire lock → DEL → DB → SET → release
catch:  Redis down → fallback to DB UPDATE only (degrades to Simple)
```

During Redis outage: writes succeed, reads hit DB. When Redis returns: brief
stale window (same as Simple). Zero-stale guarantee lost temporarily, not
permanently.

---

## 9. Race Condition Scenarios

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
Every reader: GET version → GET cache → mismatch → DB.
Cache serves zero hits. All traffic goes to DB.
✗ Cache is fiction. DB load = no-cache load.
```

### Write-through no lock (M11): concurrent writer race — VULNERABLE

```
1. Writer A: DB UPDATE (data=A) → SET cache = A (delayed by network)
2. Writer B: DB UPDATE (data=B) → SET cache = B
3. Writer A's SET arrives late → SET cache = A (overwrites B)
   DB has B. Cache has A. STALE.
✗ Until TTL or next write.
```

### Full stack: all scenarios handled

```
Concurrent writers: serialized by lock. ✓
Slow reader: Lua rejects stale write. ✓
Writer crash: watchdog stops → TTL expires → cache empty → DB. ✓
Network error (SET fails): DEL already ran → cache empty → DB. ✓
During write (cache exists): serve-if-exists → correct (DB unchanged). ✓
During write (cache miss): blocked → unlock → cache hit (M9). ✓
Redis down: fallback to Simple. ✓
```

---

## 10. The Tradeoff Landscape

```
                    Zero staleness
                         ▲
                         │
         M6 CAS ────────┤──────── Full lock stack
         (simple,        │         (complex,
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

| | Simple | CAS | Full stack |
|---|---|---|---|
| Staleness | ms, self-healing | None | None |
| Cache under writes | Invalidated, first reader repopulates | Broken (constant mismatch) | Works (write-through inside lock) |
| DB load during writes | Medium (readers fall through) | Highest (all readers hit DB) | Lowest (readers get cache) |
| Read-path overhead | None | Compare every read | None |
| Redis write dependency | No | Yes | Yes (degrades gracefully) |
| Complexity | Low | Medium | High |
| Best for | Most systems | Zero stale without locks | Zero stale + cache works under pressure |

The final question is always: **is eliminating a millisecond self-healing stale
window worth the complexity?**

- For most systems, M1+M10+M4 is the answer.
- For zero-stale without complexity, CAS.
- For zero-stale with cache that actually works under pressure, the full lock stack — and its overhead is minor compared to the DB write that dominates the path regardless.


#### Current entry task: user management system
- The current scale is ok with the (M1+M10+M4) approach of DB+delete(invalidation on write) + version in key of :v<row_version>
- The roi is not worth the effort of all the moving parts to develop, the updates and changes for the users on a user_id level is only for nickname and avatar, so it is unlikely for them to trigger the stale state which is query cache before cache invalidate (after db update), needs the nickname and avatar to change for the SAME USER within a second for this to happen, unless it is a network or some other failure which is rare and this stale level is for a user so the roi is not worth it