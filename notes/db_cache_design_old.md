# DB + Cache Consistency — Design Exploration

A systematic exploration of how to keep a cache (Redis) and a database (MySQL)
consistent under concurrent reads and writes. Starts from the naive approach,
identifies every race condition and failure mode, builds up mechanisms to fix
them, and compares the resulting combinations.

---

## 1. The Starting Question

What happens with a three-step update flow: increment version in cache → update
DB → update cache data? The cache stores `{version: ..., data: ...}`.

The immediate concern: three components (version counter, DB, cache data) that
can each be at a different state at any moment. More components = more places
for data to be out of sync.

### The core race (two concurrent writers, A and B)

```
A: INCR version → gets v2
B: INCR version → gets v3
B: UPDATE DB (with v3 data)
B: SET cache {version:3, data:B}
A: UPDATE DB (with v2 data)     ← overwrites B's newer DB data
A: SET cache {version:2, data:A} ← overwrites B's newer cache data
```

DB has A's stale data, cache has A's stale data, version went backwards.

### Can compare-and-swap (CAS) guards fix this?

A CAS guard on both DB and Redis (`UPDATE WHERE version > current`, `SET only if
version > current`) prevents the stale overwrite — A's writes would fail because
v2 < v3.

But there's still a window where DB and cache disagree. Between the DB update
and cache update, readers see mismatched state. And if the cache SET fails
(crash, network), the system is stuck diverged until TTL.

### Why delete-on-write is simpler

Delete the old cache key after the DB write. A stale delete is harmless
(idempotent), and the next reader rebuilds from DB (source of truth). There's
no intermediate state where cache claims to have the "right" version but with
wrong or missing data. Cache either has the correct entry or has nothing (miss →
DB lookup).

---

## 2. Four Basic Approaches

1. **DB first → cache delete** — simplest, small staleness window
2. **CAS/lease** — INCR version → DB update → cache data write
3. **Lock + cache delete → DB update → unlock** — readers block
4. **Lock + watchdog + TTL** — same as 3, with crash safety

---

## 3. Ordering Analysis — DB First vs Cache First

### DB update → cache delete (invalidate after)

- DB fails → nothing changes, cache still correct.
- DB succeeds, DEL fails → stale cache until TTL. Version-in-key mitigates
  (readers who know the new version won't even look at the old key).
- Small stale window between DB commit and DEL.

### Cache delete → DB update (invalidate before)

- DEL succeeds, DB fails → cache is empty, DB unchanged. Readers go to DB, get
  old-but-correct data. Correct, but unnecessary cache miss.
- DEL succeeds, DB succeeds → fine.
- **Worse stale window:** reader arrives after DEL but before DB update, reads
  OLD data from DB, repopulates cache with old data. Now cache has old data and
  DB hasn't even been updated yet.

**DB-first then DEL is better for the no-lock case.** Smaller stale window, no
unnecessary cache misses on DB failure.

**Inside a lock, DEL-first is better** because the lock prevents readers from
sneaking in between DEL and DB update. And DEL-first gives failure safety (DB
fails → cache empty → correct).

---

## 4. Staleness Windows Under Each Approach

### DB→delete (no lock)

Small stale window — reader reads old cache between DB commit and DEL. Window
duration: one Redis round trip (~0.1-0.5ms). Self-healing: next read after DEL
misses cache, reads DB, repopulates.

### CAS/lease

No stale reads — readers always compare version, mismatch → fall through to DB.
But the version counter, DB, and cache data can each be at different versions
during the write. Readers detect this via comparison and bypass cache. Under
write pressure, the system spends most of its time in this mismatched state —
readers constantly falling through to DB.

### Lock (readers block)

No stale reads during the write — readers blocked until write completes. After
unlock, cache is either empty (readers go to DB) or populated (readers get cache
hit). No intermediate state visible.

### Network error analysis

| Approach | DB succeeds, cache op fails | Cache op succeeds, DB fails |
|---|---|---|
| DB→delete | Stale until TTL. Version-in-key mitigates. | Cache empty, DB unchanged. Correct. |
| CAS/lease | Orphaned version → DB fallback until next write | Orphaned INCR, version points to nothing |
| Lock + delete-first | Lock expires, cache empty, readers hit DB. Self-heals. | Cache already deleted, DB unchanged. Correct. |

**Lock with delete-first ordering** has the safest failure mode: any failure
after the DEL leaves cache empty, which is always correct (readers go to DB).

---

## 5. The CAS/Lease Trap — "Lock-Free" That Isn't

CAS/lease appears lock-free and therefore "better" by C++ intuition. But:

### The DB row lock is the real serialization point

Writers still queue on MySQL's row lock regardless of what Redis does.
CAS/lease isn't lock-free — it just moved the lock from Redis to MySQL's row
lock. Writers pay DB contention either way, but CAS adds Redis INCR on top and
gets a worse cache in return.

### CAS readers compete with writers at the DB level

Under write pressure, CAS readers see version mismatch → fall through to DB →
join the DB row lock queue alongside writers. Lock readers wait in Redis
(~0.1ms) then get a cache hit. The lock *shields* readers from DB contention;
CAS *exposes* them to it.

### Reader latency under write pressure

| DB write pressure | DB→delete reader | CAS/lease reader | Lock reader |
|---|---|---|---|
| Low (1 writer) | ~1-5ms (DB read, light) | ~1-5ms (DB read, light) | ~2-6ms (lock wait + cache) |
| Medium (10 writers) | ~5-20ms (DB contended) | ~5-20ms+ (same or worse) | ~2-6ms (bounded) |
| High (100 writers) | ~20-100ms+ (heavily contended) | ~20-100ms+ (same or worse) | ~2-6ms (still bounded) |

Lock reader latency is bounded because readers wait for one lock holder in
Redis, not in the DB queue. CAS reader latency degrades because all readers
join the DB row lock queue alongside writers.

### CAS adds overhead at both read and write layers

On the read path: every read must do a version comparison, even when no write is
happening. On the write path: the DB does a conditional update
(`UPDATE ... WHERE version < ?`) instead of an unconditional write. The
conditional update holds the row lock slightly longer (read current version →
compare → write or abort). Failed CAS writes still consumed a row lock and
produced nothing.

Under write pressure these compound: more mismatches → more DB fallbacks → DB
is slower from conditional writes → readers wait longer → latency degrades for
everyone.

### Can CAS combine with other mechanisms?

CAS isn't inherently un-combinable, but anything you'd add makes part of CAS
redundant:

| CAS + ... | What it fixes | What becomes redundant |
|---|---|---|
| CAS + Lock | P4 fixed — readers block instead of DB fallback | Version compare on every read (lock prevents stale). INCR redundant (lock serializes writers). |
| CAS + Version-in-value Lua | P2 doubly fixed | CAS read-path compare — Lua handles write path, reads don't need to compare |
| CAS + Lock + Watchdog | P1, P2, P4, P6 all fixed | Everything CAS provides. Lock handles P1, Lua handles P2, lock handles P4. CAS's INCR and per-read compare are pure overhead. |

CAS's mechanism is **read-path detection** — check on every read if cache is
stale. Every other mechanism prevents staleness at the **write path** (lock
prevents concurrent access, version-in-value prevents stale overwrites,
delete-first prevents stale-on-failure). Once you have write-path prevention,
read-path detection is redundant overhead.

That's why CAS seems "alone" — it's strictly dominated once you add any
write-path mechanism. It's not that it can't combine; it's that it adds nothing
the other mechanisms don't already cover.

### CAS orphaned version — not permanent

I initially described orphaned versions (INCR succeeds, DB fails) as permanent.
That was wrong — they self-heal on the next successful write:

```
Failed write: INCR → v3, DB fails. Counter=v3, DB=v2, cache=v1.
              All readers: v3 != v1 → DB fallback. Cache useless.

Next write:   INCR → v4, DB succeeds, SET cache {v4, data}.
              Counter=v4, cache=v4. Match. Cache works again.
```

Duration: until the next successful write to that key. Could be milliseconds
for a hot key or hours for a cold one. Not permanent, but not bounded either.

---

## 6. Lock Deep Dive

### Lock duration variants

| | TTL only (auto-expire) | Manual unlock only | Manual unlock + TTL + watchdog |
|---|---|---|---|
| Normal case | Expires after fixed duration regardless of write completion | Writer DELs lock after write. Fast release. | Writer DELs lock. Fastest release. |
| Writer finishes early | Lock held until TTL. Readers blocked unnecessarily. | Released immediately. | Released immediately. |
| Writer slow (slow DB) | TTL expires mid-write. Lock gone, another writer enters. Defeats the lock. | Never expires. Correct but can't distinguish slow from dead. | Watchdog extends. Correct. |
| Writer crashes | Lock expires. Readers unblock. Correct. | Lock held forever. Readers starve. | Watchdog stops extending. TTL expires. Safe. |

Manual unlock + TTL + watchdog is the only variant that handles all cases.

### Watchdog amortization

The watchdog extends the lock TTL periodically (e.g. every 1 second). Normal
lock hold time is ~2-5ms (one DB round trip). The watchdog never fires during a
normal hold. Averaged across thousands of lock acquisitions, the extend cost is
near zero.

### Reader wait mechanism

How readers wait when lock is held:

| Method | Cost | Complexity |
|---|---|---|
| Spin/poll (retry GET every 1ms) | Wastes Redis ops | Low |
| `BLPOP` on a release signal | Efficient, one Redis op | Medium (pub/sub setup) |
| Lua script (check lock + GET in one call) | One round trip, atomic | Medium |
| Don't check lock, just miss cache, hit DB | Defeats the lock's purpose | None |

Lua script is the best balance — one round trip, atomic check:

```lua
local data = redis.call('GET', KEYS[1])  -- cache key
if data then
  return data                             -- hit, serve it
end
if redis.call('EXISTS', KEYS[2]) == 1 then -- lock key
  return 'LOCKED'                          -- caller waits and retries
end
return nil                                 -- normal miss, go to DB
```

### The "serve-if-exists" optimization

Key insight: cache delete is the first step inside the lock, before DB update.
So if the cache entry still exists, the DB hasn't been updated yet — the cached
data IS the current truth.

```
Reader arrival point         Cache GET     Lock     What happens                  Correct?
Before lock acquired         Hit           No lock  Return cached data            Yes — no write in progress
Between lock and cache DEL   Hit           Exists   Return cached data            Yes — DB still has old data
Between cache DEL and unlock Miss          Exists   Wait → unlock → DB or cache   Yes
After unlock                 Miss or Hit   No lock  Normal read path              Yes
```

Readers only block when they MUST (cache miss + lock held). Readers who arrive
before the cache delete get served immediately.

### Lock vs no-lock — what readers actually experience

```
Without Redis lock:
Writer: DB row lock acquired → UPDATE → row lock released
Reader: SELECT → DB round trip

Reader cost: ~1-5ms (DB network round trip)

With Redis lock:
Writer: Redis lock → DEL → DB row lock → UPDATE → SET cache → Redis unlock
Reader: Lua check → blocked → unlock → GET cache

Reader cost: wait ~1-5ms (same DB write time) + ~0.1-0.5ms (cache GET)
```

| | Without Redis lock | With Redis lock |
|---|---|---|
| Reader waits for | DB row lock (or MVCC round trip) | Redis lock release |
| Wait duration | ~1-5ms (DB write time) | ~1-5ms (same DB write time) |
| After wait, reader does | DB SELECT (~1-5ms round trip) | Redis GET (~0.1-0.5ms) |
| Total reader latency | ~2-10ms (wait + DB read) | ~1.5-5.5ms (wait + cache read) |
| DB load from readers | Every reader hits DB | Zero — readers get cache |

The wait time is the same either way — bounded by the DB write. The Redis lock
is strictly better for readers: same wait, then cache hit instead of DB round
trip. DB is protected.

### Writer serialization — not actually "double"

```
Without Redis lock, two writers:
  Writer A: DB row lock → UPDATE → release
  Writer B: DB row lock → UPDATE → release
  (serialize on DB row lock)

With Redis lock, two writers:
  Writer A: Redis lock → DB row lock → UPDATE → release DB → release Redis
  Writer B: waits on Redis lock → then same
  (serialize on Redis lock, which encompasses DB lock)
```

Writers serialize either way. Redis lock just moves the serialization point
earlier. Total time for two sequential writers is roughly the same. The only
extra cost is the Redis ops themselves (~0.5-1ms, ~10-20% on top of ~5ms DB
write).

With per-user Redis locks (`user:<id>:lock`), writers on different users don't
block each other — same as different rows having different DB row locks.

---

## 7. Version-in-Key vs Version-in-Value

### Version-in-key

Cache key: `user:<id>:v<N>`. Reader must know N to form the GET.

**Pros:**
- Prevents slow-reader overwrite by construction (different versions = different
  keys, can't collide)
- Old version key becomes instantly unreachable once any reader knows the new
  version — no delete needed for correctness
- No comparison logic on reads

**Cons:**
- Reader must know the version before reading cache — needs a source (session
  hint, DB lookup)
- Awkward with locks: reader after unlock doesn't know the new version, has to
  go to DB anyway to learn it, which defeats some of the lock's benefit
- Old keys linger until TTL (harmless but uses memory)

### Version-in-value

Cache key: `user:<id>`. Value: `{version: N, data: ...}`.

**Pros:**
- Plain key — reader doesn't need to know version
- Works naturally with locks (reader after unlock just does GET)
- Slow-reader overwrite prevented via Lua script on cache population (compare
  version before writing)

**Cons:**
- Needs Lua script for safe cache population (compare-then-set atomically)
- Version comparison exists but only on the cold path (cache miss → DB read →
  write back to cache), not on every read

### The slow-reader overwrite race (why versioning matters)

```
1. Reader A: GET cache → miss → DB read → gets v5 → hangs (GC, slow network)
2. Writer: lock → DEL → DB update (v5→v6) → SET cache v6 → unlock
3. Reader B: GET cache → v6 → served correctly
4. Reader A: wakes up → SET cache = v5 ← OVERWRITES v6 WITH STALE DATA
```

| | Prevents this? | How | Read-path cost |
|---|---|---|---|
| Version-in-key | Yes, by construction | A writes to `:v5`, B wrote to `:v6`, different keys | Need version to form key |
| Version-in-value + Lua | Yes, atomically | Lua rejects write if cache version >= writer's version | None on reads, Lua on writes |
| Plain key, no version | **No — vulnerable** | Nothing prevents the overwrite | None |
| CAS/lease | Yes | Version comparison | On every read |

### What each mechanism covers

The lock prevents stale reads **during** the write. Versioning prevents stale
**writes to cache** that happen **outside** the lock window:

| | What goes wrong | Lock fixes? | Versioning fixes? |
|---|---|---|---|
| Stale read (during write) | Reader sees old cache while DB updates | Yes (blocked) | Not needed |
| Stale cache write (slow reader) | Old data overwrites new in cache | No (reader was outside any lock) | Yes |

One Lua comparison on cache population (cold path only) is sufficient. No
comparison needed on reads. Lock handles read correctness, versioning handles
write correctness.

---

## 8. Invalidate-on-Write vs Write-Through

### Invalidate-on-write (lazy loading)

Delete cache after write. Next reader repopulates from DB.

- Reader pays the repopulation cost (DB read + cache SET)
- DEL is idempotent — two concurrent DELs produce same result
- **Safe without a lock** — no concurrent writer race
- Cache empty after write → potential DB stampede if many readers

### Write-through (eager loading)

Update cache with new data after write. Readers get cache hit immediately.

- Writer pays the population cost (extra SET)
- Two concurrent SETs can overwrite in wrong order (P7)
- **Unsafe without a lock** — concurrent writer race
- Cache populated after write → no DB stampede

### Inside a lock, write-through becomes safe

The concurrent writer race (P7) that makes write-through dangerous only exists
when two writers can interleave their cache SETs. A lock serializes writers — 
only one touches cache at a time. So write-through's only risk is eliminated.

### Combining both (delete-first + write-through inside lock)

```
Lock → DEL cache → DB update → SET cache with new data → unlock
```

- DEL first: failure safety (if DB fails, cache is empty, correct)
- SET after: performance (cache populated before unlock, readers get cache hit)
- Not contradictory — they handle different concerns

If SET fails after DB succeeds: cache is empty (DEL already ran), readers go to
DB. Correct. The DEL is the safety net for the SET.

---

## 9. Redis Dependency Analysis

Every approach uses Redis for reads (that's the cache). When Redis is down,
reads go to DB regardless.

The difference is writes:

| Approach | Write path touches Redis? | Redis down → writes? | Redis down → reads? |
|---|---|---|---|
| DB→delete | After DB (DEL) | **Writes succeed.** DEL fails, nobody reads stale cache (Redis is down). | All reads hit DB. Slower but correct. |
| CAS/lease | Before DB (INCR) | **Writes blocked** (or skip INCR → version breaks) | All reads hit DB. |
| Full lock stack | Before DB (SETNX) | **Writes blocked** (can't acquire lock) | All reads hit DB. |

DB→delete is the only approach where Redis being down doesn't block writes.

### Graceful degradation for the lock stack

```
try:
    acquire Redis lock
    DEL cache → DB update → SET cache → release lock
catch Redis unreachable:
    fall back to: DB update → (skip cache ops)
```

During Redis outage: writes succeed (DB only), reads hit DB (no cache). When
Redis returns: cache is empty or stale, first readers repopulate. You
temporarily lose the zero-staleness guarantee but don't lose availability.

The question: is zero-staleness worth having a dependency that, when it fails,
degrades to exactly what DB→delete provides anyway?

---

## 10. Problems Catalog

| Problem | What happens | When it occurs | Staleness impact |
|---|---|---|---|
| P1: Stale read during write | Reader gets old cache data while DB is being updated | Between DB update and cache invalidation | Short — ms, self-healing |
| P2: Slow-reader overwrite | Slow reader overwrites fresh cache with stale data | Reader's DB-read-to-cache-write spans a write cycle | Long — until TTL or next write |
| P3: DB stampede | All readers hit DB simultaneously after cache cleared | After cache delete, before repopulation | None (load, not staleness) |
| P4: Cache not functioning | Cache bypassed, every reader hits DB | Sustained write pressure with version mismatch | None (correct but slow) |
| P5: Orphaned state | Version/DB/cache disagree after partial failure | INCR succeeds, DB fails | Until next successful write |
| P6: Reader starvation | Readers blocked indefinitely | Writer crashes holding lock | None (no data served at all) |
| P7: Concurrent writer race | Last cache write wins regardless of DB order | Two concurrent writers using write-through without lock | Long — until TTL or next write |
| P8: Lock expires mid-write | Lock disappears while writer still updating DB | TTL shorter than DB round trip | Varies |
| P9: Staleness guarantee | Can a reader ever be served data not matching DB? | Aggregate of P1, P2, P5, P7 | Depends on which problems solved |

---

## 11. Mechanisms Catalog

### M1: DB→delete (no lock)

The simplest cache-aside invalidation. Writer updates DB, then deletes the
cache key. Next reader misses cache, reads from DB, repopulates cache.

```
Write: DB UPDATE → DEL cache_key
Read:  GET cache_key → hit? return. miss? → DB SELECT → SET cache_key → return.
```

### M2a: Lock, manual unlock only

A per-key Redis lock (`SETNX user:<id>:lock`) that the writer acquires before
touching DB or cache, and releases (`DEL user:<id>:lock`) after finishing.
Readers check for the lock and block if it exists. No automatic expiry — the
writer must explicitly delete the lock.

```
Write: SETNX lock → [locked] → cache/DB ops → DEL lock
Read:  check lock → exists? wait. → doesn't exist? normal read.
```

### M2b: Lock with TTL

Same as M2a but the lock is created with an expiry (`SET lock EX <TTL> NX`).
If the writer crashes or hangs, the lock auto-expires after the TTL and readers
unblock. Risk: if the TTL is shorter than the actual write time, the lock
expires mid-write and another writer can enter.
TTL is configurable — set based on expected write latency (e.g. 500ms–2s).

```
Write: SET lock EX <TTL> NX → [locked, expires after TTL] → cache/DB ops → DEL lock
```

### M3: Watchdog + TTL

A background goroutine (or thread) that periodically extends the lock's TTL
while the writer is still alive and working. Combines with M2b: the lock has a
short TTL, but the watchdog extends it every ~TTL/3 while the writer is active.
If the writer crashes, the watchdog stops, and the TTL expires normally.
(TTL and watchdog interval are configurable — tune based on observed write latency.)

```
Writer goroutine:   SET lock EX <TTL> NX → do work → DEL lock
Watchdog goroutine: every <TTL/3>: if writer alive → EXPIRE lock <TTL> (reset TTL)
                    if writer dead → stop extending → TTL expires
```

Normal write takes ~2-5ms — watchdog never fires. Watchdog only matters on
slow writes or crashes.

### M4: Version-in-key

The cache key includes a version number: `user:<id>:v<N>`. Each write bumps
the version in DB. Readers must know the version to form the key (from session
hint, DB lookup, etc.). Old version keys are physically different keys from new
version keys — a write to `v5` can never overwrite `v6` because they're
different Redis keys entirely.

```
Cache key: user:123:v5 → {nickname: "alice", avatar: "abc123"}
After write: user:123:v6 → {nickname: "bob", avatar: "abc123"}
(v5 key lingers until TTL, harmless — nobody with the new version asks for it)
```

### M5: Version-in-value + Lua

The cache key is plain (`user:<id>`), but the value includes a version field:
`{version: 6, nickname: "bob", ...}`. On cache population (after a DB read),
a Lua script atomically checks if the new version is greater than what's
already cached before writing. Readers do a plain GET — no version comparison
on reads.

```
Read:  GET user:<id> → return whatever is there (plain GET, no compare)
Cache population (after DB read):
  EVAL Lua:
    current = GET key
    if current exists and current.version >= new_version:
      return nil  -- don't overwrite newer data
    SET key {version: new_version, data: ...}
```

The Lua script is atomic — Redis executes it without interleaving, so no race
between the check and the write.

### M6: CAS/lease (INCR → DB → SET)

A version counter in Redis (`user:<id>:version`) is incremented before the DB
write. After the DB write, the cache data is updated with the new version. On
every read, the reader compares the cache data's version against the version
counter — if they don't match, the reader falls through to DB.

```
Write: INCR user:<id>:version → v6 → DB UPDATE → SET user:<id> {v6, data}
Read:  GET user:<id>:version → v6
       GET user:<id> → {v5, data} → v5 != v6 → mismatch → DB fallback
```

This is version-in-value (M5) with read-path enforcement added. M5 only
compares on cache writes (cold path). CAS compares on every read (hot path) —
more overhead, but catches staleness at read time, not just write time. The
only single mechanism that prevents both P1 (stale during write) and P2
(slow-reader overwrite) without a lock.

### M7: Delete-first ordering (inside lock)

Not a mechanism on its own — it's an ordering decision: inside a lock, delete
the cache BEFORE updating the DB. This means if the DB update fails, the cache
is empty (readers go to DB, get old-but-correct data) rather than stale.

```
Inside lock:
  DEL cache → DB UPDATE → (optionally SET cache) → unlock

If DB UPDATE fails:
  Cache is empty (DEL already ran). Readers miss → DB → old data. CORRECT.
  Compare to DB-first inside lock:
    DB UPDATE → SET cache fails → cache has OLD data. STALE.
```

### M8: Serve-if-exists optimization

An optimization for lock-based approaches: if a reader's cache GET returns
data, serve it immediately — even if a lock is held. Correctness reasoning:
cache delete is the first step inside the lock (M7). If the cache entry still
exists, the delete hasn't happened yet, so the DB hasn't been updated yet —
the cached data IS the current truth.

```
Reader during write:
  GET cache → HIT? → serve immediately (DB hasn't changed yet)
  GET cache → MISS? → check lock → blocked → wait → read after unlock
```

Reduces unnecessary blocking: readers who arrive between lock acquisition and
cache delete get served immediately instead of waiting.

### M9: Write-through inside lock

After the DB update (inside the lock), write the new data to cache before
releasing the lock. This means readers after unlock get a cache hit instead of
a miss that triggers a DB stampede.

```
Inside lock: DEL cache → DB UPDATE → SET cache {new data} → unlock
                                     ^^^^^^^^^^^^^^^^^^^^^^^^
                                     this is M9 — eager repopulation
```

Normally write-through is unsafe (concurrent writer race, P7). Inside a lock
it's safe — writers are serialized, only one touches cache at a time.

Combines with M7 (delete-first): DEL is the safety net. If SET fails, cache
is empty (DEL already ran), readers go to DB. Correct.

### M10: Invalidate-on-write (lazy loading)

After a write, delete the cache entry. The next reader repopulates it from DB.
This is the "delete" part of DB→delete. As a standalone concept:

- Lazy: the cost of repopulating cache is paid by the first reader, not the
  writer
- DEL is idempotent: two concurrent DELs produce the same result as one
- Safe without a lock: no concurrent writer race (unlike write-through)

```
Write: DB UPDATE → DEL cache
Reader after: GET cache → miss → DB SELECT → SET cache → return
              ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
              reader pays the repopulation cost
```

### M11: Write-through (no lock)

After a write, update the cache with the new data. No lock around it. Eager:
the writer pays the cost, readers benefit immediately.

```
Write: DB UPDATE → SET cache {new data}
Reader after: GET cache → hit → return (no DB round trip)
```

Unsafe under concurrent writers (P7): if two writers both SET, the last SET
wins regardless of which DB write was more recent. The write order to Redis may
not match the commit order in MySQL.

### M12: Request coalescing (singleflight)

When multiple goroutines/threads request the same key concurrently, only one
executes the underlying call (DB SELECT, cache GET, etc.). The rest wait for
that single result and share it. This is an application-level, in-process
deduplication — not a Redis or DB feature.

```
N concurrent readers for key X, all cache miss:
  Without M12: N DB SELECTs (N connections, N round trips, N row reads)
  With M12:    1 DB SELECT, N-1 wait in-process and share the result
```

The pattern is the same across languages: a concurrent map of in-flight
requests, keyed by the cache/DB key. First caller creates the entry and
launches the work; subsequent callers find the entry and wait on the result.
After completion, the entry is removed.

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

- **Mitigates P3 (stampede)** — collapses N concurrent DB reads into 1 per
  application instance. This is mitigation, not elimination: the 1 remaining
  reader still hits DB (~1-5ms), which is far slower than a cache hit (~0.1ms).
  Reduces the multiplier (N→1 per instance), not the per-reader latency.
- **Composable with any combination** — not specific to CAS or the lock stack.
  Can coalesce DB reads (on cache miss), cache reads (on hot keys), or both.
  The lock stack benefits less (M9 write-through already prevents stampede), but
  M12 acts as a safety net on fallback paths (e.g. writer fails to restore cache).
- **Application-level, in-process only** — each application instance coalesces
  independently. With K instances, up to K DB reads still fire (1 per instance).
  This is not a distributed deduplication — it doesn't coordinate across pods.
- **Shared error propagation** — if the one executing call fails, all N-1 waiters
  also receive the error. This is usually fine (they'd all have failed individually
  anyway), but means a transient error affects N callers instead of potentially
  only some.
- **No write-path impact** — singleflight is a read-path optimization. It doesn't
  change how writes work.

### Summary table

| Mechanism | Fixes | Introduces | Read-path cost | Write-path cost | Failure behavior | Staleness impact |
|---|---|---|---|---|---|---|
| M1: DB→delete (no lock) | — | P1 (small window), P3 (small stampede) | None — 1 GET | 1 DEL | DEL idempotent, safe | ms stale (P1) |
| M2a: Lock, manual unlock | P1, P7 | P6 (starvation on crash) | Lock check | SETNX + DEL lock | Held forever on crash | No stale during write |
| M2b: Lock with TTL | P1, P7, P6 | P8 (expires mid-write) | Lock check | SETNX EX + DEL lock | Expires — safe if TTL > write | No stale during write |
| M3: Watchdog + TTL | P6, P8 | Complexity | None extra | Periodic extends | Dies → TTL expires → safe | Eliminates P8 risk |
| M4: Version-in-key | P2 | Need version to form key | None if version known | None extra | Old keys linger, harmless | Prevents long stale from P2 |
| M5: Version-in-value + Lua | P2 | Lua complexity | None — plain GET | Lua EVAL on population | Atomic, safe | Prevents long stale from P2 |
| M6: CAS/lease (INCR→DB→SET) | P1, P2 | P4, P5 | Compare EVERY read | INCR + SET | Orphaned version (until next write) | No stale; P5 = fallback until next write |
| M7: Delete-first (inside lock) | Safe failure ordering | — | None | None extra | Cache empty on failure | Prevents stale-on-failure |
| M8: Serve-if-exists | Unnecessary blocking | — | None extra | None | — | Serves pre-write data (correct) |
| M9: Write-through inside lock | P3 (stampede) | Lock held ~0.2ms longer | None | 1 extra SET | SET fails → cache empty | Fresh cache immediately |
| M10: Invalidate-on-write | P7 (DEL idempotent) | P1 (small window), P3 | None | 1 DEL | Safe | Lazy — next reader repopulates |
| M11: Write-through (no lock) | P3 | P7 (concurrent race) | None | 1 SET | Wrong order = stale | Eager — but P7 risk |
| M12: Singleflight | P3 (mitigates) | Per-instance only; shared errors | None | None | Shared error propagation | Reduces DB load, not staleness |

---

## 11b. Flow Diagrams — Write Path + Reader Behavior

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

## 12. Mechanism Combinations

### How combinations solve each problem

| Combination | P1 | P2 | P3 | P4 | P5 | P6 | P7 | P8 | P9: Staleness | Redis write dep? |
|---|---|---|---|---|---|---|---|---|---|---|
| M1+M10 (DB→delete) | Small window | Vulnerable | Small stampede | Works when writes rare | No | No | Safe (DEL) | N/A | **ms + long possible (P2)** | No |
| M1+M10+M4 (+ version-in-key) | Small, blast limited | **Fixed** | Small stampede | Works when writes rare | No | No | Safe | N/A | **ms only (self-healing)** | No |
| M6 (CAS/lease) | **Fixed** | **Fixed** | **Worst** | **Broken** | Until next write | No | N/A | N/A | **No stale; P5 fallback** | Yes |
| M11 (write-through, no lock) | **Fixed** | Vulnerable | **Fixed** | Works | No | No | **Vulnerable** | N/A | **Long possible (P7)** | No |
| M2a+M7+M10 (lock manual) | **Fixed** | Vulnerable | Stampede | Works | No | **Vulnerable** | **Fixed** | N/A | **P2 + P6 risk** | Yes |
| M2b+M7+M10 (lock TTL) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Vulnerable** | **P2 + P8 risk** | Yes |
| M2b+M3+M7+M10 (+ watchdog) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **P2 only** | Yes |
| M2b+M3+M5+M7+M10 (+ ver-in-val) | **Fixed** | **Fixed** | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale** | Yes |
| M2b+M3+M5+M7+M8+M10 (+ serve-if-exists) | **Fixed** | **Fixed** | Reduced | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale** | Yes |
| M2b+M3+M5+M7+M8+M9+M10 (full stack) | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale** | Yes (degrades to M1+M10) |
| | | | | | | | | | | |
| *+M12 (singleflight) variants:* | | | | | | | | | | |
| M1+M10+M4+M12 (Simple+SF) | Small, blast limited | **Fixed** | **Mitigated** | Works when writes rare | No | No | Safe | N/A | **ms only (self-healing)** | No |
| M6+M12 (CAS+SF) | **Fixed** | **Fixed** | **Mitigated** | **Broken** | Until next write | No | N/A | N/A | **No stale; P5 fallback** | Yes |
| Full stack+M12 | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale** | Yes (degrades to M1+M10) |

### Staleness guarantee summary

| Combination | Worst-case stale | Duration | Self-healing? | Redis down? |
|---|---|---|---|---|
| M1+M10 (DB→delete) | P1 + P2 | ms + until TTL | P1: yes. P2: at TTL. | Writes fine. No cache. |
| M1+M10+M4 (+ version-in-key) | P1 only | ms | Yes | Writes fine. No cache. |
| M6 (CAS) | None. P5 = fallback. | P5: until next write | Yes | Writes blocked. |
| Full stack | **None** | **N/A** | **N/A** | Degrades to M1+M10 |

### Read/write path costs

| Combination | Read (no write) | Read (during write, cache exists) | Read (during write, cache miss) | Write path |
|---|---|---|---|---|
| M1+M10+M4 (delete + version-in-key) | 1 GET | 1 GET (ms stale possible) | 1 GET miss → DB ~1-5ms | DB + 1 DEL |
| M6 (CAS) | 1 GET + compare | 1 GET + compare → DB ~1-5ms | 1 GET + compare → DB ~1-5ms | INCR + DB + SET |
| M11 (write-through, no lock) | 1 GET | 1 GET (P7 stale possible) | 1 GET miss → DB | DB + 1 SET |
| Full stack (invalidate only) | 1 GET | 1 GET → **cache hit** | 1 Lua → block → DB + Lua SET | SETNX + DEL + DB + DEL lock |
| Full stack (+ write-through) | 1 GET | 1 GET → **cache hit** | 1 Lua → block → **cache hit** | SETNX + DEL + DB + Lua SET + DEL lock |
| | | | | |
| *+M12 (singleflight) variants:* | | | | |
| Simple+M12 | 1 GET | 1 GET (ms stale possible) | 1 GET miss → **1 shared DB** per instance ~1-5ms | DB + 1 DEL |
| CAS+M12 | 1 GET + compare | 1 GET + compare → **1 shared DB** per instance | 1 GET + compare → **1 shared DB** per instance | INCR + DB + SET |
| Full stack+M12 | 1 GET | 1 GET → **cache hit** | 1 Lua → block → **cache hit** (M12 as safety net on fallback) | SETNX + DEL + DB + Lua SET + DEL lock |

---

## 13. Race Condition and Failure Scenarios Per Combination

For each combination, the specific scenarios where things go wrong — concurrent
writers, slow readers, network failures, crashes — and what the reader
experiences.

### M1+M10 (DB→delete, no version, no lock)

**Concurrent writers:**
```
Writer A: DB UPDATE (data=A, v5→v6) → DEL cache
Writer B: DB UPDATE (data=B, v6→v7) → DEL cache
Both DELs succeed. Cache empty. Next reader gets v7 from DB. CORRECT.
(DEL is idempotent — concurrent DELs are harmless.)
```

**Slow reader overwrite (P2) — VULNERABLE:**
```
1. Reader A: GET cache → miss → DB read → gets v5 → hangs
2. Writer:   DB update (v5→v6) → DEL cache
3. Reader B: GET cache → miss → DB read → gets v6 → SET cache = v6
4. Reader A: wakes up → SET cache = v5 ← STALE OVERWRITE
Result: cache has v5, DB has v6. Stale until TTL or next write.
```

**Network failure (DEL fails):**
```
Writer: DB update (v5→v6) succeeds → DEL cache fails (Redis network error)
Result: cache has v5 data, DB has v6. Stale until TTL.
```

**Writer crash between DB and DEL:**
```
Writer: DB update (v5→v6) commits → crash before DEL
Result: cache has v5, DB has v6. Stale until TTL.
```

### M1+M10+M4 (DB→delete + version-in-key)

**Concurrent writers:** Same as above — DEL idempotent. CORRECT.

**Slow reader overwrite (P2) — FIXED:**
```
1. Reader A: GET user:<id>:v5 → miss → DB read → gets v5 → hangs
2. Writer:   DB update (v5→v6) → DEL user:<id>:v5
3. Reader B: GET user:<id>:v6 → miss → DB → gets v6 → SET user:<id>:v6
4. Reader A: wakes up → SET user:<id>:v5 (DIFFERENT KEY — harmless)
Result: user:<id>:v6 has correct data. v5 key lingers until TTL. CORRECT.
```

**Network failure (DEL fails):**
```
Writer: DB update (v5→v6) → DEL user:<id>:v5 fails
Result: old key v5 still exists, but any reader who learns v6 (from session
refresh, new login) asks for user:<id>:v6 → miss → DB → correct.
Readers with old session still see v5 — stale for ms until session refreshes.
```

**Stale read during write (P1) — small window:**
```
Writer: DB update (v5→v6) committed. DEL not yet sent.
Reader: GET user:<id>:v5 → HIT (old data) ← stale for ~0.1-0.5ms
Writer: DEL user:<id>:v5
Next reader: GET user:<id>:v5 → miss. Learns v6 from DB. CORRECT.
```

### M6 (CAS/lease)

**Concurrent writers — no stale but lost updates:**
```
Writer A: INCR version → v6. DB UPDATE WHERE version < v6. SET cache.
Writer B: INCR version → v7. DB UPDATE WHERE version < v7. SET cache.
If A's SET arrives after B's SET: cache has A's older data.
BUT: reader checks version counter (v7) vs cache version (v6) → mismatch → DB.
Result: no stale data served. But A's DB update may also be silently rejected
        by CAS guard → lost update, needs retry logic.
```

**Slow reader overwrite — FIXED:**
```
1. Reader A: version=v5, GET cache {v5, data} → match → hangs with v5 data
2. Writer: INCR→v6, DB update, SET cache {v6, data}
3. Reader A: wakes up, tries to SET cache {v5, data}
   Version compare: v5 < v6 → rejected. Cache keeps v6. CORRECT.
   (Or if version-in-key: writes to v5 key, harmless.)
```

**Orphaned INCR (P5) — DB failure after INCR:**
```
Writer: INCR version → v6. DB UPDATE fails (timeout, constraint violation).
Result: version counter = v6, DB = v5, cache = {v5, data}.
Reader: checks version (v6) vs cache (v5) → mismatch → DB → gets v5. CORRECT.
But: cache is useless until next successful write realigns versions.
Self-heals on next write: INCR→v7, DB succeeds, SET cache {v7}. Cache works.
```

**INCR not idempotent on retry:**
```
Writer: INCR → v6 → network timeout (did it succeed? unknown) → retry INCR → v7
Result: version jumped by 2. Functionally correct (readers see mismatch, hit
DB, get correct data). But version gap is permanent in the counter.
```

**All readers hit DB under write pressure (P4):**
```
High write rate: version INCR every few ms.
Every reader: check version → mismatch (version moved since cache SET) → DB.
Cache serves zero hits. Every read = DB round trip. Cache is fiction.
```

### M11 (write-through, no lock)

**Concurrent writer race (P7) — VULNERABLE:**
```
Writer A: DB UPDATE (data=A) → SET cache = A
Writer B: DB UPDATE (data=B) → SET cache = B
If network delays A's SET: B's SET arrives first, then A's SET overwrites.
Result: DB has B (correct). Cache has A (STALE). Until TTL or next write.
```

**Slow reader overwrite — VULNERABLE (same as M1+M10):**
```
1. Reader A: miss → DB → v5 → hangs
2. Writer: DB (v6) → SET cache = v6
3. Reader A: SET cache = v5 ← STALE
```

### M2b+M3+M7+M10 (lock + watchdog + delete-first, no version)

**Concurrent writers — serialized, CORRECT:**
```
Writer A: SETNX lock → DEL cache → DB UPDATE → DEL lock
Writer B: SETNX fails → waits → lock released → SETNX → DEL → DB → DEL lock
Serialized. No race. CORRECT.
```

**Slow reader overwrite (P2) — VULNERABLE:**
```
1. Reader A: cache miss (no lock held) → DB read → v5 → hangs
2. Writer: lock → DEL → DB (v5→v6) → unlock
3. Reader B: miss → DB → v6 → SET cache = v6
4. Reader A: wakes → SET cache = v5 ← STALE (happened outside lock)
```
Reader A started before any lock existed. Lock can't protect what happens
outside its window.

**Writer crash — FIXED by watchdog:**
```
Writer: SETNX lock (TTL=configurable) → DEL cache → DB UPDATE hangs/crashes
Watchdog: stops extending TTL
After TTL expires: lock releases. Readers unblock. Cache is empty (DEL already ran).
Reader: miss → DB → gets whatever DB has (old if crash before commit,
new if crash after commit). CORRECT.
```

**Network error — Redis dies mid-lock:**
```
Writer: SETNX lock → DEL cache → DB UPDATE succeeds → SET cache fails → DEL lock fails
Result: lock stuck (Redis has it but writer can't delete it). TTL expires.
Cache was deleted but not repopulated. Readers after TTL: miss → DB. CORRECT.
```

### M2b+M3+M5+M7+M10 (lock + watchdog + version-in-value + delete-first)

**All of above plus P2 FIXED:**
```
1. Reader A: miss → DB → v5 → hangs
2. Writer: lock → DEL → DB (v5→v6) → unlock
3. Reader B: miss → DB → v6 → Lua SET (v6 > nothing, succeeds)
4. Reader A: wakes → Lua SET → v5 < v6 → REJECTED. Cache keeps v6. CORRECT.
```

**Network failure during write:**
```
Writer: lock → DEL cache → DB UPDATE → Lua SET fails → DEL lock
Result: cache empty (DEL ran), DB has v6. Lock released.
Reader: miss → DB → v6 → Lua SET (v6 > nothing, succeeds). CORRECT.
```

### M2b+M3+M5+M7+M8+M9+M10 (full stack)

**Same as above, plus M8 (serve-if-exists) and M9 (write-through inside lock):**

```
Write flow: lock → DEL cache → DB UPDATE → Lua SET cache {v6, data} → unlock

Reader arrives between lock and DEL:
  GET cache → HIT (old data, but DB hasn't changed yet) → CORRECT

Reader arrives between DEL and unlock:
  GET cache → miss → check lock → LOCKED → wait
  Writer finishes: Lua SET cache → unlock
  Reader: GET cache → HIT (v6, fresh data) → CORRECT

Reader arrives after unlock:
  GET cache → HIT (v6, fresh data) → CORRECT
```

**Slow reader overwrite — FIXED (Lua version compare on cache writes)**

**Writer crash mid-write:**
```
Writer: lock → DEL cache → DB UPDATE hangs → crash
Watchdog stops. TTL expires. Lock gone. Cache empty.
Reader: miss → no lock → DB → gets old data (DB unchanged). CORRECT.
```

**Concurrent writers — serialized by lock. CORRECT.**

**Redis dies:**
```
Fallback: DB UPDATE → skip cache ops.
Reads all hit DB (Redis down). When Redis returns, cache empty or stale,
first reader repopulates. Brief ms stale possible (degrades to M1+M10).
```

**No scenario produces stale data** (under normal Redis operation).

---

## 13b. Failure Path Analysis — Step-by-Step

The race condition scenarios above show what happens when concurrent actors
interleave. This section covers a different dimension: what happens when
individual steps in the write path fail — crash, Redis down, DB down, network
error — and what the reader experiences in each case.

For each failure, we trace:
1. What state the system is in (which components updated, which didn't)
2. How the writer recovers (or doesn't)
3. What readers see (staleness? latency?)
4. How the system self-heals

### Full lock stack: failure at every step of the write path

The full lock stack write path (M2b+M3+M5+M7+M8+M9+M10) has 5 steps. At each
step the writer can fail. Below traces every failure point systematically.

**Reference: the normal (happy) write path and its latency:**

```
Step 1: SETNX lock EX <TTL> NX       ~0.1-0.5ms
Step 2: GET cache (save old value)   ~0.1-0.5ms
        DEL cache                    ~0.1-0.5ms
Step 3: DB UPDATE                    ~1-5ms
Step 4: Lua SET cache {new data}     ~0.1-0.5ms
Step 5: DEL lock                     ~0.1-0.5ms
                                     ─────────
Normal total:                        ~1.5-7ms
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
the cache to its pre-write state rather than leaving it empty. This avoids an
unnecessary DB stampede when the failure is transient.

Why this matters: without saving the old value, a DB failure leaves cache empty.
Every reader after unlock hits DB. With the saved value, the writer restores
cache to the old (correct, since DB didn't change) state, and readers after
unlock get cache hits. The cost is one extra GET (~0.1-0.5ms) on every write,
paying for a path that rarely fires but saves significant DB load when it does.

---

**Failure 1: SETNX lock fails**

```
Cause: Redis down, or lock already held by another writer.

State after failure:
  Lock:  not acquired (or held by someone else)
  Cache: unchanged (correct)
  DB:    unchanged (correct)

  Nothing has been touched. The system is in the same state as before the
  write attempt.

Writer recovery:
  Redis down → degrade to Simple: DB UPDATE → DEL cache (if Redis comes back).
    This is the graceful degradation path — zero-stale guarantee is temporarily
    lost, but writes succeed and the system behaves like M1+M10.
  Lock held → wait/retry (another writer owns the lock and is mid-write).
    The other writer finishes normally, releases lock, our writer retries.
    This is expected contention, not an error.

Reader impact: none — cache and DB are consistent, normal reads continue.

Writer latency:  ~0ms (immediate failure) or retry overhead
Reader latency:  ~0.1-0.5ms (normal cache hit, completely unaffected)
Staleness:       None
```

**Failure 2: DEL cache fails (after lock acquired)**

```
Cause: Redis connection lost between SETNX and DEL. The lock was acquired
(Redis was up), but then Redis becomes unreachable before the DEL.

State after failure:
  Lock:  acquired (but Redis may be fully down now, so lock visibility unclear)
  Cache: still has old data (DEL never executed)
  DB:    unchanged

  This is an unusual state: we hold a lock on a system that's now unreachable.
  The lock exists in Redis but we can't send further commands.

Writer recovery:
  Redis is down → cannot DEL cache, cannot DEL lock, cannot do any Redis ops.
  Degrade: proceed with DB UPDATE only (Simple mode). The write succeeds at
  the DB level. Cache ops are skipped entirely.
  Watchdog cannot extend lock → TTL expires → lock auto-releases.

  When Redis returns: lock gone (TTL expired), cache has old data, DB has new
  data. Brief stale window until a reader repopulates or next write invalidates.
  This is the same state as Simple (M1+M10) after a failed DEL.

Reader impact during Redis outage:
  If Redis partially up (reads work, writes don't):
    GET cache → HIT old data → correct (DB unchanged at time of read)
  If Redis fully down:
    readers fall through to DB → correct data from DB

Writer latency:  ~0.1-0.5ms (SETNX) + failure detection
Reader latency:  ~0.1-0.5ms (cache hit, old = correct) or ~1-5ms (DB if Redis down)
Staleness:       None during failure. Brief window possible after Redis returns
                 (same as Simple — self-healing on next write or reader repopulation).
```

**Failure 3: DB UPDATE fails (after DEL cache succeeded)**

```
Cause: DB down, constraint violation, deadlock, timeout.

State after failure:
  Lock:  held (writer still alive, watchdog extending)
  Cache: empty (was deleted in step 2)
  DB:    unchanged (old data, update failed — transaction rolled back)

  This is the state M7 (delete-first ordering) was designed for. The cache
  is empty but the DB hasn't changed. Readers who reach DB get correct old
  data. There is never a window where cache has new data and DB has old data
  (which would be stale).

Writer recovery — two paths:

  With saved old value (from step 2 GET/GETDEL):
    SET cache {old data, old version} → DEL lock
    Cache restored to pre-write state. Readers after unlock get cache hits.
    The DB failure becomes invisible to readers — cache has correct old data.

  Without saved old value (or if restore SET also fails):
    DEL lock → cache stays empty → readers after unlock hit DB.
    Correct, but N readers all hit DB (small stampede).
    M12 (singleflight) mitigates: N DB reads → 1 per instance.

  In both cases: DEL lock releases, readers unblock.

Reader impact during recovery (lock still held):
  Cache empty + lock held → reader's Lua check sees LOCKED → blocked.
  M8 (serve-if-exists) has nothing to serve — cache was already deleted.
  Readers must wait for the writer to unlock.

  After unlock:
    With restored cache: GET cache → HIT (old correct data) → ~0.1-0.5ms
    Without restored cache: GET cache → MISS → DB SELECT → ~1-5ms

  Critical: the DB read after unlock is a CLEAN SELECT — the writer's DB
  transaction already rolled back (it failed), so the row lock is released.
  The reader pays 1 DB access, not 2. No row lock wait.

Writer latency:  ~0.5-1.5ms (SETNX + GET + DEL) + DB fail time + ~0.1-0.5ms restore
Reader latency:
  Block time: ~(writer's DB fail time: ~1-5ms for timeout, or <1ms for constraint)
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
  Lock:  held (or expired/unreachable if Redis down)
  Cache: empty (deleted in step 2, new SET failed)
  DB:    has new data (committed)

  DB has moved forward but cache wasn't updated. This is the inverse of
  failure 3: there, DB failed and cache was empty; here, DB succeeded but
  cache is empty. In both cases, cache empty → readers go to DB → correct.

  IMPORTANT: do NOT restore old value here. DB has new data. Restoring old
  cache data would create a stale cache (old data) with new DB data — the
  exact scenario delete-first ordering prevents.

Writer recovery:
  Retry SET once. Redis might have had a transient error.
  If retry fails → DEL lock (or let TTL expire), accept empty cache.
  The first reader to arrive after unlock will repopulate cache from DB
  with the new correct data.

Reader impact during recovery (lock still held):
  Cache empty + lock held → blocked (same as failure 3 during recovery).
  After unlock → cache miss → DB read (correct new data).

  The DB UPDATE already committed → row lock is released. The reader's
  SELECT is a clean read (~1-5ms), with no row lock wait. The block time
  on the Redis lock IS the wait for the writer's operation — readers don't
  also wait separately on a DB row lock.

Reader impact after lock expires (Redis fully down):
  Lock eventually expires (TTL). Cache empty (Redis down).
  Readers fall through to DB → correct new data.

Writer latency:  ~1.5-7ms (SETNX + GET + DEL + DB) + retry ~0.1-0.5ms + DEL lock
Reader latency:
  Block time: ~(remaining lock time after DB committed — typically short,
              since DB UPDATE is the bulk of the work and it already finished)
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
  Cache: has new correct data (Lua SET succeeded in step 4)
  DB:    has new correct data (committed in step 3)

  This is the most benign failure. Everything is consistent — lock, cache,
  and DB all reflect the new state. The lock is just an orphaned artifact
  that will expire via TTL.

Writer recovery:
  Retry DEL lock once. If retry fails → watchdog stops (writer exits) →
  TTL expires → lock auto-deleted. No intervention needed.

Reader impact while lock held:
  GET cache → HIT (new correct data) → serve immediately via M8.
  The lock is held, but M8 doesn't care — cache exists, data is correct,
  serve it. Readers are completely unaffected.

  This is why M8 is important: without it, readers would see a lock and
  block until TTL expires, even though the cache has perfectly good data.

Writer latency:  ~1.5-7ms (full path) + retry ~0.1-0.5ms
Reader latency:  ~0.1-0.5ms (cache hit, correct data, unblocked by M8)
Staleness:       None. Everything is consistent. Lock is a stale artifact,
                 expires via TTL. No reader impact.
```

**Failure 6: Writer crash (at any point after lock acquired)**

```
Cause: process killed, OOM, panic, hardware failure.

State after failure:
  Lock:  held, no writer to release it
  Cache: depends on where crash happened
  DB:    depends on where crash happened

  This is the scenario the watchdog (M3) exists for. With manual unlock only
  (M2a), a crash means the lock is held forever — readers starve. With TTL
  (M2b) + watchdog (M3), the watchdog goroutine dies with the writer process,
  stops extending the TTL, and the TTL expires.

Writer recovery:
  None — writer is dead. Watchdog is dead (same process).
  Lock TTL expires (configurable, e.g. 500ms–2s). System self-heals.

  What did the DB do? If the writer had an open transaction:
  - DB detects the dead connection (TCP keepalive / timeout)
  - DB rolls back the uncommitted transaction
  - Row lock is released
  This happens independently of Redis — the DB manages its own cleanup.

Timeline of crash at each step:

  Crash during step 2 (GET/DEL cache):
    Cache: may be old (GET done, DEL not yet) or empty (DEL done)
    DB: unchanged (hasn't started)
    → Correct either way. Old cache = correct. Empty cache → DB = correct.

  Crash during step 3 (DB UPDATE):
    Cache: empty (DEL completed in step 2)
    DB: transaction uncommitted → rolled back by DB → old data, row lock released
    → Correct. Cache empty → readers hit DB → get old data.

  Crash during step 4 (Lua SET cache):
    Cache: empty (DEL completed, Lua SET not completed)
    DB: committed (step 3 finished) → row lock released
    → Correct. Cache empty → readers hit DB → get new data.

  Crash after step 4 (before step 5 DEL lock):
    Cache: has new correct data (Lua SET completed)
    DB: has new correct data (committed)
    → Correct. M8 serves cache hit immediately. Lock expires, no harm.

Reader impact while lock held (up to TTL expiry):
  Cache exists → M8 serves it (correct, whatever version is there)
  Cache empty → blocked until TTL expires → then DB read (correct)

Reader latency:
  Best:   ~0.1-0.5ms (cache hit via M8 — cache exists, correct data)
  Worst:  blocked up to TTL → cache miss → DB SELECT ~1-5ms
          Total: ~TTL + 1-5ms (TTL dominates)
  +M12:   after TTL expiry, 1 shared DB read per instance

  The worst case (crash during step 3, cache empty, readers blocked for full
  TTL) is why the TTL should be tuned tight — just long enough that the
  watchdog can extend it during normal writes, short enough that crash
  recovery doesn't block readers for too long.

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

**Why lock stack readers don't face DB row lock waits:**

In the lock stack, readers never reach the DB while the writer's transaction is
in-progress. The Redis lock blocks them (or M8 serves them from cache). By the
time readers are unblocked and might need to hit DB, the writer's transaction
has already resolved — committed or rolled back — and the row lock is released.

- Failure 3: DB UPDATE failed → rolled back → row lock released → clean SELECT
- Failure 4: DB UPDATE succeeded → committed → row lock released → clean SELECT
- Failure 6: crash → DB rolls back → row lock released → clean SELECT

This is a structural advantage over CAS, where readers go directly to DB during
the write and are blocked behind the writer's row lock. Lock stack readers pay
at most: block (Redis lock wait, ~1-5ms) + DB SELECT (~1-5ms) = 2 DB access
durations, but never row-lock-wait + DB SELECT (which would be 2 DB accesses
from different causes stacked). CAS readers can pay: version check + row lock
wait + SELECT ≈ 2 DB accesses where the wait and the read both happen at the
DB level.

The invariant: **at no failure point does a reader ever see stale data.** The
cache is either correct (serve it), or empty (fall through to DB, which is
always the source of truth). The delete-first ordering (M7) guarantees this —
cache is destroyed before DB changes, so any intermediate state has either
old-correct-cache or no-cache, never new-DB-with-old-cache.

---

### CAS: failure at every step of the write path

The CAS write path (M6: INCR → DB → SET) has 3 steps plus the ever-present
crash scenario. CAS has a qualitatively different failure mode from the lock
stack: failures can leave the cache **permanently useless** until the next
successful write, not just temporarily empty.

**Reference: the normal (happy) write path:**

```
Step 1: INCR version               ~0.1-0.5ms
Step 2: DB UPDATE                   ~1-5ms
Step 3: SET cache {version, data}   ~0.1-0.5ms
                                    ─────────
Normal total:                       ~1.2-6ms
Normal reader (no write):           ~0.2-1ms (GET version + GET cache + compare)
```

---

**Failure 1: INCR version fails**

```
Cause: Redis down.

State after failure:
  Version counter: unchanged (v(N))
  DB:              unchanged (v(N))
  Cache data:      unchanged (v(N))

  All three components are still in sync. Nothing happened.

Writer recovery:
  Cannot proceed — INCR is required before DB (version must be incremented
  to signal readers that a write is in progress). The write is blocked.

  This is the hard Redis write dependency. Unlike the lock stack, which
  degrades to Simple (DB UPDATE → skip cache ops) when Redis is down, CAS
  CANNOT proceed without the INCR. The version counter IS the mechanism.
  Without it, there's no way to signal readers that the cache may be stale.

  The writer must return an error to the caller. The write simply doesn't
  happen.

Reader impact: none — version and cache match, normal cache hits continue.
  Readers don't even know a write was attempted.

Writer latency:  N/A (write blocked, returns error to caller)
Reader latency:  ~0.2-1ms (normal, completely unaffected)
Staleness:       None — nothing changed.
```

**Failure 2: DB UPDATE fails (after INCR succeeded)**

```
Cause: DB down, constraint violation, deadlock, timeout.

State after failure:
  Version counter: v(N+1) — incremented, orphaned (P5)
  DB:              v(N) — unchanged (update failed, transaction rolled back)
  Cache data:      v(N) — unchanged (hasn't been touched yet)

  This is the orphaned version scenario (P5). The version counter jumped
  ahead but neither DB nor cache followed. Version says v(N+1), everything
  else says v(N). Every reader sees the mismatch and falls through to DB.

Writer recovery:
  None immediate. The orphaned INCR cannot be rolled back:
  - DECR is not safe — another writer may have INCRed again concurrently,
    and DECRing would corrupt their version
  - The version counter is a monotonically increasing value; going backwards
    would break the entire CAS invariant

  The orphaned version is a permanent gap in the version sequence. The
  system self-heals on the next SUCCESSFUL write to this key:
    INCR → v(N+2), DB UPDATE succeeds, SET cache {v(N+2), data}.
    Now version=v(N+2), cache=v(N+2). Match. Cache works again.
    The gap at v(N+1) is harmless — just a skipped number.

Reader impact — two distinct phases:

  PHASE 1: DURING the DB UPDATE attempt (writer in-progress, holding row lock):
    Reader does: GET version → v(N+1). GET cache → {v(N), data}. Mismatch.
    Reader falls through to DB SELECT.
    BUT: the writer holds the row lock on this row (its UPDATE is in-flight).
    Reader's SELECT is blocked behind the writer's row lock.
    
    Reader pays: version check (~0.2-1ms) + row lock wait (~1-5ms) + SELECT (~1-5ms)
    ≈ version check + 2 DB access durations (~2.2-11ms)
    
    This is worse than the lock stack equivalent: the lock stack blocks readers
    on the Redis lock (~fast, ~0.1ms check), then serves them a cache hit.
    CAS sends readers to the DB where they queue behind the row lock (~slow).

  PHASE 2: AFTER the DB UPDATE fails (writer done, row lock released):
    Reader does: GET version → v(N+1). GET cache → {v(N), data}. Mismatch.
    Reader falls through to DB SELECT → clean SELECT (row lock released).
    
    Reader pays: version check (~0.2-1ms) + clean SELECT (~1-5ms) = ~1.2-6ms
    
    This is the SUSTAINED state. Every single reader for this key pays this
    cost. Not just readers caught during the failure — ALL readers, indefinitely,
    until the next successful write resyncs the version.

  Cache is useless in both phases — the version mismatch is permanent until
  the next successful write resolves it.

Recovery timeline:
  Next successful write: INCR → v(N+2), DB UPDATE, SET cache {v(N+2), data}.
  Cache works again. The gap at v(N+1) is harmless.
  
  But WHEN is the next successful write? For a hot key (writes every second),
  recovery is fast. For a cold key (writes once a day), the cache is useless
  for up to a day. This is unbounded.

Reader latency:
  During write attempt: ~0.2-1ms + row lock wait + SELECT ≈ 2 DB accesses (~2.2-11ms)
  After failure (sustained): ~0.2-1ms + clean SELECT ≈ 1 DB access (~1.2-6ms)
  +M12: DB reads coalesced to 1 per instance, but per-reader latency unchanged

Staleness:       None — readers always get correct data from DB on mismatch.
Duration:        Cache useless until next successful write (seconds to hours).
```

**Failure 3: SET cache fails (after DB UPDATE succeeded)**

```
Cause: Redis down after DB commit, network error.

State after failure:
  Version counter: v(N+1) — incremented
  DB:              v(N+1) — committed, new data
  Cache data:      v(N) — old data (SET never happened)

  Propagation stopped at the last step. Version and DB both advanced to
  v(N+1), but cache is stuck at v(N). From the reader's perspective, this
  looks identical to Failure 2 — version mismatch forces DB fallback.

  Key difference from Failure 2: the DB UPDATE SUCCEEDED and COMMITTED.
  The row lock is already released. Readers hitting DB get the NEW data
  (not old data like in Failure 2). And they don't face a row lock wait.

Writer recovery:
  Retry SET once — Redis might have had a transient error.
  If retry succeeds: cache now has {v(N+1), data}. Version matches. Done.
  If retry fails: accept orphaned state. Same sustained impact as Failure 2.

Reader impact:
  Version mismatch → DB fallback → correct NEW data from DB.
  No row lock wait — DB transaction already committed.
  
  This is purely a cache availability problem, not a correctness problem.
  Every read returns correct data; it just comes from DB instead of cache.

Reader latency:
  ~0.2-1ms (version check) + DB SELECT ~1-5ms = ~1.2-6ms (1 DB access, no lock wait)
  +M12: 1 shared DB read per instance

Staleness:       None — mismatch always triggers DB read.
Duration:        Until retry succeeds, or next successful write resyncs.
```

**Failure 4: Writer crash (at any point)**

```
Cause: process killed, OOM, panic.

The crash scenario reduces to one of the previous failures depending on
where in the write path the crash happened:

  Crash during step 1 (INCR):
    Either INCR completed or it didn't.
    If completed → orphaned version (same as Failure 2, phase 2).
    If not completed → nothing changed (same as pre-write state).

  Crash during step 2 (DB UPDATE):
    INCR done. DB transaction in-flight → DB detects dead connection →
    rolls back transaction → row lock released.
    Orphaned version, DB unchanged → same as Failure 2, phase 2.
    
    Note: during the brief window while DB is detecting the dead connection
    and rolling back, readers may still be blocked behind the row lock.
    This is typically fast (DB connection timeout, usually <1s).

  Crash after step 2 (DB committed, before SET):
    INCR done, DB committed (row lock released), cache not updated.
    Same as Failure 3 — no row lock wait for readers.

In all cases: version mismatch → readers fall through to DB → correct data.
Row lock is always released eventually (crash → DB rolls back uncommitted
transactions, or transaction already committed).

Reader latency:
  During crash (if writer's DB UPDATE still in-flight):
    Row lock wait + SELECT ≈ 2 DB accesses (~2.2-11ms)
    (DB detecting dead connection and rolling back)
  After crash (transaction resolved):
    Clean SELECT ≈ 1 DB access (~1.2-6ms)
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

**Key difference: lock stack vs CAS failure recovery**

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
3. **Blast radius:** lock stack failure affects readers during one write cycle.
   CAS failure affects all readers until the next write — potentially unbounded.

---

### CAS back-of-envelope: why DB fallback dominates during writes

The CAS write path is: `INCR version` → `DB UPDATE` → `SET cache {version, data}`.

Between the INCR and the SET, the version counter is ahead of the cache data.
Any reader during this window sees a mismatch and falls through to DB.

The DB UPDATE is the bottleneck — it dwarfs Redis INCR and SET latencies. A
rough breakdown of a typical write (~5ms total):

```
INCR version:    ~0.1-0.5ms   (Redis round trip)
DB UPDATE:       ~1-5ms        (query + commit + fsync)
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
  → row lock released         → SELECT completes (~1-5ms for the read itself)
  Total: ~2-11ms (dominated by row lock wait + SELECT)

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

---

### Heavy writes: CAS state divergence vs lock stack serialization

Under sustained writes to the same key, CAS's three components (version counter,
DB, cache data) can each be at a different version simultaneously. The lock stack
serializes writes, so only one is ever in-flight.

**CAS under heavy writes (3 writers in quick succession):**

```
Write path: INCR version → DB UPDATE → SET cache data
Columns follow write path order: Version | DB (data) | Cache data

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

Total time with useless cache: t1 to t9 ≈ 3 full write cycles (~15-30ms).
During this entire window, every reader for this key pays DB latency.
With N writers overlapping: cache is useless for N write cycles.

Additionally, during the DB UPDATE steps, readers not only fall through to DB
but are blocked behind the writer's row lock — paying ~2 DB access durations
instead of 1. The row lock is held sequentially (DB serializes), but the
version counter is already far ahead.

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
Maximum simultaneous states: 2 (correct cache or empty cache). Never 3.
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

Note: "concurrent" for CAS is misleading — the DB row lock still serializes the
actual DB UPDATEs. CAS's "concurrency" only exists at the Redis layer (INCRs
can overlap). The DB bottleneck is the same either way. See section 16 for why
this means the lock stack's serialization cost is minor.

---

## 13. Best Fit by Workload

### Note on CAS fairness

CAS (M6) compared standalone against multi-mechanism combinations is unfair.
CAS is the **only single mechanism that handles both P1 and P2 without a lock.**
That's genuinely valuable. Its relationship to version-in-value (M5): CAS is
M5 with read-path enforcement added. M5 only compares on cache writes (cold
path). CAS compares on every read (hot path) — more overhead, but covers P1
in addition to P2, without needing any lock.

| Workload | Best | Why | Runner-up | Why | Worst | Why |
|---|---|---|---|---|---|---|
| High read, low write | M1+M10+M4 | Simple, cache works, ms stale barely matters | M6 CAS | Zero stale, read overhead minor when writes rare, cache mostly works | M11 write-through no lock | P7 concurrent writer risk |
| Low read, high write | M1+M10+M4 | Cache rarely useful, minimal ops | M2b+M3+M7+M10 (lock, no version) | Serializes writers cleanly | M6 CAS | Every write = INCR+SET overhead, cache never serves hits |
| High read, high write, ms stale OK | M1+M10+M4 | ms stale self-healing, simple | Full stack (correct but heavy) | M6 CAS | Cache broken under sustained writes, DB hammered |
| High read, high write, zero stale | Full stack | Zero stale + cache works + no stampede | M6 CAS | Zero stale, no lock complexity, but cache degrades under writes | M11 write-through no lock | P7 = long stale, opposite of zero |
| Zero stale, no lock complexity | **M6 CAS** | Only zero-stale without locks | M1+M10+M4 (ms stale, not zero) | Full stack | Has locks (unwanted) |
| High write concurrency, same key | **No ideal** | Full stack: writer serialization. CAS: cache broken. Real tradeoff. | Full stack (correct, writers slower) | M11 no lock | P7 race + no protection |
| Redis outage tolerance | M1+M10+M4 | Writes don't depend on Redis | Full stack with fallback | M6 CAS | INCR before DB = writes blocked |

---

## 14. Real Downsides of the Full Lock Stack

| Downside | Impact | Reality check |
|---|---|---|
| Writer serialization | Writers wait on Redis lock before DB | Writers already serialize on DB row lock. Redis adds ~0.5-1ms (~10-20%). Not double. |
| Redis is write dependency | Redis down → writes blocked | Fallback to lockless mode is straightforward. Degrades to M1+M10. |
| Lua scripts block Redis | Redis single-threaded, Lua blocks all clients | Each script ~microseconds. Only matters at very high throughput. |
| Watchdog goroutine load | N writers = N watchdog goroutines | Watchdog rarely fires during normal holds (~2-5ms). Negligible. |
| Debugging difficulty | More moving parts to trace | Real but bounded — lock → watchdog → DEL → DB → SET → unlock is a fixed sequence. |
| Lock held slightly longer (M9) | Write-through adds ~0.1-0.5ms | Saves every reader ~1-5ms (cache hit vs DB read). Clear net win. |

### The strongest argument for the lock

Readers are going to wait for the write to finish regardless — either in the DB
row lock queue or behind the Redis lock. The DB write time dominates both
paths. The difference is what happens after:

- Without lock: reader waited ~1-5ms, then does DB SELECT (~1-5ms). Total ~2-10ms.
- With lock: reader waited ~1-5ms, then does Redis GET (~0.1-0.5ms). Total ~1.5-5.5ms.

Same wait, better outcome, DB protected. The lock doesn't add wait time — it
moves the wait from the expensive place (DB) to the cheap place (Redis).

---

## 15. Summary

### The simple approach (M1+M10+M4: DB→delete + version-in-key)

- ms stale window, self-healing
- No Redis write dependency
- No lock overhead, no complexity
- Best for: most systems, especially high-read-low-write

### The full lock stack (M2b+M3+M5+M7+M8+M9+M10)

- Zero stale reads
- Cache works under write pressure
- No DB stampede
- Degrades gracefully to simple approach on Redis failure
- Best for: high-read-high-write with zero-staleness requirement

### CAS/lease (M6)

- Zero stale reads — the only single mechanism that handles both P1 and P2
  without any lock
- CAS is really version-in-value (M5) with read-path enforcement: M5 compares
  on cache writes only (cold path), CAS compares on every read (hot path)
- Orphaned versions on failure — but self-healing on next successful write, not
  permanent
- Cache degrades under write pressure (constant version mismatch → DB fallback)
- Read-path overhead on every read, even when no writes happening

**When CAS is the right choice:** zero-staleness required but lock complexity
unwanted/unjustified. CAS gives you zero stale reads as a single, self-contained
mechanism. Under low write pressure, the cache-not-caching problem barely
manifests (versions mostly match → cache hits). It's only under sustained high
writes that the cache becomes fiction.

**When CAS is not the right choice:** high write pressure (cache broken, DB
hammered), or when combined with other mechanisms (lock makes CAS's read-path
compare redundant — the lock already prevents stale reads, so CAS adds overhead
without benefit).

### The tradeoff landscape

```
                    Zero staleness
                         ▲
                         │
         M6 CAS ─────────┤──────── Full lock stack
         (simple,        │         (complex,
          cache degrades │          cache works,
          under writes)  │          DB protected)
                         │
                         │
         M1+M10+M4 ──────┤
         (simple,        │
          ms stale,      │
          always works)  │
                         │
                         └──────────────────────► Complexity
```

The final question is always: **is eliminating a millisecond self-healing stale
window worth the complexity?**
- For most systems, M1+M10+M4 is the answer.
- For zero-stale without complexity, CAS.
- For zero-stale with cache that actually works under pressure, the full lock stack — and its overhead is minor compared to the DB write that dominates the path regardless.

---

## 16. Why Locks Work Here (When They Don't in Low-Level Systems)

### The C++ intuition — and why it's wrong in this domain

In C++ and low-level systems programming, the hierarchy is clear: atomics are
fast (~1-100ns per operation), mutexes are slow (~1-10μs per lock/unlock cycle).
That's a 100-1000× gap. On a hot path doing millions of operations per second,
a mutex turns a nanosecond-scale operation into a microsecond-scale one — a
catastrophic penalty. This is why lock-free data structures, CAS instructions,
and atomics dominate performance-critical C++ code. The intuition "locks are
slow, avoid them on hot paths" is deeply ingrained and correct in that context.

But the DB+cache domain operates at entirely different timescales. The
bottleneck is not the lock — it's the DB access, measured in **milliseconds**,
not microseconds or nanoseconds. When the dominant cost is ~1-5ms, adding a
~0.1-0.5ms Redis lock is noise, not a bottleneck.

```
Operation                  Latency         Relative to DB     Domain
───────────────────────────────────────────────────────────────────────
C++ atomic CAS             ~1-100ns        0.0001-0.001×      Systems
C++ mutex lock/unlock      ~1-10μs         0.001-0.01×        Systems
  → mutex vs atomic: 100-1000× penalty — prohibitive

Redis SETNX (lock acquire) ~0.1-0.5ms      0.02-0.5×          DB+cache
Redis GET/SET/DEL          ~0.1-0.5ms      0.02-0.5×          DB+cache
DB SELECT (simple PK)      ~1-5ms          1× (baseline)      DB+cache
DB UPDATE (commit+fsync)   ~1-5ms          1×                 DB+cache
  → Redis lock vs DB: 5-20% overhead — acceptable
```

The ratio tells the story. In C++, locks are 100-1000× slower than the
operations they protect — that's why they're avoided. In DB+cache, the Redis
lock is 0.02-0.5× the DB operation it protects — a rounding error. The same
engineering principle ("don't put a slow lock around a fast operation") leads to
opposite conclusions in the two domains because the relative speeds are
reversed.

### The system already has locks at every layer

The Redis lock is not introducing serialization into an otherwise concurrent
system. Every layer already serializes:

```
Layer          Lock mechanism             Granularity        Latency
────────────────────────────────────────────────────────────────────────
DB (MySQL)     InnoDB row locks           per-row            ~1-5ms
               Transactions hold row locks for the duration of the write.
               Two writers to the same row serialize on this lock.

Redis          single-threaded event loop per-server         ~μs
               Every Redis command executes atomically. Lua scripts run
               without interleaving. Redis IS a lock — a global one.

Application    Redis SETNX (lock stack)   per-key            ~0.1-0.5ms
               Explicit distributed lock. Same granularity as DB row lock.
```

The DB already serializes concurrent writes to the same row via InnoDB row
locks. Redis already forces ordering via its single-threaded execution model —
every command is atomic, and Lua scripts execute without interleaving. The
application-level Redis lock (SETNX) is not adding a foreign concept to the
system; it's extending the DB's existing per-row serialization outward to also
cover the cache update.

Think of it as scope expansion, not a new lock:

- **Without Redis lock:** DB row lock protects [DB UPDATE]. Cache update happens
  outside this scope — gap exists.
- **With Redis lock:** Redis lock protects [DEL cache → DB UPDATE → SET cache].
  DB row lock still exists inside, protecting the DB portion. The Redis lock
  widens the protected region to include cache operations on both sides.

### What the Redis lock actually does — closing the coherence gap

Without the lock, there's a window between the DB commit and the cache update
where the two systems disagree. Any reader in that window sees stale cache or
has to fall through to DB. The Redis lock closes this gap by preventing readers
from seeing the intermediate state.

```
Without Redis lock (DB→delete, M1+M10):
  DB row lock:     [=======DB UPDATE=======]
  Cache coherent:  [===]                     [====]
                        ↑ gap: DB committed, cache not yet invalidated
  Readers here:         ↑ see stale cache (P1, ms window)

With Redis lock (full lock stack):
  Redis lock:      [========================================]
  DB row lock:          [=======DB UPDATE=======]
  Cache state:     [old]→[empty]→[empty during DB]→[new]→[new]
                    ↑ M8  ↑ M7   ↑ readers blocked  ↑ M9  ↑ unlocked
  Cache coherent:  [========================================]
  Readers:         cache hit (M8) or blocked — never stale
```

The Redis lock is fast enough (~0.1-0.5ms to acquire and release) relative to
the DB operation (~1-5ms) that adding it doesn't meaningfully change the total
write latency. It's adding a ~0.1ms lock around a ~5ms operation. In C++ terms,
that's equivalent to adding a ~1μs mutex around a ~50μs operation — nobody
would think twice about that ratio.

### Both approaches are serial at the DB — the lock stack just extends it

This is the key insight that makes the CAS-vs-lock-stack comparison clear.

CAS is **not truly concurrent** for writes to the same key. The DB row lock
still serializes writers — Writer B's UPDATE blocks behind Writer A's UPDATE
regardless of whether a Redis lock exists. The only part that's "concurrent" in
CAS is the Redis layer (INCR can overlap with other INCRs), and that
concurrency is **exactly what creates the version divergence that breaks the
cache.**

```
CAS write serialization (two writers, same key):

  Writer A: INCR v6 (concurrent) → DB UPDATE [====row lock====] → SET cache
  Writer B: INCR v7 (concurrent) ──────────→ DB UPDATE [====row lock====] → SET cache
            ↑ concurrent here                 ↑ serial here (DB row lock)

  Redis layer: concurrent INCRs → version divergence (v6, v7 both in-flight)
               → cache broken (version counter ahead of cache data)
  DB layer:    serial anyway (InnoDB row lock queues Writer B behind Writer A)

  The "concurrency" at Redis didn't help — it just created version chaos.
  The DB bottleneck is the same either way.
```

```
Lock stack write serialization (two writers, same key):

  Writer A: [=====Redis lock=====DB UPDATE====SET cache=====] → unlock
  Writer B: ──────────────────────────────────────────────────→ [=====...]
            ↑ serial here (Redis lock)     ↑ serial here too (same lock scope)

  Redis layer: serial (SETNX) → no divergence → cache works throughout
  DB layer:    serial anyway (row lock), but now encompassed by Redis lock

  Writer B never touches Redis or DB while A is working.
  No version divergence, no intermediate states, cache is always correct.
```

The lock stack extends the DB's existing serialization outward by ~1ms of Redis
operations (SETNX + DEL lock + DEL cache + SET cache). That's it. The writes
were already serial at the DB — the lock stack just makes the cache consistent
with that serialization instead of letting it diverge during the gap.

### The cost of that extension

Concrete numbers — how much does the lock stack add to the write path compared
to CAS?

```
CAS write path:
  INCR version    ~0.1-0.5ms  (1 Redis round trip)
  DB UPDATE       ~1-5ms      (query + commit + fsync)
  SET cache       ~0.1-0.5ms  (1 Redis round trip)
  Total:          ~1.2-6ms

Lock stack write path:
  SETNX lock      ~0.1-0.5ms  (1 Redis round trip)
  DEL cache       ~0.1-0.5ms  (1 Redis round trip, or GETDEL for restore)
  DB UPDATE       ~1-5ms      (query + commit + fsync — same as CAS)
  Lua SET cache   ~0.1-0.5ms  (1 Redis round trip)
  DEL lock        ~0.1-0.5ms  (1 Redis round trip)
  Total:          ~1.4-7ms

Difference:       ~0.2-1ms per write (~4-15% overhead on the write path)
```

The lock stack adds 2 extra Redis round trips (SETNX + DEL lock) and replaces
a plain SET with a Lua SET. Total overhead: ~0.2-1ms. On a ~5ms write, that's
~4-20% more — not negligible, but far from the 100-1000× penalty that makes
locks unthinkable in C++.

### What that overhead buys — read savings

The write overhead is a one-time cost per write. The read benefit accrues to
every concurrent reader:

```
Example: 100 reads + 1 write per second, same key:

CAS:
  Write: ~1.2-6ms
  Readers during write: version mismatch → DB fallback → row lock wait + SELECT
  Each reader pays: ~2.2-11ms (version check + row lock wait + SELECT)
  DB connections during write: up to 100 (all readers hit DB)

Lock stack:
  Write: ~1.4-7ms
  Readers during write: cache hit (M8) or block → cache hit (M9)
  Each reader pays: ~0.1-0.5ms (cache hit) or ~1-5ms block + ~0.1-0.5ms (cache hit)
  DB connections during write: 0 (all readers served from cache)

Extra write cost:    ~0.2-1ms (one time, one writer)
Read savings:        ~1-10ms × N concurrent readers (every write cycle)
DB load reduction:   N readers × ~1-5ms → 0
```

On a read-heavy workload (which most systems are), this tradeoff is
overwhelmingly favorable. The writer pays a small premium; every reader
benefits. The DB is completely shielded from read traffic during writes.

Even on write-heavy workloads, the lock stack's serialization isn't worse than
CAS — because CAS writers also serialize on the DB row lock. The lock stack
just makes the serialization explicit at a higher level, where the cache can
benefit from it.

### Serial is not "bad" — it depends on what you're serializing on

The word "serial" has negative connotations in systems engineering, but it
describes a property, not a defect. Whether serialization is a problem depends
on what you're serializing on and how fast that serializer is:

| Serializer | Speed | Concern level |
|---|---|---|
| C++ mutex (in-process) | ~1-10μs | Moderate — 100-1000× slower than atomics |
| Network RPC (cross-service) | ~1-50ms | High — dominates the path |
| DB row lock (InnoDB) | ~1-5ms | Unavoidable — DB writes are inherently serial per-row |
| Redis command (single-threaded) | ~0.1-0.5ms | Low — fast relative to DB |

Redis itself is single-threaded — every command is serial. It processes
~100,000+ commands/second precisely because each one is fast (~μs of CPU time,
~0.1-0.5ms including network). It's the fastest component in the entire stack.
Nobody avoids Redis because it's "serial."

The DB is serial per-row via InnoDB row locks. Two writers to the same row
always serialize. Nobody avoids writing to MySQL because "row locks are serial."

Adding one more serial layer at Redis speed (~0.1ms per operation) around DB
speed operations (~1-5ms) is extending an already-serial path by a small
constant. It's not introducing a new bottleneck — it's wrapping the existing
bottleneck (DB) with a thin, fast coordination layer (Redis) so that the cache
can participate in the DB's consistency guarantees.

### The domain analogy

```
C++ hot path:
  The "fast thing":     atomic CAS        ~1-100ns
  The "slow lock":      mutex             ~1-10μs
  Ratio:                100-1000×         → lock is the bottleneck → avoid it

DB+cache path:
  The "fast lock":      Redis SETNX       ~0.1-0.5ms
  The "slow thing":     DB UPDATE         ~1-5ms
  Ratio:                0.02-0.5×         → DB is the bottleneck → lock is cheap

In C++:    lock >> protected operation → lock dominates → avoid lock
In DB+cache: lock << protected operation → DB dominates → lock is free
```

**This is why the lock stack is a viable option in this domain, where locks
would be unthinkable on the equivalent hot path in systems programming.** The
lock operates at Redis speed, wrapping DB speed operations. The ratio is
favorable. In C++, locks operate at mutex speed, wrapping atomic-speed
operations — the ratio is prohibitive.

The engineering principle is the same: don't let the synchronization mechanism
dominate the operation it protects. In C++, that means avoiding locks. In
DB+cache, that means the lock is fine — the DB is the bottleneck regardless,
and the Redis lock adds a fraction of the DB's cost while making the cache
consistent.

---

## 17. Network Latency Caveat

All latency numbers in this document are **application-observed round-trip
times**, which include network transport between the application server and
Redis/MySQL. This is worth making explicit because network latency, not
operation processing time, often dominates — especially for Redis.

### What the numbers actually measure

**Redis (~0.1-0.5ms observed):**

- Actual operation (GET, SET, DEL): microseconds (in-memory, single-threaded,
  O(1) for simple key ops)
- Network RTT (same datacenter): ~0.1-0.5ms
- Redis latency is **almost entirely network**. The operation itself is
  negligible. This means every Redis command in a write path is really "one
  network round trip," not "one Redis operation." When we say "5 Redis
  operations," we're really saying "5 network round trips."

**MySQL (observed latency for PK operations):**

- SELECT and UPDATE processing times are hardware-dependent — NVMe fsync
  (~0.1ms) vs HDD (~10ms), MySQL config (innodb_flush_log_at_trx_commit),
  buffer pool hit rate, and concurrent load all affect the numbers significantly.
  Benchmark your specific setup rather than assuming fixed values.
- Network RTT (same datacenter): ~0.1-0.5ms
- MySQL is a **mix** — processing is real (especially writes with fsync), and
  network adds on top. Unlike Redis, the operation time is not negligible.

### Why this matters — round trip count per approach

Each Redis command in the write/read path is a network round trip. The
approaches differ significantly in how many RTs they need:

**Lock stack write path: 5 Redis round trips**

```
RT 1: SETNX lock
RT 2: GET cache (save old value for restoration)
RT 3: DEL cache
        ── DB UPDATE happens here (not a Redis RT) ──
RT 4: Lua SET cache {new data, version} (or plain SET)
RT 5: DEL lock
```

**CAS write path: 2 Redis round trips**

```
RT 1: INCR version (before DB)
        ── DB UPDATE happens here ──
RT 2: SET cache {version, data} (after DB)
```

**Simple write path: 1 Redis round trip**

```
        ── DB UPDATE happens here ──
RT 1: DEL cache (after DB)
```

**Read paths:**

- Simple/Lock stack (no write): 1 RT (GET cache)
- CAS (no write): 2 RTs (GET version + GET cache), or 1 RT if pipelined
- Lock stack during write: 1 RT (GET cache → M8 serve-if-exists), or 1 Lua RT
  (check lock + GET atomically)

### How network latency scales across deployment scenarios

| Network scenario | Redis RTT | Lock stack (5 RTs) | CAS (2 RTs) | Simple (1 RT) |
|---|---|---|---|---|
| Same machine (localhost) | ~0.01-0.05ms | ~0.05-0.25ms | ~0.02-0.1ms | ~0.01-0.05ms |
| Same datacenter | ~0.1-0.5ms | ~0.5-2.5ms | ~0.2-1ms | ~0.1-0.5ms |
| Cross-datacenter | ~1-10ms | ~5-50ms | ~2-20ms | ~1-10ms |
| Cross-region | ~10-100ms | ~50-500ms | ~20-200ms | ~10-100ms |

At same-DC (~0.1-0.5ms RTT), the lock stack's 5 RTs add ~0.5-2.5ms — still
minor relative to the ~1-5ms DB operation. The "Redis is fast relative to DB"
assumption that underpins the lock stack analysis holds.

At cross-DC (~1-10ms RTT), 5 RTs = ~5-50ms. Redis overhead now **dominates**
the write path, exceeding the DB operation itself. The entire analysis in this
document — that "the lock adds minor overhead" — breaks down. The lock's
overhead would exceed the DB operation it's protecting.

### Mitigation: pipelining and Lua batching to amortize network latency

Redis supports pipelining (sending multiple commands in one network round trip
without waiting for individual responses) and Lua scripts (executing multiple
commands atomically in one round trip on the server). These can dramatically
reduce the lock stack's RT count.

**Pre-DB phase — batch into 1 RT:**

```
Lua script (1 RT):
  local ok = redis.call('SET', KEYS[1], val, 'EX', ttl, 'NX')  -- SETNX lock
  if not ok then return 'LOCKED' end
  local old = redis.call('GET', KEYS[2])                         -- GET cache
  redis.call('DEL', KEYS[2])                                     -- DEL cache
  return old                                                      -- saved value
→ 3 operations, 1 network round trip, atomic
```

Using a Lua script for the pre-DB phase is better than pipelining because
there's a dependency: SETNX must succeed before GET+DEL make sense. The Lua
script handles this atomically — if the lock isn't acquired, it returns
immediately without touching the cache.

An alternative is pipelining SETNX alone (1 RT), then pipelining GET+DEL (1 RT)
after checking the result. This gives 2 RTs for the pre-DB phase but avoids
Lua on the acquisition path.

**Post-DB phase — Lua script into 1 RT:**

```
Lua script (1 RT):
  local current = redis.call('GET', KEYS[1])
  if current and cjson.decode(current).version >= new_version then
    return nil                                                    -- don't overwrite newer
  end
  redis.call('SET', KEYS[1], new_data)                            -- SET cache
  redis.call('DEL', KEYS[2])                                      -- DEL lock
  return 'OK'
→ version compare + SET cache + DEL lock, 1 network round trip
```

The version compare (M5) and SET are already in a Lua script in the full lock
stack design — adding DEL lock to the same script adds zero network overhead.

**Optimized RT count:**

```
Lock stack write path (naive):     5 RTs
  SETNX → GET → DEL → [DB] → Lua SET → DEL lock

Lock stack write path (optimized): 2 RTs
  Pre-DB Lua (SETNX+GET+DEL) → [DB] → Post-DB Lua (SET+DEL lock)

Lock stack write path (conservative, no pre-DB Lua): 3 RTs
  SETNX → Pipeline(GET+DEL) → [DB] → Post-DB Lua (SET+DEL lock)
```

**Updated table with pipelining:**

| Network scenario | Redis RTT | Lock stack naive (5 RTs) | Lock stack pipelined (2-3 RTs) | CAS (2 RTs) | Simple (1 RT) |
|---|---|---|---|---|---|
| Same datacenter | ~0.1-0.5ms | ~0.5-2.5ms | ~0.2-1.5ms | ~0.2-1ms | ~0.1-0.5ms |
| Cross-datacenter | ~1-10ms | ~5-50ms | ~2-30ms | ~2-20ms | ~1-10ms |
| Cross-region | ~10-100ms | ~50-500ms | ~20-300ms | ~20-200ms | ~10-100ms |

With pipelining, the lock stack's network overhead approaches CAS's — both at
2 RTs. The extra Redis operations (SETNX, GET, DEL vs just INCR) are batched
into the same round trips, so the network cost is amortized. You pay for the
round trips, not the individual commands.

**CAS can also optimize:**

- Read path: pipeline GET version + GET cache into 1 RT (from 2)
- Write path: INCR must be before DB, SET must be after — can't reduce below
  2 RTs. The DB is between the two Redis operations, so they can't be batched.

### The core conclusion

The analysis in this document assumes **same-datacenter deployment** (Redis RTT
~0.1-0.5ms). Under this assumption, the lock stack's Redis overhead is minor
and the tradeoff analysis holds as written.

For cross-DC or cross-region Redis, the picture changes:

- The lock stack needs pipelining/Lua batching to remain viable — without it,
  5 RTs at cross-DC latency dominates the write path
- With pipelining (2 RTs), the lock stack's network cost matches CAS's
- CAS's inherently lower RT count (2, can't reduce further) becomes a real
  structural advantage in the naive (non-pipelined) case
- The Simple approach's single RT makes it the most network-resilient of all
  three — and it doesn't depend on Redis for writes at all
- At extreme latencies (~100ms RTT), all cache approaches add significant
  write-path overhead, and the question shifts from "which cache strategy" to
  "should this key be cached at all in a remote Redis, or should the
  application use a local/regional cache instead"

### Breakeven analysis: when does CAS beat the lock stack?

**Variables — keeping all components explicit:**

Redis and DB may not be at the same network distance (Redis could be a sidecar
on the same host, DB could be a separate cluster, or vice versa). So we use
separate network RTT variables.

| Variable | What | Typical range (benchmark your system) |
|---|---|---|
| n_r | Network RTT to Redis | 0.01-100ms |
| n_d | Network RTT to DB | 0.01-100ms |
| p | Redis simple op processing (GET/SET/DEL/SETNX) | ~0.005ms |
| l | Redis Lua script processing | ~0.03ms |
| u | DB UPDATE processing (incl. commit, fsync) | hardware-dependent (NVMe vs SSD vs HDD, flush settings) |
| s | DB SELECT processing (PK lookup, buffer pool) | hardware-dependent |

Round trip costs:
- Redis simple: n_r + p
- Redis Lua: n_r + l
- DB UPDATE: n_d + u
- DB SELECT: n_d + s

Note: u and s are left as variables intentionally. They vary enormously by
hardware (NVMe fsync ~0.1ms vs HDD ~10ms), MySQL config (innodb_flush_log_at_trx_commit),
buffer pool hit rate, and concurrent load. Benchmark your specific setup.

**Write path derivation:**

Simple: DB UPDATE → DEL cache (sequential)
= (n_d + u) + (n_r + p)
= **n_r + p + n_d + u**

CAS: INCR → DB UPDATE → SET cache (sequential — INCR before DB, SET after)
= (n_r + p) + (n_d + u) + (n_r + p)
= **2n_r + 2p + n_d + u**

Lock stack naive: SETNX → GET old → DEL cache → DB UPDATE → Lua SET → DEL lock
= (n_r+p) + (n_r+p) + (n_r+p) + (n_d+u) + (n_r+l) + (n_r+p)
= **5n_r + 4p + l + n_d + u**

Lock stack pipelined: Pre-DB Lua(SETNX+GET+DEL) → DB UPDATE → Post-DB Lua(SET+DEL lock)
= (n_r+l) + (n_d+u) + (n_r+l)
= **2n_r + 2l + n_d + u**

**Reader costs — no write vs during write:**

These are separate calculations. "No write" is the steady-state read cost.
"During write" is what a reader pays when arriving while a write is in-flight.

No write happening:
- Lock stack: n_r + p (cache hit)
- CAS: 2(n_r + p) (GET version + GET cache, version matches → cache hit)
- Simple: n_r + p (cache hit)

During write — modeled as reader arriving at a uniformly random point during
the blocked period, so average wait = 1/2 of the remaining blocked time:

**Lock stack reader (cache miss, blocked after DEL cache):**
- The blocked window starts after DEL cache: DB UPDATE + Lua SET = (n_d + u) + (n_r + l)
- Reader arrives uniformly in this window → average wait: 1/2(n_d + u + n_r + l)
- After unlock: GET cache → hit (M9 wrote cache before unlocking): n_r + p
- Total: 1/2(n_d + u + n_r + l) + (n_r + p)
- = **3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u**

**CAS reader (version mismatch → DB fallback):**
- Version check + cache GET: 2(n_r + p)
- Mismatch detected → DB SELECT, but writer holds row lock during DB UPDATE
- Row lock held for: n_d + u. Reader arrives mid-UPDATE → average wait: 1/2(n_d + u)
- After row lock released, SELECT executes: n_d + s
- Total: 2(n_r + p) + 1/2(n_d + u) + (n_d + s)
- = **2n_r + 2p + 3/2 n_d + 1/2 u + s**

**Simple reader (during write):**
- GET cache → hit (stale for ms — DB committed but DEL not sent yet)
- = **n_r + p**

**Note — M8 (serve-if-exists) best case for lock stack:** Readers arriving
between SETNX and DEL cache (cache still has old correct data) or between Lua
SET cache and DEL lock (cache has new correct data) get a cache hit at n_r + p
with zero blocking. The average formula above models the blocked case. In
practice, M8 makes real-world lock stack reader cost lower than the formula.

**Total system cost per write cycle (write + R readers during write):**

```
Approach         Write cost                + R × (reader during write)
───────────────────────────────────────────────────────────────────────────
Simple           n_r + p + n_d + u         + R(n_r + p)
Lock pipelined   2n_r + 2l + n_d + u       + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
Lock naive       5n_r + 4p + l + n_d + u   + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
CAS              2n_r + 2p + n_d + u       + R(2n_r + 2p + 3/2 n_d + 1/2 u + s)
```

Per-reader cost difference (CAS - Lock stack):
= (2n_r + 2p + 3/2 n_d + 1/2 u + s) - (3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
= **1/2 n_r + p - 1/2 l + n_d + s**

Each CAS reader pays roughly n_d + s more than a lock stack reader — an extra
full DB round trip. The lock stack reader waits (1/2 the blocked period) then
gets a cache hit. The CAS reader waits (1/2 the row lock) then does a full DB
SELECT on top.

**Deriving the breakeven: naive lock stack vs CAS (full variables)**

Lock naive total < CAS total when:

(5n_r + 4p + l + n_d + u) + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
< (2n_r + 2p + n_d + u) + R(2n_r + 2p + 3/2 n_d + 1/2 u + s)

Left - Right:
(3n_r + 2p + l) + R(3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u) - R(2n_r + 2p + 3/2 n_d + 1/2 u + s)
= (3n_r + 2p + l) + R(-1/2 n_r - p + 1/2 l - n_d - s)
= (3n_r + 2p + l) - R(1/2 n_r + p - 1/2 l + n_d + s)

Lock < CAS when this < 0:
(3n_r + 2p + l) < R(1/2 n_r + p - 1/2 l + n_d + s)

Approximating (p, l small relative to n_r and n_d):
**3n_r < R(1/2 n_r + n_d + s)**
**R > 3n_r / (1/2 n_r + n_d + s)**

Or equivalently: **R > 6n_r / (n_r + 2n_d + 2s)**

| Scenario | n_r | n_d | s | Breakeven R |
|---|---|---|---|---|
| Same network (n_r = n_d) | 0.3ms | 0.3ms | 0.3ms | R > 6(0.3)/(0.3+0.6+0.6) = 1.2 → **R ≥ 2** |
| Redis local, DB remote | 0.01ms | 5ms | 0.3ms | R > 6(0.01)/(0.01+10+0.6) ≈ 0.006 → **R ≥ 1** |
| Redis remote, DB local | 10ms | 0.1ms | 0.3ms | R > 6(10)/(10+0.2+0.6) = 5.6 → **R ≥ 6** |
| Both remote, equal | 5ms | 5ms | 0.3ms | R > 6(5)/(5+10+0.6) = 1.9 → **R ≥ 2** |

Key insights:
- **n_r = n_d (typical):** breakeven at R ≥ 2, same as before.
- **n_r << n_d (Redis sidecar, DB remote):** lock stack wins at R ≥ 1 — its
  overhead is cheap (local Redis), CAS readers pay expensive (remote DB).
- **n_r >> n_d (unusual — Redis remote, DB local):** CAS can win up to R=5.
  Lock stack's 5 remote Redis RTs are expensive while CAS readers hit cheap local DB.
- **Both remote:** still R ≥ 2. The n_d term in CAS reader cost dominates.

With pipelining (2n_r + 2l vs 2n_r + 2p on write path — nearly equal):
Pipelined lock < CAS when: reader savings alone justify it, which is true for
any R ≥ 1 since each reader saves ~(n_d + s).

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
  R≈6 to catch up; pipelined lock stack beats CAS from R≈2.
- **Setup 4 (localhost):** DB processing (u=2ms) overwhelms everything. All
  approaches within ~1ms of each other at low R.

**When is Simple the best? Always — if staleness is acceptable.**

Simple has the lowest total cost at every n_r, n_d, R:
- vs pipelined lock stack: saves n_r + (2l - p) ≈ n_r on write, saves
  R(1/2 n_r + 1/2 l + 1/2 n_d + 1/2 u) on readers (lock stack readers wait,
  Simple readers get stale cache hit immediately)
- vs CAS: saves n_r + p on write, saves R(n_r + p + 3/2 n_d + 1/2 u + s) on readers

The question is never latency. Simple always wins on latency. The question
is whether the stale window is acceptable.

**Approximation regimes (when terms drop):**

| Regime | Condition | What simplifies |
|---|---|---|
| Typical same-DC | p, l << n_r << u | Drop p, l. Formulas become coefficients of n_r, n_d, u, s. |
| Localhost | n_r ≈ p | Can't drop p. But all Redis terms tiny vs DB. Everything ≈ n_d + u. |
| Cross-region | n_r >> u | u drops from comparison. Differences purely in n_r, n_d coefficients. |
| Redis ≈ DB distance | n_r ≈ n_d | Replace both with r. Formulas simplify to single-variable. |

**Caveat: MVCC and row lock waits.**

The CAS reader formula includes a 1/2(n_d + u) row lock wait term, assuming
the reader's DB SELECT blocks behind the writer's UPDATE. In MySQL InnoDB with
default isolation (REPEATABLE READ or READ COMMITTED), a plain SELECT uses
MVCC — it reads the last committed snapshot **without blocking** on row locks.
Only locking reads (SELECT FOR UPDATE, SELECT LOCK IN SHARE MODE) block.

If MVCC applies (plain SELECT for cache population):
- CAS reader cost drops to: 2(n_r + p) + (n_d + s) — no row lock wait
- But the reader gets v(N) data — the **same data the cache already had**
- The version counter is at v(N+1) (INCR ran ahead), DB hasn't committed yet
- So the reader went to DB and got the same old committed data → wasted trip
- This is P4 (cache not functioning) made concrete: CAS bypasses correct cache
  during in-flight writes, then DB returns the same data the cache had

If locking reads (SELECT FOR UPDATE):
- Reader blocks behind row lock, waits for commit, gets v(N+1) — fresh data
- Higher latency but the DB trip at least returned new data

Either way, the CAS reader pays a DB round trip (n_d + s) that lock stack
readers avoid entirely. The row lock wait term affects the exact cost but
doesn't change the structural advantage: lock stack readers get cache hits,
CAS readers hit DB.

With MVCC, the breakeven formula changes slightly (drop the 1/2(n_d+u) from
CAS reader cost), but the n_d + s term remains — CAS readers still pay a full
DB round trip that lock stack readers don't.

**Note: longer RTT worsens the problems themselves (not just overhead).**

- **CAS:** longer n_r = longer INCR-to-SET window = more readers see mismatch
  during writes. Longer n_d = more expensive wasted DB trips.
- **Simple:** longer n_r = longer stale window (gap between DB commit and DEL
  delivery). At n_r=0.1ms the window is ~0.1ms; at n_r=10ms it's ~10ms.
- **Lock stack:** longer n_r = longer lock hold time = readers blocked longer.
  But correctness is RTT-invariant — readers always get correct cache hit after,
  regardless of how long they waited.

The lock stack's zero-staleness guarantee holds at any RTT. CAS and Simple's
staleness properties degrade as RTT increases.

---

## 18. Approach Ranking (After Full Analysis)

After the full analysis — failure paths, latency formulas with separate variables
(n_r, n_d, p, l, u, s), network caveats, breakeven math, MVCC considerations,
heavy write scenarios — the ranking:

**1. Simple (M1+M10+M4) — best for most systems.**

Lowest total cost at every n_r, n_d, and R. No Redis write dependency — writes
succeed even when Redis is down. Simplest to implement and operate — no locks,
no watchdog, no Lua scripts, no version counters.

The ms stale window (P1) is the only correctness cost. It requires a reader to
hit cache in the ~n_r window between DB commit and DEL delivery, for a key that
was just written — and with version-in-key (M4), even that reader must be using
the old version's key. In practice this means the same user triggering concurrent
updates within sub-second, which is rare for most workloads.

Unless zero staleness is a hard, demonstrable requirement, this is the answer.
The complexity of the other approaches is not free — more moving parts means
more failure modes, harder debugging, and operational overhead that must be
justified by the staleness requirement.

**2. Full lock stack, pipelined (M2b+M3+M5+M7+M8+M9+M10, with Lua batching).**

Zero staleness with cache that actually works during writes. Write cost matches
CAS (both ~2n_r + n_d + u). Readers during writes get cache hits (n_r + p via
M8, or 3/2 n_r + 1/2 n_d + 1/2 u average for blocked readers via M9) instead
of DB fallbacks.

The pipelining reduces 5 Redis RTs to 2 by batching:
- Pre-DB: Lua script (SETNX + GET + DEL) → 1 RT
- Post-DB: Lua script (version-compare SET + DEL lock) → 1 RT

This makes the write path network cost match CAS while retaining all the lock
stack's reader-side benefits. The implementation complexity is higher (Lua
scripts must be correct, deployed, and maintained), but the latency benefit
is clear.

Best when: zero staleness required AND read traffic during writes is
non-trivial AND you can invest in the Lua script implementation.

**3. Full lock stack, naive / no pipelining (same mechanisms, sequential commands).**

Same zero-staleness guarantee, same reader benefits. Write path costs 3n_r
more than pipelined (5 Redis RTs vs 2). Each extra RT is a network round trip
to Redis — at same-DC (n_r ~0.1-0.5ms), that's ~0.3-1.5ms extra; at cross-DC,
it compounds.

Simpler to implement than the pipelined variant: just sequential Redis commands,
no Lua batching for the pre-DB phase. The post-DB Lua (version-compare SET,
M5) is still needed for correctness, but the pre-DB commands can be plain
SETNX → GET → DEL.

Still beats CAS at R ≥ 2 concurrent readers during a write. The math:
- Extra write cost over CAS: ~3n_r
- Per-reader savings over CAS: ~(n_d + s) (CAS reader hits DB, lock reader gets cache)
- At R=2: savings = 2(n_d + s) ≈ 2n_d, which exceeds 3n_r whenever n_d > 1.5 n_r
- At same network (n_r ≈ n_d): 2n_d > 3n_r → 2 > 3 is false...

Actually, the full breakeven from section 17 gives R > 6n_r / (n_r + 2n_d + 2s).
At n_r = n_d, s small: R > 6n_r / 3n_r = 2. So R ≥ 3 to be safe.
At n_r << n_d: R ≈ 1.
At n_r >> n_d: R grows — naive lock stack is expensive when Redis is far.

Best when: want the lock stack's guarantees without Lua pipelining complexity,
your Redis is same-DC or closer, and your key has at least modest read traffic
during writes.

**4. CAS/lease (M6).**

Zero staleness by detecting and bypassing stale cache — readers fall through to
DB on version mismatch. The cache works when no writes are happening but becomes
fiction under write pressure.

The analysis surfaced several structural weaknesses:

- **Not truly lock-free.** DB row lock serializes writers regardless of Redis.
  CAS moves the consistency problem from "prevent at write time" to "detect at
  read time," but doesn't eliminate serialization.

- **Cache abandoned, not corrected.** The lock stack achieves zero staleness by
  keeping the cache correct (write-through inside lock). CAS achieves it by
  bypassing the cache whenever it might be wrong. One is a solution, the other
  is a workaround.

- **Three-way version divergence under heavy writes.** Version counter, DB, and
  cache data can all be at different versions simultaneously. The lock stack has
  only two states: correct cache or empty cache. Never three-way divergence.

- **MVCC wasted trips.** With plain SELECTs (InnoDB MVCC), CAS readers go to DB
  during in-flight writes and get the same old committed data the cache already
  had. The DB trip was wasted — CAS detected a "stale" cache that was actually
  correct (version counter ran ahead of the DB commit).

- **Sustained failure impact.** An orphaned INCR (DB fails after version
  increment) leaves the cache useless until the next successful write. Duration
  depends on write frequency — seconds for hot keys, hours for cold. The lock
  stack recovers cache immediately after any failure.

CAS only wins over the naive lock stack when:
- R ≤ 1 concurrent readers during writes (nobody reads during writes)
- OR n_r >> n_d (Redis much further than DB — unusual deployment)

Both conditions are rare in practice. If R ≤ 1, zero staleness likely isn't
needed (nobody observes the stale window). If n_r >> n_d, the Redis placement
should be reconsidered.

**The one thing CAS does well:** it's a single mechanism that handles both P1
and P2 without any lock. For a team that needs zero staleness but cannot accept
the operational complexity of locks + watchdog + Lua, CAS is a pragmatic choice
— as long as write pressure is low enough that the cache-not-caching problem
(P4) doesn't manifest.

**Summary table:**

| Rank | Approach | Staleness | Cache during writes | Write overhead (Redis RTs) | Reader during write | Best for |
|---|---|---|---|---|---|---|
| 1 | Simple | ms, self-healing | Invalidated (lazy) | 1 RT | cache hit (stale ms) | Most systems |
| 2 | Lock stack pipelined | None | Works (write-through) | 2 RTs | cache hit (correct) | Zero stale + high read traffic |
| 3 | Lock stack naive | None | Works (write-through) | 5 RTs | cache hit (correct) | Zero stale, simpler implementation |
| 4 | CAS | None | Broken (DB fallback) | 2 RTs | DB round trip (wasted if MVCC) | Zero stale, no lock complexity, low writes |

### Does MVCC change the ranking?

With MVCC (plain SELECT in InnoDB REPEATABLE READ / READ COMMITTED), the CAS
reader's DB SELECT doesn't block behind the writer's row lock. This removes the
1/2(n_d + u) wait term from CAS reader cost.

**Per-reader latency comparison during a write:**

```
CAS reader (MVCC, no wait):      2(n_r + p) + (n_d + s)
                                 = 2n_r + 2p + n_d + s

Lock stack reader (blocked avg): 1/2(n_d + u + n_r + l) + (n_r + p)
                                 = 3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u
```

Difference (CAS - Lock stack):
= (2n_r + 2p + n_d + s) - (3/2 n_r + p + 1/2 l + 1/2 n_d + 1/2 u)
= 1/2 n_r + p - 1/2 l + 1/2 n_d + s - 1/2 u

CAS reader is faster when this is negative:
1/2 n_r + 1/2 n_d + s < 1/2 u  (dropping small p, l)
**n_r + n_d + 2s < u**

**Example** (illustrative — all concrete values below are assumptions; n_r, n_d,
s, u are hardware- and deployment-dependent, benchmark your system):

At same-DC (n_r ≈ 0.3ms, n_d ≈ 0.3ms, s ≈ 0.3ms, u ≈ 2ms):
0.3 + 0.3 + 0.6 = 1.2 < 2 — condition holds. CAS reader ~1.2ms vs lock stack
blocked reader ~1.6ms. In this example, ~0.4ms per-reader advantage for CAS.

**The exact gap depends entirely on your values of n_r, n_d, s, u.** The
condition n_r + n_d + 2s < u holds when DB UPDATE processing (u) dominates —
which it usually does when fsync is the bottleneck. The condition fails when:
- u is small (fast NVMe storage, relaxed flush settings like
  innodb_flush_log_at_trx_commit=2)
- n_r + n_d is large (cross-DC deployment)
- s is large (complex SELECT, cold buffer pool)

**Why this doesn't change the ranking — three reasons:**

**1. M8 covers many lock stack readers for free.**

The lock stack reader formula above models the blocked case (cache miss after
DEL, waiting for M9 write-through). But M8 (serve-if-exists) means readers
arriving before DEL cache or after Lua SET cache (but before DEL lock) get a
cache hit at n_r + p ≈ 0.3ms with zero blocking.

```
Lock stack reader timing windows during write:
  [SETNX → DEL cache]       cache exists → M8 hit: n_r + p     (~0.3ms)
  [DEL cache → Lua SET]     cache empty → blocked: ~1.6ms avg
  [Lua SET → DEL lock]      cache exists (new data) → M8 hit: n_r + p (~0.3ms)
```

The blocked window (DEL → Lua SET = n_d + u ≈ 2.3ms) is the majority of the
lock time, but the M8 windows are real. A reader arriving in an M8 window pays
0.3ms — much less than CAS's 1.2ms.

**2. After the write, lock stack cache is populated — CAS isn't.**

M9 (write-through inside lock) means the cache has new correct data before
unlock. Every reader after the write gets n_r + p. With CAS, the SET cache
hasn't happened yet (it's the last step). Readers between DB commit and SET
cache still see version mismatch → still fall through to DB. During heavy
writes, subsequent write INCRs keep pushing the version counter ahead — the
cache may never serve a hit.

The per-reader advantage during one write doesn't compound — CAS's
~0.4ms/reader savings is one-time per write, while the lock stack's post-write
cache benefits all future readers until the next write.

**3. CAS's faster trip was wasted — it returned redundant data.**

With MVCC, the CAS reader's DB SELECT returns the last committed version —
which is v(N), the same data the cache had. The writer's v(N+1) isn't committed
yet, so MVCC can't return it.

The CAS reader paid ~1.2ms to confirm what the cache already knew. The lock
stack reader paid ~1.6ms but got new correct data (v(N+1)) from cache after
unlock. The CAS trip contributed nothing; the lock stack trip gave the reader
fresh data.

```
CAS reader during write (MVCC):
  Version check → mismatch → DB SELECT → v(N) (same as cache) → wasted trip
  Total: ~1.2ms for redundant data

Lock stack reader during write (blocked):
  Wait for lock → GET cache → v(N+1) (new data from M9) → useful result
  Total: ~1.6ms for fresh data
```

**Conclusion:** MVCC makes CAS readers individually faster during the blocked
window (~0.4ms per reader at same-DC). But the lock stack's total system cost
remains lower: M8 serves some readers at ~0.3ms (faster than CAS), M9 populates
cache for all post-write readers (CAS doesn't), and CAS's faster trip returned
data the cache already had. The ranking is unchanged.

---

#### Current entry task: user management system
- The current scale is ok with the (M1+M10+M4) approach of DB+delete(invalidation on write) + version in key of :v<row_version>
- The roi is not worth the effort of all the moving parts to develop, the updates and changes for the users on a user_id level is only for nickname and avatar, so it is unlikely for them to trigger the stale state which is query cache before cache invalidate (after db update), needs the nickname and avatar to change for the SAME USER within a second for this to happen, unless it is a network or some other failure which is rare and this stale level is for a user so the roi is not worth it

---

## 15. Lock Stack — Slow Reader Cache-Populate Failure Modes

The lock stack (M2b+M3+M5+M7+M8+M9+M10) prevents stale reads **during** the
write via the lock, and prevents stale **cache overwrites** outside the lock via
the Lua version compare (M5). But the reader's cache-populate path — the code
that runs after a reader misses cache, reads DB, and writes the result back to
cache — has two failure modes where stale data can briefly appear in cache.

### Scenario 1: Slow reader writes stale data while writer is between DB UPDATE and write-through

```
Timeline:
1. Reader: GET cache → miss, no lock → DB SELECT → gets version N
2. Writer: acquires lock → DEL cache → DB UPDATE (N→N+1)
   [writer has committed N+1 to DB but not yet written to cache]
3. Slow reader: readerVersionSet(N) → cache is EMPTY (writer DELeted it)
   → version compare passes (nothing to compare against) → WRITES STALE N
4. Writer: write-through N+1 → Lua sees N+1 > N → overwrites → correct again
```

**The stale window:** between step 3 (reader writes N) and step 4 (writer
overwrites with N+1). Any reader hitting cache during this window sees stale N.
The window is very brief (one Redis round trip within the writer's own
execution), but it exists.

**Fix (implemented):** Add a lock check to the reader's cache-populate Lua
script. The reader's version-compare-and-SET atomically checks whether a lock
is held before writing. If locked, skip the write — the writer's write-through
will handle it.

```lua
-- readerVersionSetScript
-- KEYS[1] = cache key, KEYS[2] = lock key
-- ARGV[1] = new version, ARGV[2] = encoded value, ARGV[3] = TTL
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1  -- lock held, skip (writer will handle)
end
local current = redis.call('GET', KEYS[1])
if current then
  local sep = string.find(current, '\n')
  if sep then
    local ver = tonumber(string.sub(current, 1, sep - 1))
    if ver >= tonumber(ARGV[1]) then
      return 0  -- reject: current is same or newer
    end
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3]))
return 1
```

Because the Lua script is atomic in Redis (single-threaded execution), there is
no TOCTOU gap between the lock check and the SET. This eliminates scenario 1
entirely.

The writer's own write-through uses the original `versionSetScript` (no lock
check) because the writer holds the lock — checking it would reject its own
write.

### Scenario 2: Writer's write-through fails, slow reader writes stale data after lock release

```
Timeline:
1. Reader: GET cache → miss, no lock → DB SELECT → gets version N
2. Writer: lock → DEL cache → DB UPDATE (N→N+1) → write-through FAILS
   (Redis error on the Lua EVALSHA)
3. Writer: releases lock (cache is still empty — DEL ran, write-through failed)
4. Slow reader: readerVersionSet(N) → no lock (released) → cache EMPTY
   → writes stale N
5. Cache has stale N. DB has N+1.
```

This is harder than scenario 1 because the lock has been released by the time
the slow reader writes. The scenario 1 fix (lock check) does not help here.

**Self-healing:** the next reader that arrives does: GET cache → hits stale N →
serves it (stale). But another reader that misses (or after TTL): DB SELECT →
N+1 → readerVersionSet(N+1) → N+1 > N → overwrites. The staleness window is
from step 4 until the next reader that repopulates from DB.

**STATUS: open for discussion — several possible fixes, each with tradeoffs.**

#### Option A: Accept it

The write-through failure requires Redis to fail on a Lua EVALSHA, which is
already a degraded state. The staleness self-corrects on the next DB-sourced
cache populate. Duration: until next reader populates from DB (could be
immediate if no reader has the stale version cached in a session hint, or up to
CacheTTL if readers keep hitting the stale entry).

#### Option B: Shortened lock TTL on write-through failure

Instead of releasing the lock when write-through fails, the writer shortens the
lock's TTL to `ReadOpTimeout` (the total budget for one read operation: cache
GET + DB SELECT + cache populate) and skips the explicit release. The lock
expires naturally after ReadOpTimeout.

During that window, the scenario 1 fix (lock check in readerVersionSet) rejects
any slow reader's stale write. After the lock expires, cache is still empty,
next reader populates from DB with N+1.

```
Writer: lock → DEL → DB UPDATE (N→N+1) → write-through FAILS
  → PEXPIRE lock <ReadOpTimeout> → exit (don't DEL lock)
Slow reader: readerVersionSet(N) → lock still held → SKIP
After ReadOpTimeout: lock expires → cache empty → next reader → DB → N+1 → correct
```

The ReadOpTimeout is the right bound because the slow reader's entire remaining
path (DB SELECT + readerVersionSet) must complete within it. Setting it to just
the Redis IO timeout is too narrow — the reader might still be in its DB SELECT.

**Tradeoff:** readers are blocked for up to ReadOpTimeout on write-through
failure (rare). No stale data.

#### Option C: Tombstone instead of DEL

Instead of DELeting the cache in the write path (step 3 of the lock stack), the
writer writes a **tombstone**: a cache entry with the current version but no
data. Format: `<version>\n` (version prefix, newline, empty data).

The tombstone acts as a version barrier:
- Slow reader tries readerVersionSet(N) → Lua sees tombstone version N ≥ N →
  **rejected**. Stale write prevented.
- Writer's write-through (N+1): N+1 > N → overwrites tombstone with real data.
- If write-through fails: tombstone persists. Next reader: GET → tombstone →
  decoded as miss (empty data) → DB → N+1 → readerVersionSet(N+1) → N+1 > N →
  overwrites tombstone. Self-heals on next read.

```
Writer: lock → write tombstone {version=N, data=""} → DB UPDATE (N→N+1)
  → write-through {version=N+1, data=real} → overwrites tombstone → release lock

If write-through fails:
  → tombstone stays → slow reader's N rejected by Lua (N ≥ N)
  → next reader: tombstone = miss → DB → N+1 → overwrites tombstone
```

**Tombstone TTL must be LockTTL, not CacheTTL.** If the writer crashes before
the DB UPDATE, DB still has version N. Tombstone has version N. Readers: DB →
N → readerVersionSet(N) → Lua sees tombstone N ≥ N → **rejected**. Cache
cannot be repopulated until tombstone expires. With CacheTTL (e.g. 1h), every
read hits DB for an hour. With LockTTL (e.g. 2s), tombstone expires alongside
the lock, and normal cache repopulation resumes immediately.

In the non-crash path, the writer's write-through overwrites the tombstone with
real data + CacheTTL, so the short tombstone TTL costs nothing.

**Version source for the tombstone:** extracted from the saved cache value
(step 2). If cache was empty (no saved value), the writer has no version for
the tombstone without an extra DB read (`SELECT row_version WHERE id = ?`).

**Read path change:** `decodeValue` must treat empty data as a miss. This is
already natural — `data = []byte(raw[idx+1:])`, if `len(data) == 0` return
`ok=false`. M8 (serve-if-exists) sees a tombstone, decodes as miss, proceeds
to lock check — correct.

**Tradeoff:** no reader blocking, no timing dependency. But: Lua scripts and
decode logic gain tombstone awareness, and the "cache was empty" case needs
either an extra DB read or a fallback to option B.

#### Option D: Reader lock-wait evidence (speculative, needs further discussion)

Idea: a reader should only be allowed to overwrite cache after a DB read if it
can prove it arrived **after** the writer. Evidence: the reader observed a lock
being held (it went through the wait-poll loop), then the lock was released,
then it read DB, then it writes to cache. This sequence proves its DB read
happened after the writer's DB commit — so its data is at least as fresh as
the writer's.

A reader that never saw a lock (went straight to DB from a cold miss) cannot
make this claim — its DB read might have happened before the writer's commit.

**Implementation sketch:** the read path sets a flag `sawLock = true` when the
readCheck Lua returns `__LOCKED__`. After the DB read, the cache-populate path
checks this flag:
- `sawLock = true` → always write (data is post-writer, guaranteed fresh)
- `sawLock = false` → use version compare (might be stale)

**Problems and nuances (needs further discussion):**
- Multiple concurrent writers: which writer did the reader wait for? If reader
  waited for Writer A (v5→v6) but Writer B (v6→v7) started after, the reader's
  DB read might return v6 which is already stale relative to Writer B's v7. The
  "I saw a lock" evidence proves the reader is post-A, but not post-B.
- Lock identity: the reader would need to track which specific writer it waited
  for (by writer_id?), and the cache-populate would need to know which writer
  last held the lock. This adds complexity to both the Lua scripts and the Go
  code.
- TOCTOU: the lock could be released by one writer and acquired by another
  between the reader's lock-check and its cache-populate. The reader thinks it
  waited for the same writer, but the lock changed hands.
- Does this actually fix scenario 2? If the writer's write-through failed and
  the lock was released, a reader that saw the lock (waited, then saw it release)
  would have `sawLock = true` → writes unconditionally → but its DB read might
  have happened before the DB UPDATE committed (if using MVCC/READ COMMITTED,
  the SELECT might see the old version). So the evidence isn't as strong as it
  seems.

**STATUS: interesting direction but has significant nuances with multiple
writers and lock identity. Needs further discussion before implementation.**