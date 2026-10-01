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
2. **CAS (version counter)** — INCR version → DB update → cache data write
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

### CAS (version counter)

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
| CAS | Orphaned version → DB fallback until next write | Orphaned INCR, version points to nothing |
| Lock + delete-first | Lock expires, cache empty, readers hit DB. Self-heals. | Cache already deleted, DB unchanged. Correct. |

**Lock with delete-first ordering** has the safest failure mode: any failure
after the DEL leaves cache empty, which is always correct (readers go to DB).
One exception found later: a slow reader can refill that empty slot with old
data (§17) — closed by the reader lease (M13), which makes "empty" safe again.

---

## 5. The CAS Trap — "Lock-Free" That Isn't

CAS appears lock-free and therefore "better" by C++ intuition. But:

### The DB row lock is the real serialization point

Writers still queue on MySQL's row lock regardless of what Redis does.
CAS isn't lock-free — it just moved the lock from Redis to MySQL's row
lock. Writers pay DB contention either way, but CAS adds Redis INCR on top and
gets a worse cache in return.

### CAS readers compete with writers at the DB level

Under write pressure, CAS readers see version mismatch → fall through to DB →
join the DB row lock queue alongside writers. Lock readers wait in Redis
(~0.1ms) then get a cache hit. The lock *shields* readers from DB contention;
CAS *exposes* them to it.

With MVCC (plain fresh SELECT, the InnoDB default), CAS readers don't queue on
the row lock — but each is still a DB query, so they add DB load and compete for
connections and I/O with the writers. The exposure is to DB load rather than to
the row lock queue; the conclusion is the same.

### Reader latency under write pressure

| DB write pressure | DB→delete reader | CAS reader | Lock reader |
|---|---|---|---|
| Low (1 writer) | ~1-5ms (DB read, light) | ~1-5ms (DB read, light) | ~2-6ms (lock wait + cache) |
| Medium (10 writers) | ~5-20ms (DB contended) | ~5-20ms+ (same or worse) | ~2-6ms (bounded) |
| High (100 writers) | ~20-100ms+ (heavily contended) | ~20-100ms+ (same or worse) | ~2-6ms (still bounded) |

Lock reader latency is bounded because readers wait for one lock holder in
Redis, not in the DB queue. CAS reader latency degrades because all readers
join the DB row lock queue alongside writers (or, with MVCC, add their queries
to an already loaded DB).

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

That's why CAS seems "alone" — it's largely redundant once write-path
mechanisms exist. It's not that it can't combine; its remaining appeal on its
own is that it needs no lock (though it has its own complexity — see §11 M6).

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
| `BLPOP` on a release signal | Wakes only one waiter per push; holds a pooled connection per blocked waiter | Medium |
| Pub/sub wake-up (one subscriber per instance) | ~1 RT after unlock; at-most-once, needs fallback poll | Medium-high |
| Lua script (check lock + GET in one call) | One round trip, atomic | Medium |
| Don't check lock, just miss cache, hit DB | Defeats the lock's purpose | None |

Lua script is the best balance — one round trip, atomic check. It is called
only on a miss (plain GET first on the hit path), and retried with capped
exponential backoff (0.5 → 1 → 2 → 4ms + jitter). With M13 it also grants the
lease — see §17 "Lease design concepts" points 6–7 for the reasoning and the
final script.

Pre-lease sketch (the final readCheck, which also grants the lease, is in §17):

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
| Reader waits for | DB row lock (locking read; under MVCC a plain SELECT doesn't wait) | Redis lock release |
| Wait duration | ~1-5ms (DB write time; ~0 under MVCC) | ~1-5ms (same DB write time) |
| After wait, reader does | DB SELECT (~1-5ms round trip) | Redis GET (~0.1-0.5ms) |
| Total reader latency | ~2-10ms (wait + DB read; ~1-5ms under MVCC, old value) | ~1.5-5.5ms (wait + cache read) |
| DB load from readers | Every reader hits DB | Zero — readers get cache |

The wait time is the same either way — bounded by the DB write. The Redis lock
is better for readers under locking reads or load (see MVCC note below): same
wait, then cache hit instead of DB round trip. DB is protected.

**In the MVCC scenario (plain, fresh SELECT — the InnoDB default):** the
no-lock reader does *not* wait on the row lock. MVCC returns the last committed
value immediately, so its cost is one DB round trip (~1-5ms), with no wait.
At low load that can be faster than waiting behind the Redis lock. But:
- It returns the **old** committed value — the same data the cache already had
  — so the trip bought nothing new. The lock stack reader gets the **new** value
  from cache after the unlock.
- Every such reader is a DB query, so DB load grows with readers, and DB latency
  (and writers' latency) rises with it. The lock stack reader adds zero DB load
  and its cost stays flat as readers scale.

So "same wait, better outcome" holds as stated for locking reads and under load;
with MVCC at low load the no-lock reader can be quicker, and the lock stack's
advantage is the DB load and fresh data rather than per-reader latency.

**Poll slack (minor):** a blocked lock stack reader notices the unlock on its
next poll, so it also pays a small slack S (≈ ½ the poll interval in effect;
small with capped exponential backoff 0.5 → 1 → 2 → 4ms).

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
extra cost is the Redis ops themselves (~0.5-1ms, ~5-20% on top of ~5ms DB
write).

With per-user Redis locks (`{user:<id>}:lock`), writers on different users don't
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
  version before writing) — except a slow reader writing into the empty slot
  (§17), closed by M13

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
| Version-in-value + Lua | Yes, atomically — except into an empty slot (§17; M13 closes it) | Lua rejects write if cache version >= writer's version | None on reads, Lua on writes |
| Plain key, no version | **No — vulnerable** | Nothing prevents the overwrite | None |
| CAS | Yes | Version comparison | On every read |

### What each mechanism covers

The lock prevents stale reads **during** the write. Versioning prevents stale
**writes to cache** that happen **outside** the lock window:

| | What goes wrong | Lock fixes? | Versioning fixes? |
|---|---|---|---|
| Stale read (during write) | Reader sees old cache while DB updates | Yes (blocked) | Not needed |
| Stale cache write (slow reader) | Old data overwrites new in cache | No (reader was outside any lock) | Yes |

One Lua comparison on cache population (cold path only) is sufficient (except a slow reader writing into the empty slot, §17 — closed by M13). No
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
DB. Correct. The DEL is the safety net for the SET (except a slow reader writing into the empty slot, §17 — closed by M13).

---

## 9. Redis Dependency Analysis

Every approach uses Redis for reads (that's the cache). When Redis is down,
reads go to DB regardless.

The difference is writes:

| Approach | Write path touches Redis? | Redis down → writes? | Redis down → reads? |
|---|---|---|---|
| DB→delete | After DB (DEL) | **Writes succeed.** DEL fails, nobody reads stale cache (Redis is down). | All reads hit DB. Slower but correct. |
| CAS | Before DB (INCR) | **Writes blocked** (or skip INCR → version breaks) | All reads hit DB. |
| Lock stack | Before DB (SETNX) | **Writes blocked** (can't acquire lock) | All reads hit DB. |

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
Redis returns: an empty key is repopulated by the next reader; a key whose DEL
was skipped still holds the old value and is served stale until its CacheTTL or
the next write. You temporarily lose the zero-staleness guarantee but don't
lose availability.

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

A per-key Redis lock (`SETNX {user:<id>}:lock`) that the writer acquires before
touching DB or cache, and releases (`DEL {user:<id>}:lock`) after finishing.
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

### M6: CAS — version counter (INCR → DB → SET)

*Naming:* earlier drafts called M6 "CAS/lease". It is not a lease — it is a
version counter with compare-and-set — and is unrelated to M13 (reader lease).

A version counter in Redis (`user:<id>:version`) is incremented before the DB
write. After the DB write, the cache data is updated with the new version. On
every read, the reader compares the cache data's version against the version
counter — if they don't match, the reader falls through to DB.

```
Write: INCR user:<id>:version → v6 → DB UPDATE → SET user:<id> {v6, data}
Read:  MGET user:<id>:version user:<id> → [v6, {v5, data}]
       → v5 != v6 → mismatch → DB fallback
```

This is version-in-value (M5) with read-path enforcement added. M5 only
compares on cache writes (cold path). CAS compares on every read (hot path) —
more overhead, but catches staleness at read time, not just write time. The
only single mechanism that prevents both P1 (stale during write) and P2
(slow-reader overwrite) without a lock.

**What M6 needs to actually be correct** (implicit in "versioned", spelled out
here):

```
Write: v = INCR user:<id>:version
       UPDATE users SET ..., row_version = v WHERE id = ? AND row_version < v
         → 0 rows affected? a newer write already committed → lost update (retry or drop)
       SET user:<id> {v, data}
Read:  MGET user:<id>:version user:<id>     ← ONE atomic command, not two GETs
       match? return. mismatch? → DB.
Reader fill (on miss/mismatch):
       DB SELECT → {data, row_version r}
       cache only if r == counter (Lua: compare + SET), else don't cache
```

- **Conditional DB update** — INCR order is not DB commit order (InnoDB's row
  lock decides the latter). Without `row_version < v`, W1 (v6) can commit after
  W2 (v7): DB ends with W1's data while the cache/counter say v7 → stale.
- **MGET** — two separate GETs can straddle a write (read counter before the
  INCR, cache after the commit). One MGET is atomic, so the pair is a snapshot.
- **Reader fill with DB row_version** — filling with the *counter* value the
  reader saw is unsafe: counter = 6 (INCRed, not committed), MVCC returns v5
  data, reader stores `{6, data5}` → matches → stale. Using the row's own
  `row_version` means pre-commit reads (r=5, counter=6) simply don't cache.
- **Counter must not go backwards** (rare) — see §13 M6 scenarios.

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
is empty (DEL already ran), readers go to DB. Correct — except a slow reader
filling that empty slot with old data (§17), closed by M13.

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
  With the lock stack + lease, the lease already gives 1 DB read per cold miss
  across instances; M12 wraps the whole miss path (readCheck polling + DB read)
  so only one goroutine per key per instance polls Redis.
- **Application-level, in-process only** — each application instance coalesces
  independently. With K instances, up to K DB reads still fire (1 per instance).
  This is not a distributed deduplication — it doesn't coordinate across pods.
- **Shared error propagation** — if the one executing call fails, all N-1 waiters
  also receive the error. This is usually fine (they'd all have failed individually
  anyway), but means a transient error affects N callers instead of potentially
  only some.
- **No write-path impact** — singleflight is a read-path optimization. It doesn't
  change how writes work.

### M13: Reader lease (lease token on cache miss) — CHOSEN

A reader that misses cache may only populate it if it holds a **lease**: a
random token it placed into the empty cache slot *before* reading the DB. The
cache write succeeds only if the slot still holds that exact token. The writer's
existing DEL-first step (M7) wipes any outstanding lease, so a reader whose DB
read may predate the write can never land its result.

This is the "lease" from Facebook's memcache paper (NSDI '13) — the real
meaning of the word, which M6's earlier name "CAS/lease" borrowed loosely. The rule it enforces:

> **Readers never write into an empty slot — only into their own lease.**

```
Reader, on miss:
  Lua: slot empty?          → SET cache "L:<token>" PX <LeaseTTL> → GRANTED
       lock held?           → LOCKED  (wait, retry)
       someone else's lease → LEASED  (wait briefly, retry)
  DB SELECT
  Lua: cache == "L:<token>" → SET cache <data> EX <CacheTTL>
       anything else        → drop it (serve the DB result, don't cache it)

Writer (unchanged): lock → DEL cache (voids every lease) → DB UPDATE → SET cache → unlock
```

**The token** is an identity check, not a secret. It must never repeat for the
same key within a lease's lifetime, or a slow reader could match someone else's
lease. Chosen: a random instance ID generated once at startup + a per-process
atomic counter (~10ns per token, unique across instances and restarts because a
restart gets a new ID). 128 random bits per token (`crypto/rand`, ~0.1–0.5µs)
also works. A Redis `INCR` counter would not: it can reset on a Redis restart
without persistence and hand out the same numbers again. Tokens are generated
only on a miss.

**Why it works where versions don't:** the question a reader's cache write must
answer is *"did my DB read happen after the latest write started?"* A version
number only answers *"is my data newer than what's cached?"*, and has nothing to
compare against once the cache is empty. The lease answers the real question
directly: if the slot still holds my token, no writer has touched this key
since before my DB read (every writer DELs first).

**What it is and isn't:**

- **Fixes the slow-reader cache-populate gap** (§17 scenarios 1 and 2) —
  including the write-into-empty-slot case that M5 cannot catch.
- **Replaces M5 on the reader path.** Readers no longer need a version compare.
  M5 stays on the writer's write-through as extra protection.
- **Fixes P3 on cold misses across instances** — only the lease holder reads
  DB; other readers see `LEASED` and wait. Unlike M12, this coordinates across
  pods (the lease lives in Redis). M12 can still sit in front to cut Redis
  polling within one instance.
- **Design rules and what each prevents** (one key/one DEL, fresh read, no
  lease watchdog, GET-first, backoff, negative caching, eviction, hash tags):
  §17 "Lease design concepts".
- **Does not depend on timing** — stall duration, lock TTL, ≥ vs > no longer
  matter.
- **One key, one DEL.** The lease lives in the same key as the data; the slot
  holds either data or a lease, never both. The writer's single DEL clears
  whichever is there — no extra command. (A separate lease key would need two
  DELs and a two-key check.)
- **Requires a fresh DB read after the grant** — autocommit, or a transaction
  begun after GRANTED. A `REPEATABLE READ` snapshot opened before the grant
  could predate a committed write, and the lease would then protect stale data.
- **Costs** no extra round trip compared with lock stack (M5): the
  grant is one more SET inside the readCheck Lua that already ran on a miss,
  and `leaseSetScript` replaces the reader's version-compare Lua one-for-one.
  (Compared with plain cache-aside GET → DB → SET, the miss path has one extra
  Lua round trip — the readCheck.) Zero cost on a cache hit.

### Summary table

| Mechanism | Fixes | Introduces | Read-path cost | Write-path cost | Failure behavior | Staleness impact |
|---|---|---|---|---|---|---|
| M1: DB→delete (no lock) | — | P1 (small window), P3 (small stampede) | None — 1 GET | 1 DEL | DEL idempotent, safe | ms stale (P1) |
| M2a: Lock, manual unlock | P1, P7 | P6 (starvation on crash) | Lock check | SETNX + DEL lock | Held forever on crash | No stale during write |
| M2b: Lock with TTL | P1, P7, P6 | P8 (expires mid-write) | Lock check | SETNX EX + DEL lock | Expires — safe if TTL > write | No stale during write |
| M3: Watchdog + TTL | P6, P8 | Complexity | None extra | Periodic extends | Dies → TTL expires → safe | Eliminates P8 risk |
| M4: Version-in-key | P2 | Need version to form key | None if version known | None extra | Old keys linger, harmless | Prevents long stale from P2 |
| M5: Version-in-value + Lua | P2 (not into an empty slot, §17) | Lua complexity | None — plain GET | Lua EVAL on population | Atomic, safe | Prevents long stale from P2, except empty-slot fill (M13 closes it) |
| M6: CAS — version counter (INCR→DB→SET) | P1, P2 | P4, P5 | Compare EVERY read (1 MGET) | INCR + SET (+ conditional DB update) | Orphaned version (until next write) | No stale; P5 = fallback until next write |
| M7: Delete-first (inside lock) | Safe failure ordering | — | None | None extra | Cache empty on failure | Prevents stale-on-failure |
| M8: Serve-if-exists | Unnecessary blocking | — | None extra | None | — | Serves pre-write data (correct) |
| M9: Write-through inside lock | P3 (stampede) | Lock held ~0.2ms longer | None | 1 extra SET | SET fails → cache empty | Fresh cache immediately |
| M10: Invalidate-on-write | P7 (DEL idempotent) | P1 (small window), P3 | None | 1 DEL | Safe | Lazy — next reader repopulates |
| M11: Write-through (no lock) | P3 | P7 (concurrent race) | None | 1 SET | Wrong order = stale | Eager — but P7 risk |
| M12: Singleflight | P3 (mitigates) | Per-instance only; shared errors | None | None | Shared error propagation | Reduces DB load, not staleness |
| M13: Reader lease | P2 incl. write-into-empty (§17), P3 on cold miss (cross-instance) | Lease grant on miss | None on hit; no extra RT on miss vs lock stack (M5) (grant is inside the readCheck Lua) | None (DEL-first already voids leases) | Lease expires → holder's write dropped, next reader re-leases | Closes the last stale path in the lock stack |

---

## 11b. Flow Diagrams — Write Path + Reader Behavior

Visual flows for each approach: what the writer does, what happens at each step
if it succeeds or fails, and how readers arriving during the write are handled.

### CAS (M6) — Write + Reader Flow

```
WRITER                                          READER (during write)
──────                                          ──────────────────────
    │                                               │
    ▼                                               ▼
┌─────────────┐                              ┌──────────────┐
│ INCR version│                              │ MGET version  │
│ → v(N+1)    │                              │  + cache (1RT)│
└──────┬──────┘                              └──────┬───────┘
       │                                            │
       │ Redis has version=v(N+1)                   ▼
       │ DB still has v(N)                   ┌──────────────┐
       │ Cache still has v(N)                │ version match?│
       │                                     └──┬───────┬───┘
       ▼                                        │       │
┌──────────────┐                            YES │       │ NO (v(N+1) ≠ v(N))
│ DB UPDATE    │                                │       │
│ WHERE ver< v │                                ▼       ▼
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

### Lock Stack + Lease (naive) — Write + Reader Flow

```
WRITER                                        READER (during write)
──────                                        ──────────────────────
    │                                             │
    ▼                                             ▼
┌──────────────┐                           ┌──────────────┐
│ SETNX lock   │                           │ GET cache    │
│ (n_r + p)    │                           │ (plain GET)  │
└──────┬───────┘                           └──────┬───────┘
       │                                          │
       │ ← FAIL (lock held)?                      ▼
       │    → wait/retry or                ┌──────────────┐
       │      degrade to Simple            │ real data?   │
       │                                   │ ("L:" = miss)│
       ▼ ACQUIRED                          └──┬────────┬──┘
┌──────────────┐                          YES │        │ NO (empty or lease)
│ GET cache    │                              ▼        ▼
│ (save old    │                        ┌─────────┐ ┌──────────────┐
│  value)      │                        │ Serve   │ │ readCheck Lua│
│ (n_r + p)    │                        │ cache   │ │ (miss only)  │
└──────┬───────┘                        │ (M8) ✓  │ └──────┬───────┘
       │                                │ correct │        │
       ▼                                │ old data│        ▼
┌──────────────┐                        └─────────┘ ┌─────────────────┐
│ DEL cache    │                                    │ HIT / LOCKED /  │
│ (voids any   │                                    │ LEASED / GRANTED│
│  lease)      │                                    └──┬──────────┬───┘
│ (n_r + p)    │                        LOCKED/LEASED  │          │ GRANTED
└──────┬───────┘                                       │          │ (no lock, slot
       │ cache now EMPTY                               ▼          │  was empty)
       │ (readers after this: blocked)          ┌────────────┐    ▼
       │                                        │ WAIT: poll │ ┌──────────────┐
       ▼                                        │ w/ capped  │ │ fresh DB     │
┌──────────────┐                                │ exp backoff│ │ SELECT       │
│ DB UPDATE    │                                └─────┬──────┘ │ → leaseSet:  │
│ (n_d + u)    │                                      │        │ still my     │
└──────┬───────┘                                      │        │ lease? SET   │
       │                                              │        │ : drop       │
       │ ← FAIL?                                      │        └──────────────┘
       │    ▼                                         │
       │  try restore: SET cache {old value}          │
       │  → unlock                                    │
       │  → readers: cache hit (restored) ✓           │
       │  or if restore fails:                        │
       │  → readers: miss → one lease holder → DB ✓   │
       │                                              │
       ▼ SUCCESS                                      │
┌──────────────┐                                      │
│ Lua SET cache│                                      │
│ {v(N+1),data}│                                      │
│ (n_r + l)    │                                      │
└──────┬───────┘                                      │
       │                                              │
       │ ← FAIL?                                      │
       │    → cache empty (DEL already ran)           │
       │    → unlock                                  │
       │    → readers: one lease holder → DB,         │
       │      rest wait; slow reader's old fill       │
       │      rejected (its lease was voided) ✓       │
       │                                              │
       ▼ SUCCESS                                      │
┌──────────────┐                                      │
│ DEL lock     │                                      │
│ (n_r + p)    │◄─────────────────────────────────────┘
└──────┬───────┘          readers notice on next poll (+ S)
       │                  → readCheck → HIT (M9) ✓
       ▼
  DONE. cache=v(N+1), DB=v(N+1), lock=free

DURING WRITE:
  cache exists (before DEL) → M8: serve immediately ✓
  cache empty + locked       → readCheck says LOCKED → poll until unlock
  after unlock               → cache hit (M9 populated) ✓
  → cache WORKS throughout (serves or blocks-then-serves)

COLD MISS (no write):
  first reader → GRANTED → fresh DB read → leaseSet → cached ✓
  other readers → LEASED → poll → HIT (one DB read in total) ✓
  any writer that locks meanwhile DELs the lease → the fill is dropped ✓
```

### Lock Stack + Lease (pipelined) — Write + Reader Flow


```
WRITER                                        READER (same as naive)
──────                                        ──────────────────────
    │                                         (reader flow identical
    ▼                                          to naive — M8, readCheck,
┌───────────────────────┐                      lease, M9 unchanged)
│ PRE-DB LUA SCRIPT     │
│ (single round trip)   │
│                       │
│  SETNX lock           │
│  → FAIL? degrade      │
│  GET cache (save old) │
│  DEL cache (voids     │
│    any lease)         │
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
              CAS                          Lock Stack + Lease
              ───                          ──────────────────
Reader        MGET version + cache         GET cache
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
                wasted trip             (avg 1/2 write + S)
                                           │
                                           ▼
                                      readCheck → HIT (M9)
                                      (n_r + l, new data)
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
| M6 (CAS) | **Fixed** ‡ | **Fixed** | **Worst** | **Broken** | Until next write | No | Fixed (conditional DB update) | N/A | **No stale; P5 fallback** | Yes |
| M11 (write-through, no lock) | **Fixed** | Vulnerable | **Fixed** | Works | No | No | **Vulnerable** | N/A | **Long possible (P7)** | No |
| M2a+M7+M10 (lock manual) | **Fixed** | Vulnerable | Stampede | Works | No | **Vulnerable** | **Fixed** | N/A | **P2 + P6 risk** | Yes |
| M2b+M7+M10 (lock TTL) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Vulnerable** | **P2 + P8 risk** | Yes |
| M2b+M3+M7+M10 (+ watchdog) | **Fixed** | Vulnerable | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **P2 only** | Yes |
| M2b+M3+M5+M7+M10 (+ ver-in-val) | **Fixed** | **Fixed** † | Stampede | Works | No | **Fixed** | **Fixed** | **Fixed** | **Stale possible** † (no write-through: gap after every write) | Yes |
| M2b+M3+M5+M7+M8+M10 (+ serve-if-exists) | **Fixed** | **Fixed** † | Reduced | Works | No | **Fixed** | **Fixed** | **Fixed** | **Stale possible** † (no write-through: gap after every write) | Yes |
| M2b+M3+M5+M7+M8+M9+M10 (lock stack (M5)) | **Fixed** | **Fixed** † | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale except † gap** (failure cases) | Yes (degrades to M1+M10) |
| **M2b+M3+M7+M8+M9+M10+M13 (lock stack + lease) — CHOSEN** | **Fixed** | **Fixed** | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale** | Yes (degrades to M1+M10) |
| | | | | | | | | | | |
| *+M12 (singleflight) variants:* | | | | | | | | | | |
| M1+M10+M4+M12 (Simple+SF) | Small, blast limited | **Fixed** | **Mitigated** | Works when writes rare | No | No | Safe | N/A | **ms only (self-healing)** | No |
| M6+M12 (CAS+SF) | **Fixed** ‡ | **Fixed** | **Mitigated** | **Broken** | Until next write | No | Fixed (conditional DB update) | N/A | **No stale; P5 fallback** | Yes |
| Lock stack (M5)+M12 | **Fixed** | **Fixed** † | **Fixed** | Works | No | **Fixed** | **Fixed** | **Fixed** | **No stale except † gap** (failure cases) | Yes (degrades to M1+M10) |

† M5 alone leaves one gap: a slow reader writing into the **empty** slot left
by DEL-first when the writer's write-through fails or the writer dies after the
DB commit — M5 has no version to compare against. See §17. M13 (reader lease)
closes it, which is why the chosen stack swaps M5 on the reader path for M13.
Without write-through (M9) the gap is wider: every write leaves the slot empty
after unlock, so a slow reader can fill it with old data even when nothing fails.

‡ CAS P1 is Fixed only if the reader fetches counter + cache with one `MGET`.
With two separate GETs there is a small window — see §13 M6 "Read straddling a
write".

### Staleness guarantee summary

| Combination | Worst-case stale | Duration | Self-healing? | Redis down? |
|---|---|---|---|---|
| M1+M10 (DB→delete) | P1 + P2 | ms + until TTL | P1: yes. P2: at TTL. | Writes fine. No cache. |
| M1+M10+M4 (+ version-in-key) | P1 only | ms | Yes | Writes fine. No cache. |
| M6 (CAS) | None. P5 = fallback. | P5: until next write | Yes | Writes blocked. |
| Lock stack (M5) | Slow reader into empty slot (§17) | Until next DB-sourced populate or CacheTTL | Eventually | Degrades to M1+M10 |
| **Lock stack + lease — chosen** | **None** | **N/A** | **N/A** | Degrades to M1+M10 |

### Read/write path costs

| Combination | Read (no write) | Read (during write, cache exists) | Read (during write, cache miss) | Write path |
|---|---|---|---|---|
| M1+M10+M4 (delete + version-in-key) | 1 GET | 1 GET (ms stale possible) | 1 GET miss → DB ~1-5ms | DB + 1 DEL |
| M6 (CAS) | 1 MGET + compare | 1 MGET + compare → DB ~1-5ms | 1 MGET + compare → DB ~1-5ms | INCR + conditional DB UPDATE + SET |
| M11 (write-through, no lock) | 1 GET | 1 GET (P7 stale possible) | 1 GET miss → DB | DB + 1 SET |
| Lock stack (M5, invalidate only) | 1 GET | 1 GET → **cache hit** | 1 Lua → block → DB + Lua SET | SETNX + DEL + DB + DEL lock |
| Lock stack (M5, + write-through) | 1 GET | 1 GET → **cache hit** | 1 Lua → block → **cache hit** | SETNX + DEL + DB + Lua SET + DEL lock |
| **Lock stack + lease (chosen)** | 1 GET | 1 GET → **cache hit** | 1 GET → readCheck Lua → poll → **cache hit** | SETNX + DEL + DB + Lua SET + DEL lock |
| Lock stack + lease, cold miss (no write) | — | — | holder: GET → readCheck (grant) → DB → leaseSet; others: wait → **cache hit** (1 DB read total) | — |
| | | | | |
| *+M12 (singleflight) variants:* | | | | |
| Simple+M12 | 1 GET | 1 GET (ms stale possible) | 1 GET miss → **1 shared DB** per instance ~1-5ms | DB + 1 DEL |
| CAS+M12 | 1 MGET + compare | 1 MGET + compare → **1 shared DB** per instance | 1 MGET + compare → **1 shared DB** per instance | INCR + conditional DB UPDATE + SET |
| Lock stack (M5)+M12 | 1 GET | 1 GET → **cache hit** | 1 Lua → block → **cache hit** (M12 as safety net on fallback) | SETNX + DEL + DB + Lua SET + DEL lock |

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

### M6 (CAS)

**Concurrent writers — no stale but lost updates:**
```
Writer A: INCR version → v6. DB UPDATE WHERE version < v6. SET cache.
Writer B: INCR version → v7. DB UPDATE WHERE version < v7. SET cache.
If A's SET arrives after B's SET: cache has A's older data.
BUT: reader checks version counter (v7) vs cache version (v6) → mismatch → DB.
Result: no stale data served. But A's DB update may also be silently rejected
        by CAS guard → lost update, needs retry logic.
```

**Concurrent writers — why the conditional DB update is required:**
```
INCR order is not commit order. InnoDB's row lock decides who commits first.

Writer A: INCR → v6          Writer B: INCR → v7
Writer B: DB UPDATE (data=B) commits first
Writer A: DB UPDATE (data=A) commits second

Without `WHERE row_version < v`:
  DB = A. Then B's SET {v7, B} lands last → cache {v7,B}, counter v7 → MATCH
  → serves B while DB has A. STALE until next write.
  (If A's SET lands last instead: cache {v6,A} → mismatch → DB → A. Correct by luck.)

With `WHERE row_version < v`:
  A's UPDATE (v6) finds row_version = 7 → 0 rows → rejected. DB = B.
  Cache {v7,B} matches DB. CORRECT. A's write is lost → retry or drop.
```

**Slow reader overwrite — FIXED:**
```
1. Reader A: MGET → counter v5, cache miss → DB SELECT → {data5, row_version 5} → hangs
2. Writer: INCR→v6, conditional DB UPDATE (row_version 6), SET cache {v6, data6}
3. Reader A: wakes up, tries to fill with {data5, row_version 5}
   Fill rule: cache only if row_version == counter → 5 != 6 → not cached.
   Cache keeps {v6, data6}. CORRECT.
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

**Reader fill with the counter value — STALE; with DB row_version — FIXED:**
```
Who refills the cache after TTL expiry/eviction? Writers only → cold key stays
uncached until its next write (could be days). So readers must fill — carefully.

Unsafe (tag with the counter the reader saw):
1. Writer: INCR → v6. DB UPDATE in flight (not committed).
2. Reader: counter = v6, cache miss → DB SELECT → MVCC returns v5 data
3. Reader: SET {v6, data5}              ← tagged v6, holds v5 data
4. Writer: dies after commit (or its SET {v6,data6} lands before step 3)
Result: cache {v6, data5}, counter v6 → MATCH → serves v5 data. STALE.

Safe (tag with the row's own row_version, cache only if it equals the counter):
2. Reader: counter = v6 → DB SELECT → {data5, row_version 5}
3. Reader: 5 != 6 → don't cache, just return data5.
   After commit: DB SELECT → {data6, row_version 6} → 6 == 6 → Lua SET {v6,data6}.
Result: CORRECT. Needs the row_version column from the conditional update.
```

**Read straddling a write (two separate GETs) — small window, fixed by MGET:**
```
1. Reader: GET counter → v5                    (before the INCR)
2. Writer: INCR → v6 → DB UPDATE (v5→v6) commits
3. Reader: GET cache → {v5, data5} → v5 == v5 → returns data5
Result: at the moment of return, DB (source of truth) holds v6.
```
Two views of whether this is stale:
- **Strict (matches DB at return time):** yes, stale. Small window — the reader
  must stall between its two GETs for the writer's whole INCR + DB commit.
- **Linearizable (stale = old data after a write that finished before the read
  started):** not stale — the read overlapped the write, either value is legal.

Either way it's removed by reading both keys with **one `MGET`** (atomic in
Redis). At that instant, `cache.ver == counter` implies no newer write has
committed — any commit needs an INCR first, which would make them differ. So
the value matches the DB at the read instant. What remains is response
transport time, which no approach (not even a direct DB read) can avoid.
In Redis Cluster the two keys need a shared hash tag (`{user:<id>}:version`,
`{user:<id>}`).

**Counter goes backwards — ABA (rare):**
```
The counter is the source of truth for "latest version". If it goes backwards,
an old cache entry can match again.

Failover with async replication:
  Master: counter v10, DB has data10. Replica lagged: counter v8. Cache {v9, data9} survived.
  Promote replica. Next writer: INCR → v9 → counter v9 == cache v9 → MATCH
  → serves data9 while DB has data10. STALE.

Counter key evicted while cache entries survive:
  Counter restarts at 1 and climbs. If an old {v57} entry survives until the
  counter reaches 57 → match → stale. (With a version-compare SET, the new
  {v1} can't overwrite {v57} → stuck mismatched → cache useless, not stale.)
```
Rare — needs failover or eviction of the counter key. Mitigations, cheapest
first:
- Initialise a missing counter at a **random 64-bit value** (`SET ver <rand> NX`,
  then INCR) — an old entry's version is vanishingly unlikely to be reached
  again (same idea as the random lease token).
- Never evict the counter: no TTL on it + a `volatile-*` eviction policy.
- Re-seed a missing counter from the DB's `row_version`.

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
Writer: SETNX lock → DEL cache → DB UPDATE succeeds → DEL lock fails
Result: lock stuck (Redis has it but writer can't delete it). TTL expires.
Cache was deleted (this combo has no write-through). Readers after TTL: miss → DB. CORRECT.
```

### M2b+M3+M5+M7+M10 (lock + watchdog + version-in-value + delete-first)

**All of above plus P2 FIXED:**
```
1. Reader A: miss → DB → v5 → hangs
2. Writer: lock → DEL → DB (v5→v6) → unlock
3. Reader B: miss → DB → v6 → Lua SET (v6 > nothing, succeeds)
4. Reader A: wakes → Lua SET → v5 < v6 → REJECTED. Cache keeps v6. CORRECT.
```
Caveat: this assumes Reader B fills first. If Reader A wakes before any newer
fill, the slot is empty, M5 has nothing to compare, and v5 is cached → stale
(the §17 gap; wider here since there is no write-through). Closed by M13.

**Network failure during write:**
```
Writer: lock → DEL cache → DB UPDATE → DEL lock (no write-through in this combo)
Result: cache empty (DEL ran), DB has v6. Lock released.
Reader: miss → DB → v6 → Lua SET (v6 > nothing, succeeds). CORRECT —
unless a slow reader holding v5 fills the empty slot first (§17 gap).
```

### M2b+M3+M5+M7+M8+M9+M10 (lock stack (M5))

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

**Slow reader overwrite — FIXED (Lua version compare on cache writes)** —
overwrites of existing cache only. Writes into an empty slot are the §17 gap,
closed by M13.

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
Reads all hit DB (Redis down). When Redis returns: an empty key is repopulated
by the next reader; a key whose DEL was skipped is served stale until its
CacheTTL or the next write (degrades to M1+M10).
```

**No scenario produces stale data** (under normal Redis operation) — with one
exception found later: a slow reader writing into the empty slot after the
writer's write-through fails or the writer dies post-commit. See §17; closed by
M13 (reader lease).

### M2b+M3+M7+M8+M9+M10+M13 (lock stack + lease — chosen)

Same writer path as above (M5 kept on the writer's write-through only). The
reader's miss path changes: GET → readCheck Lua (HIT / LOCKED / LEASED /
GRANTED) → on GRANTED, fresh DB read → leaseSet (SET only if the slot still
holds this reader's lease).

**Slow reader after a failed write-through — FIXED:**
```
1. Reader A: miss, no lock → GRANTED lease L1 → DB → v5 → stalls
2. Writer:   lock → DEL cache (L1 gone) → DB (v5→v6) → Lua SET FAILS → unlock
3. Reader A: wakes → leaseSet(L1): slot is empty, not L1 → DROPPED
4. Reader B: miss → GRANTED L2 → DB → v6 → leaseSet(L2) → cached. CORRECT.
```

**Slow reader, writer process dies after the DB commit — FIXED:**
```
1. Reader A: GRANTED L1 → DB → v5 → stalls
2. Writer:   lock → DEL (L1 gone) → DB commits v6 → process killed
3. Lock stays until TTL; readers wait (LOCKED). Reader A wakes → L1 gone → DROPPED
4. After TTL: next reader GRANTED → DB → v6 → cached. CORRECT.
```

**Writer dies before the DB commit — no lost cache time:**
```
Writer: lock → DEL → crash (DB unchanged, v5)
After TTL: next reader GRANTED → DB → v5 → cached. CORRECT, cache usable at once.
(Compare the tombstone option, which would reject v5 until the tombstone expired.)
```

**Cold miss, many readers — one DB read:**
```
Reader 1: GRANTED → DB → leaseSet → cached
Readers 2..N: LEASED → poll (capped exponential backoff) → HIT
DB sees 1 query instead of N, across all instances.
```

**Lease holder dies or its DB read fails:**
```
Dies:  lease expires after LeaseTTL → next poll by a waiter → GRANTED → fills
Fails: holder releases its lease (check-and-DEL) → a waiter gets GRANTED at once
```

**No scenario produces stale data** (under normal Redis operation, with fresh
DB reads after the grant).

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

### Lock stack: failure at every step of the write path

The lock stack write path (M2b+M3+M5+M7+M8+M9+M10, same for lock stack + lease) has 5 steps. At each
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
  data. Readers hit the old entry, so it is served stale until its CacheTTL or
  the next write invalidates it — the same state as M1+M10 after a failed DEL.

Reader impact during Redis outage:
  If Redis partially up (reads work, writes don't):
    GET cache → HIT old data → correct until the degraded DB UPDATE commits,
    stale after that (until CacheTTL or the next write)
  If Redis fully down:
    readers fall through to DB → correct data from DB

Writer latency:  ~0.1-0.5ms (SETNX) + failure detection
Reader latency:  ~0.1-0.5ms (cache hit, old = correct) or ~1-5ms (DB if Redis down)
Staleness:       None before the degraded DB UPDATE commits. After it, the old
                 entry can be served until its CacheTTL or the next write (same as
                 M1+M10 after a failed DEL).
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
    Correct. With the lease (M13), one reader takes the lease and reads DB;
    the rest wait on LEASED → 1 DB read in total (not N).
    (Lock stack (M5) without the lease: N readers hit DB — small stampede,
    which M12 only cuts to 1 per instance.)

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
  After unlock, one reader takes the lease and repopulates the cache from DB
  with the new correct data; the rest wait for its fill.

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
                 get correct data from DB. The lease holder repopulates cache.
                 CAVEAT: a slow reader holding pre-commit data can write it into
                 this empty slot once the lock is released (§17 scenario 2).
                 With M13 its lease was voided by the DEL → rejected.
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
  Cache empty → blocked until TTL expires → then one lease holder reads DB,
                the rest wait for its fill (correct)

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
                 (Lock stack (M5): a crash at step 4 leaves the empty slot that
                 a slow reader can fill with old data — the §17 gap, closed by M13.)
```

**Summary: lock stack failure paths**

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
  write, cache miss empty   same     No       ~0.1-0.5ms     ~0.1-0.5ms    block+S+0.1ms Yes (M9)
  cold miss (lease) empty   same     No       fill (holder)  ½fill+S       fill+S      Yes — 1 DB read total

Failure paths
1. SETNX fail     correct   same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes
2. DEL cache fail correct   same     No       ~0.1-0.5ms     ~0.1-0.5ms    ~1-5ms †    Yes/DB †
3. DB UPDATE fail rest/empty same    No       ~0.1-0.5ms *   block+0.1ms   block+1-5ms No ‡
4. Lua SET fail   empty     new      No       ~1-5ms *       block+1-5ms   block+1-5ms No ‡
5. DEL lock fail  correct   new      No       ~0.1-0.5ms     ~0.1-0.5ms    ~0.1-0.5ms  Yes (M8)
6. Writer crash   varies    varies   No       ~0.1-0.5ms *   TTL/2+1-5ms   TTL+1-5ms   Varies
7. Lease holder   empty/    same     No       fill *         LeaseTTL/2    LeaseTTL    Yes, after
   crashes        lease                       +fill          +fill         +fill       re-lease
8. Lease holder   lease→    same     No       fill *         ½fill+S       ~2×fill+S   Yes, after
   DB read fails  released                    +fill          +fill                     retry

*  Rare: reader arrives after writer unlocks. DB access is ~80-90% of lock hold
   time, so most readers hit the block → average case is the common one.
S  Poll slack: gap between unlock/fill and the waiter's next poll (≈ ½ the poll
   interval in effect; small with capped exponential backoff). See §16.
fill = lease holder's fill window ≈ n_d + s + n_r + l (DB SELECT + leaseSet).
†  Redis down — readers fall through to DB.
‡  "block" is the Redis lock wait, but its duration varies by scenario:

   Normal (cache miss): block ≈ full write time (~1-5ms DB + ~0.1-0.5ms Lua SET).
     Reader waits for the entire write to finish including cache write-through (M9).
     After unlock: cache hit (~0.1ms). This is the longest "happy" block.

   3. DB UPDATE fail: block ≈ DB fail time + restore attempt.
     Fast fail (constraint error): block <1ms. Timeout: block ~1-5ms.
     After unlock: cache hit if restored (~0.1ms), or DB SELECT if not (~1-5ms).

   4. Lua SET fail: block ≈ DB UPDATE time (~1-5ms) + SET fail time.
     DB already committed by this point. After unlock: cache empty → one reader
     takes the lease and does the DB SELECT; the rest wait on LEASED (½fill + S)
     instead of all hitting DB. A slow reader's old fill is rejected (M13).

   6. Writer crash: block ≈ up to TTL (configurable). Watchdog is dead, lock expires.
     After TTL: cache hit if populated before crash, or DB SELECT if not.

   7. Lease holder crashes (after GRANTED, before leaseSet): waiters sit on
     LEASED until LeaseTTL expires, then one gets GRANTED and fills. Bounded by
     LeaseTTL (keep it short: DB query timeout + margin). Only that key affected.

   8. Lease holder's DB read fails: it releases its lease (check-and-DEL), so
     waiters see an empty slot on their next poll; one gets GRANTED and retries
     the DB read. If the DB is down, every retry fails → waiters hit their max
     wait and return the error / fall back uncached.

   Worst total (any failure) = block + S + DB SELECT ≈ 2 DB accesses (~2-10ms).
   Exception: crash = TTL + DB SELECT (TTL dominates); lease holder crash =
   LeaseTTL + fill.
   TTL is configurable — tune based on observed write latency and acceptable
   crash-recovery delay (e.g. 500ms–2s for fast writes, longer for slow ones).
All worst cases with DB reads: M13 (lease) → 1 DB read across all instances;
M12 additionally collapses in-process pollers to 1 per instance.
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
the write and are blocked behind the writer's row lock (locking read; under MVCC a plain SELECT doesn't wait, but still adds a DB query). Lock stack readers pay
at most: block (Redis lock wait, ~1-5ms) + DB SELECT (~1-5ms) = 2 DB access
durations, but never row-lock-wait + DB SELECT (which would be 2 DB accesses
from different causes stacked). CAS readers can pay: MGET + row lock
wait + SELECT ≈ 2 DB accesses where the wait and the read both happen at the
DB level.

The invariant: **at no failure point does a reader ever see stale data.** The
cache is either correct (serve it), or empty (fall through to DB, which is
always the source of truth). The delete-first ordering (M7) guarantees this —
cache is destroyed before DB changes, so any intermediate state has either
old-correct-cache or no-cache, never new-DB-with-old-cache.

The invariant only holds if nothing *outside* the write path can fill the empty
slot with old data. A slow reader can (§17). M13 (reader lease) makes the
invariant hold: an empty slot can only be filled by a reader whose lease
survived, which means its DB read happened after the last writer's DEL.

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
Normal reader (no write):           ~0.1-0.5ms (MGET version + cache, compare)
```

**Reading the counter and the cache: two GETs vs one MGET.**

The CAS read needs two values — the version counter and the cached entry — and
compares them. There are two ways to fetch them:

```
Two GETs (2 round trips, ~0.2-1ms):
  GET user:<id>:version   → v5
  GET user:<id>           → {v5, data5}
  compare → match → return

One MGET (1 round trip, ~0.1-0.5ms):
  MGET user:<id>:version user:<id>   → [v5, {v5, data5}]
  compare → match → return
```

Two GETs work most of the time, but other clients' commands can run between
them. If a writer's whole INCR → DB commit lands in that gap, the reader pairs
an old counter with the old cache entry, sees a match, and returns data5 while
the DB already has data6:

```
1. Reader: GET version → v5
2. Writer: INCR → v6 → DB commit (data6)
3. Reader: GET cache → {v5, data5} → v5 == v5 → returns data5   ✗
```

MGET reads both keys in one atomic command, so the pair is a snapshot of one
instant. At that instant, `cache.ver == counter` means no newer write has
committed yet (a commit requires the INCR first), so the value matches the DB.
It is also one round trip instead of two. So: **MGET, always.** (A pipeline of
two GETs saves the round trip but is *not* atomic, so it doesn't close the gap.
In Redis Cluster, both keys need a shared hash tag.)

The reader costs below use MGET.

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
Reader latency:  ~0.1-0.5ms (normal, completely unaffected)
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
    Reader does: MGET → version v(N+1), cache {v(N), data}. Mismatch.
    Reader falls through to DB SELECT.
    BUT: the writer holds the row lock on this row (its UPDATE is in-flight).
    Reader's SELECT is blocked behind the writer's row lock (locking read;
    under MVCC a plain SELECT doesn't wait — it returns v(N) at once — but it
    is still a DB query).
    
    Reader pays: MGET (~0.1-0.5ms) + row lock wait (~1-5ms) + SELECT (~1-5ms)
    ≈ MGET + 2 DB access durations (~2.1-10.5ms)
    
    Compare the lock stack: readers wait on the Redis lock for about as long
    (the write time), then get a cache hit — no DB query. CAS sends readers to
    the DB, where they queue behind the row lock (locking read) or at least add
    a query to the DB (MVCC).

  PHASE 2: AFTER the DB UPDATE fails (writer done, row lock released):
    Reader does: MGET → version v(N+1), cache {v(N), data}. Mismatch.
    Reader falls through to DB SELECT → clean SELECT (row lock released).
    
    Reader pays: MGET (~0.1-0.5ms) + clean SELECT (~1-5ms) = ~1.1-5.5ms
    
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
  During write attempt: ~0.1-0.5ms + row lock wait + SELECT ≈ 2 DB accesses (~2.1-10.5ms)
  After failure (sustained): ~0.1-0.5ms + clean SELECT ≈ 1 DB access (~1.1-5.5ms)
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
  If retry fails: not sustained. Counter and DB are both at v(N+1), so the next
  reader's DB read returns row_version N+1 == counter and the reader fill rule
  (§11 M6) repopulates the cache. Unlike Failure 2, this self-heals on the first
  read.

Reader impact:
  Version mismatch → DB fallback → correct NEW data from DB.
  No row lock wait — DB transaction already committed.
  
  This is purely a cache availability problem, not a correctness problem.
  Every read returns correct data; it just comes from DB instead of cache.

Reader latency:
  ~0.1-0.5ms (MGET) + DB SELECT ~1-5ms = ~1.1-5.5ms (1 DB access, no lock wait)
  +M12: 1 shared DB read per instance

Staleness:       None — mismatch always triggers DB read.
Duration:        Until the first reader's fill (row_version == counter), or the
                 retry succeeds.
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
    Row lock wait + SELECT ≈ 2 DB accesses (~2.1-10.5ms)
    (DB detecting dead connection and rolling back)
  After crash (transaction resolved):
    Clean SELECT ≈ 1 DB access (~1.1-5.5ms)
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
  no write        v(N)     v(N)       v(N)        No      ~0.1-0.5ms     ~0.1-0.5ms     ~0.1-0.5ms  Yes
  write ongoing   v(N+1)   updating   v(N)        No      ~1.1-5.5ms *   rlk+1-5ms      rlk+1-5ms   No ‡

Failure paths (propagation stopped at ↓)
1. INCR fail      v(N)     v(N)       v(N)        No      ~0.1-0.5ms     ~0.1-0.5ms     ~0.1-0.5ms  Yes
                  ↑ failed here — nothing changed
2. DB UPDATE fail v(N+1) † v(N)       v(N)        No      ~1.1-5.5ms *   rlk+1-5ms      rlk+1-5ms   No ‡‡
                           ↑ failed here — version ahead, DB and cache still at v(N)
3. SET cache fail v(N+1) † v(N+1)     v(N)        No      ~1.1-5.5ms     ~1.1-5.5ms     ~1.1-5.5ms  After 1st fill
                                      ↑ failed here — version and DB at v(N+1), cache stuck at v(N)
4. Writer crash   varies   varies     varies      No      ~0.1-0.5ms *   rlk+1-5ms      rlk+1-5ms   No ‡‡
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
     (Locking read. Under MVCC rlk ≈ 0 — the SELECT returns v(N) at once.)
     Total: MGET (~0.1-0.5ms) + rlk (~1-5ms) + SELECT (~1-5ms) ≈ 2 DB accesses.
     No cache fallback — version mismatch always forces DB.

   2. DB UPDATE fail: rlk ≈ DB fail time.
     Fast fail (constraint error): rlk <1ms. Timeout: rlk ~1-5ms.
     After row lock released: clean SELECT (~1-5ms).

   4. Writer crash (during DB): rlk ≈ until DB rolls back transaction.
     DB detects dead connection → rollback → row lock released.
     Faster than lock stack's TTL wait (DB timeout vs configurable TTL).

   Worst total = MGET + rlk + SELECT ≈ 2 DB accesses (~2.1-10.5ms).

‡‡ Sustained: after failure, orphaned version means ALL readers pay DB latency
   (~1.1-5.5ms per read, clean SELECT, no row lock) until next successful write.
   Duration depends on write frequency — seconds for hot keys, hours for cold.
   Applies when the DB did NOT commit (failure 2, or a crash before commit).
   Failure 3 (DB committed, SET failed) is not sustained: the first reader's
   fill (row_version == counter) repairs it.
   Lock stack does NOT have this: cache recovers immediately after failure.

+M12: DB reads coalesced to 1 per instance, but per-reader latency unchanged.
```

**Key difference: lock stack vs CAS failure recovery**

The lock stack's failure modes mostly leave readers with a cache hit (~0.1-0.5ms)
or a brief block followed by a cache hit. Worst case is block (~1-5ms) + DB
SELECT (~1-5ms) ≈ 2 DB accesses — but only for the readers caught during that
one failure. After recovery, cache is restored or repopulated by the first reader.

CAS's failure modes where the DB did not commit (2, and 4 when the crash is
before the commit) leave the cache **useless** until the next successful write.
(Failure 3 is repaired by the first reader's fill.) Every reader pays DB latency (~1.1-5.5ms) for the
entire duration — and during the write attempt itself, readers behind the row
lock pay ~2 DB accesses (~2.1-10.5ms). For a key that writes infrequently, an
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
on the same row. With a locking read, the reader's SELECT must wait for the
writer's transaction to commit before it can read (under MVCC a plain SELECT
doesn't wait — it returns the old committed value at once — but it is still a
DB query). With a locking read the reader pays:

```
CAS reader during write:
  MGET version + cache        ~0.1-0.5ms  (one Redis round trip)
  → mismatch detected
  → DB SELECT                 blocked behind writer's row lock (~remaining write time)
  → row lock released         → SELECT completes (~1-5ms for the read itself)
  Total: ~2-11ms (dominated by row lock wait + SELECT)

Compare — lock stack reader during write:
  GET cache → HIT (serve-if-exists, M8) → return
  Total: ~0.1-0.5ms
  OR
  GET cache → MISS → readCheck (LOCKED) → poll → unlock → readCheck → HIT (M9, +S)
  Total: ~write time + S + 0.1-0.5ms (same wait, but ends with cache hit not DB read;
  vs MVCC CAS the no-wait DB read can be quicker at low load, but it's a DB query)
```

The key insight: CAS achieves zero staleness by **abandoning the cache whenever
correctness is in question**. The cache is a fair-weather optimization — it works
when no writes are happening, and turns off when they are. During any single
write, ~80-90% of concurrent readers for that key fall through to DB and wait
behind the writer's row lock (locking read; under MVCC they don't wait but each
is still a DB query). A DB row lock is being held either way (CAS or
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
but are blocked behind the writer's row lock (locking read) — paying ~2 DB
access durations instead of 1. Under MVCC they don't wait, but each is still a
DB query on a DB that is busy with the writes. The row lock is held sequentially (DB serializes), but the
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
can overlap). The DB bottleneck is the same either way. See §15 for why
this means the lock stack's serialization cost is minor.

---

## 14. Real Downsides of the Lock Stack

| Downside | Impact | Reality check |
|---|---|---|
| Writer serialization | Writers wait on Redis lock before DB | Writers already serialize on DB row lock. Redis adds ~0.5-1ms (~5-20%). Not double. |
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

**In the MVCC scenario (plain, fresh SELECT):** the no-lock reader doesn't wait
— it gets the old committed value in one DB round trip. At low load that can be
faster per reader. The lock still wins on what matters at scale: zero reader DB
load (vs one query per reader, with DB latency rising as readers grow) and the
new value instead of the old one. **Poll slack (minor):** the lock stack reader
also pays a small slack S after the unlock (≈ ½ the poll interval in effect).

---

## 15. Why Locks Work Here (When They Don't in Low-Level Systems)

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

With Redis lock (lock stack):
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
  GETDEL cache    ~0.1-0.5ms  (1 Redis round trip: save old value + delete;
                               GET + DEL without Redis 6.2 = 2 RTs, the 5-RT
                               naive path in §16)
  DB UPDATE       ~1-5ms      (query + commit + fsync — same as CAS)
  Lua SET cache   ~0.1-0.5ms  (1 Redis round trip)
  DEL lock        ~0.1-0.5ms  (1 Redis round trip)
  Total:          ~1.4-7ms

Difference:       ~0.2-1ms per write (~5-20% overhead on the write path)
```

The lock stack adds 2 extra Redis round trips (SETNX + DEL lock) and replaces
a plain SET with a Lua SET. Total overhead: ~0.2-1ms. On a ~5ms write, that's
~5-20% more — not negligible, but far from the 100-1000× penalty that makes
locks unthinkable in C++.

### What that overhead buys — read savings

The write overhead is a one-time cost per write. The read benefit accrues to
every concurrent reader:

```
Example: 100 reads + 1 write per second, same key:

CAS (MGET read):
  Write: ~1.2-6ms
  Readers during write: MGET → version mismatch → DB fallback → row lock wait + SELECT
  Each reader pays: ~2-10ms (MGET + row lock wait + SELECT)
  DB connections during write: up to 100 (all readers hit DB)

Lock stack + lease:
  Write: ~1.4-7ms
  Readers during write: cache hit (M8) or block → cache hit (M9)
  Each reader pays: ~0.1-0.5ms (cache hit) or ~1-5ms block + S + ~0.1-0.5ms (cache hit)
  DB connections during write: 0 (all readers served from cache)

Extra write cost:    ~0.2-1ms (one time, one writer)
Read savings:        ~1-5ms × N concurrent readers (every write cycle)
DB load reduction:   N readers × ~1-5ms → 0
```

**In the MVCC scenario (plain, fresh SELECT):** CAS readers skip the row lock
wait and pay ~1-5ms (MGET + one DB round trip) for the old committed value. The
per-reader saving shrinks (it can reverse at low load), but the DB load line
does not change: up to N queries per write for CAS, 0 for the lock stack, and
those N queries raise DB latency as readers scale.

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

## 16. Network Latency and Cost Model

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

### Variables

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
- CAS (no write): 2 RTs (GET version + GET cache), or 1 RT with `MGET`
  (recommended — atomic as well as faster)
- Lock stack during write: hit → 1 RT (plain GET, M8 serve-if-exists); miss →
  GET + readCheck Lua (2 RTs), then polls until the unlock

### Algebra — costs and breakeven: when does CAS beat the lock stack?

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
"Cold miss" is a read with no write in progress but nothing cached.

One new variable for the lock stack's waiting readers:

| Variable | What |
|---|---|
| S | Poll slack: gap between the unlock (or lease fill) and the waiter's next poll. Polls *during* the block are free (they overlap the writer's work); only this final gap is extra. ≈ ½ × the poll interval in effect when the block ends. With capped exponential backoff (0.5 → 1 → 2 → 4ms) a normal-length block ends during the short early polls, so S is small; S → 0 when n_r ≫ the poll interval (the readCheck round trip itself dominates). |

CAS reads use one `MGET` (counter + cache, 1 RT, atomic) — see §13 M6.

No write happening:
- Lock stack: n_r + p (cache hit)
- CAS: n_r + p (MGET, version matches → cache hit; + a negligible compare)
- Simple: n_r + p (cache hit)

During write — modeled as reader arriving at a uniformly random point during
the blocked period, so average wait = 1/2 of the remaining blocked time:

**Lock stack reader (cache miss, blocked after DEL cache):**
- The blocked window starts after DEL cache: DB UPDATE + post-DB Lua = (n_d + u) + (n_r + l)
- Reader arrives uniformly in this window → average wait: 1/2(n_d + u + n_r + l)
- The unlock is noticed on the next poll: + S
- That poll is a readCheck Lua → HIT (M9 wrote cache before unlocking): n_r + l
- Total: 1/2(n_d + u + n_r + l) + S + (n_r + l)
- = **3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S**

**CAS reader (version mismatch → DB fallback, locking read):**
- MGET: n_r + p
- Mismatch detected → DB SELECT, but writer holds row lock during DB UPDATE
- Row lock held for: n_d + u. Reader arrives mid-UPDATE → average wait: 1/2(n_d + u)
- After row lock released, SELECT executes: n_d + s
- Total: (n_r + p) + 1/2(n_d + u) + (n_d + s)
- = **n_r + p + 3/2 n_d + 1/2 u + s**

(With two separate GETs the first term would be 2(n_r + p); MGET is one round
trip and atomic.)

**Simple reader (during write):**
- GET cache → hit (stale for ms — DB committed but DEL not sent yet)
- = **n_r + p**

**Note — M8 (serve-if-exists) best case for lock stack:** Readers arriving
between SETNX and DEL cache (cache still has old correct data) or between Lua
SET cache and DEL lock (cache has new correct data) get a cache hit at n_r + p
with zero blocking. The average formula above models the blocked case. In
practice, M8 makes real-world lock stack reader cost lower than the formula.

**Cold miss (no write in progress, nothing cached):**

| Approach | Path | Cost |
|---|---|---|
| Simple | GET → DB → SET | 2n_r + 2p + n_d + s |
| CAS | MGET → DB → Lua fill | 2n_r + p + l + n_d + s |
| Lock + lease, lease holder | GET → readCheck (GRANTED) → DB → leaseSet | **3n_r + p + 2l + n_d + s** |
| Lock + lease, other readers (waiters) | GET → readCheck (LEASED) → poll → readCheck (HIT) | **≈ 3/2 n_r + 3/2 l + 1/2 n_d + 1/2 s + S** |

- The lease holder pays one extra Redis round trip (the readCheck) versus Simple.
- Each waiter waits half the holder's fill window (n_d + s + n_r + l) instead of
  doing its own DB read. With N concurrent misses: Simple and CAS do **N** DB
  reads; the lease does **1** (across all instances).

**Total system cost per write cycle (write + R readers during write):**

```
Approach         Write cost                + R × (reader during write)
───────────────────────────────────────────────────────────────────────────
Simple           n_r + p + n_d + u         + R(n_r + p)
Lock pipelined   2n_r + 2l + n_d + u       + R(3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S)
Lock naive       5n_r + 4p + l + n_d + u   + R(3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S)
CAS              2n_r + 2p + n_d + u       + R(n_r + p + 3/2 n_d + 1/2 u + s)
```

Per-reader cost difference (CAS − Lock stack):
= (n_r + p + 3/2 n_d + 1/2 u + s) − (3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S)
= **n_d + s − 1/2 n_r − S + p − 3/2 l**
≈ **n_d + s − 1/2 n_r − S**

The lock stack reader saves the DB round trip (n_d + s) but pays half an extra
Redis round trip and the poll slack, so **the per-reader saving is thin** — it
is only clearly positive when n_d + s is well above 1/2 n_r + S.

**Deriving the breakeven: naive lock stack vs CAS (full variables)**

Lock naive total < CAS total when:

(5n_r + 4p + l + n_d + u) + R(3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S)
< (2n_r + 2p + n_d + u) + R(n_r + p + 3/2 n_d + 1/2 u + s)

Left − Right:
= (3n_r + 2p + l) − R(n_d + s − 1/2 n_r − S + p − 3/2 l)

Lock < CAS when:
(3n_r + 2p + l) < R(n_d + s − 1/2 n_r − S + p − 3/2 l)

Approximating (p, l small relative to n_r and n_d):
**R > 3n_r / (n_d + s − 1/2 n_r − S)**, provided the denominator is positive.

If n_d + s ≤ 1/2 n_r + S, the lock stack reader is *slower* than the CAS
reader and naive lock never catches up on latency alone.

Pipelined lock vs CAS: write costs differ only by 2l − 2p ≈ 0, so pipelined
lock wins on latency at **R ≥ 1 whenever the denominator is positive**, and
loses per reader when it isn't.

| Scenario (qualitative) | Denominator n_d + s − ½n_r − S | Naive breakeven | Pipelined |
|---|---|---|---|
| Same network, n_r ≈ n_d | ≈ ½ n_d + s − S — small, near zero if S ≈ s | **Large R** (can be 10+) | R ≥ 1 if positive, marginal |
| Redis local, DB remote (n_r ≪ n_d) | ≈ n_d + s — large | **R ≥ 1** | R ≥ 1, clear win |
| Both remote, equal (n_r ≈ n_d, large) | ≈ ½ n_d + s — S ≈ 0 at large n_r | **R of a few** | R ≥ 1 |
| Redis remote, DB local (n_r ≫ n_d) | negative | **Never** | Never — CAS reader cheaper |
| Localhost (n_r, n_d tiny) | ≈ s − S — S can exceed s | **Never / marginal** | Marginal — everything ≈ u anyway |

Key insights:
- **n_r ≪ n_d (Redis sidecar, DB remote):** lock stack wins clearly — its
  overhead is cheap, CAS readers pay the expensive DB trip.
- **n_r ≈ n_d:** on per-reader latency during a write, lock and CAS are close.
  The lock stack's advantage there comes from the non-latency points in §18
  (M8, post-write cache, fresh data, P4), not from this formula.
- **n_r ≫ n_d or localhost:** CAS readers are cheaper during a write.
- **Poll slack matters:** S is the term under the implementer's control. A
  short first poll interval (or a wake-up signal) keeps it small.

**When is Simple the best? Always — if staleness is acceptable.**

Simple has the lowest total cost at every n_r, n_d, R:
- vs pipelined lock stack: saves n_r + (2l - p) ≈ n_r on write, saves
  R(1/2 n_r + 1/2 n_d + 1/2 u + S) on readers (lock stack readers wait,
  Simple readers get stale cache hit immediately)
- vs CAS: saves n_r + p on write, saves R(3/2 n_d + 1/2 u + s) on readers

The question is never latency. Simple always wins on latency. The question
is whether the stale window is acceptable.

**Approximation regimes (when terms drop):**

| Regime | Condition | What simplifies |
|---|---|---|
| Typical same-DC | p, l << n_r << u | Drop p, l. Formulas become coefficients of n_r, n_d, u, s. |
| Localhost | n_r ≈ p | Can't drop p. But all Redis terms tiny vs DB. Everything ≈ n_d + u. |
| Cross-region | n_r >> u | u drops from comparison. Differences purely in n_r, n_d coefficients. |
| Redis ≈ DB distance | n_r ≈ n_d | Replace both with r. Formulas simplify to single-variable. |

### Caveats that change the result

**Caveat — n_d, s and u are load-dependent; these formulas are low-load.**
The formulas treat DB costs as constants. They aren't:
- CAS sends every reader caught in a write (and every mismatch) to the DB, so
  DB read load grows with R. The lock stack sends **zero** readers to the DB
  during a write (they wait in Redis, then hit cache) and one lease holder per
  cold miss.
- DB latency rises with utilisation (queueing, roughly s / (1 − utilisation)),
  and a saturated connection pool adds waits and timeouts. So a CAS reader's
  n_d + s grows with R; a lock stack reader's cost does not.
- Extra reads also compete with writes, so u grows for every approach under CAS.
- MVCC only removes the row-lock wait — the CAS reader still pays the DB round
  trip and still adds DB load.

So the per-reader saving n_d + s(load) − 1/2 n_r − S is near zero at low load
(n_r ≈ n_d) and **grows as readers scale**. The "roughly a tie" results above
are the best case for CAS; at scale the lock stack wins on latency as well as
DB load.

**Caveat: MVCC and row lock waits.**

The CAS reader formula includes a 1/2(n_d + u) row lock wait term, assuming
the reader's DB SELECT blocks behind the writer's UPDATE. In MySQL InnoDB with
default isolation (REPEATABLE READ or READ COMMITTED), a plain SELECT uses
MVCC — it reads the last committed snapshot **without blocking** on row locks.
Only locking reads (SELECT FOR UPDATE, SELECT LOCK IN SHARE MODE) block.

If MVCC applies (plain SELECT for cache population):
- CAS reader cost drops to: (n_r + p) + (n_d + s) — no row lock wait
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

With MVCC, drop the 1/2(n_d+u) from CAS reader cost. The CAS reader is then
cheaper than a blocked lock stack reader in most deployments (see §18 "Does
MVCC change the ranking?") — but it still pays a full DB round trip for data
the cache already had.

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
  -- KEYS[1] = cache key, KEYS[2] = lock key
  -- ARGV[1] = new version, ARGV[2] = "<version>\n<data>", ARGV[3] = CacheTTL, ARGV[4] = writer_id
  local current = redis.call('GET', KEYS[1])
  local sep = current and string.find(current, '\n')
  local ver = sep and tonumber(string.sub(current, 1, sep - 1))
  if not (ver and ver >= tonumber(ARGV[1])) then                 -- M5: skip if same/newer
    redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[3])) -- SET cache
  end
  if redis.call('GET', KEYS[2]) == ARGV[4] then                  -- only our own lock
    redis.call('DEL', KEYS[2])                                   -- DEL lock
  end
  return 'OK'
→ version compare + SET cache + ownership-checked DEL lock, 1 network round trip
```

The version compare (M5) and SET are already in a Lua script in the lock
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

- Read path: pipeline GET version + GET cache into 1 RT (from 2). Prefer
  `MGET` — also 1 RT, and atomic (a pipeline lets other clients' commands run
  between the two GETs, so it doesn't close the straddling-read window; see §13
  M6). The formulas in this section use MGET: n_r + p.
- Write path: INCR must be before DB, SET must be after — can't reduce below
  2 RTs. The DB is between the two Redis operations, so they can't be batched.

### Rough scenario estimates (illustrative only)

The algebra above is what matters — plug in your own benchmarked values. For a
feel, rough estimates with p ≈ 0.005, l ≈ 0.03, u ≈ 2, s ≈ 0.3 (ms) and the
0.5 → 1 → 2 → 4ms backoff:

| Scenario | n_r / n_d | Lock reader (write) | CAS reader (write) | Reader-cost winner | Naive lock breakeven |
|---|---|---|---|---|---|
| Same DC | 0.3 / 0.3 | ~2ms | ~2ms | ≈ tie | R ≈ 15+ |
| Redis local, DB remote | 0.01 / 5 | ~5ms | ~9ms | Lock | R ≥ 1 |
| Redis remote, DB local | 10 / 0.1 | ~16ms | ~11ms | CAS | never |
| Localhost | 0.02 / 0.02 | ~1.8ms | ~1.4ms | CAS (marginal) | never |

Simple's reader during a write is always ≈ n_r + p (stale hit). Write costs are
dominated by n_d + u in every scenario except remote Redis, where the naive lock
stack's 5 Redis RTs dominate.

Observations:
- **Same DC:** per-reader costs roughly equal; pipelined lock ≈ CAS in total
  latency. Choose on correctness/cache behaviour, not latency.
- **Redis local:** lock stack wins massively — CAS readers pay the remote DB.
- **Redis remote:** CAS readers are cheaper during writes, and the naive lock
  stack's writes are much more expensive. Reconsider Redis placement.
- **Localhost:** DB processing (u) dominates everything; differences are noise.

### The core conclusion

From the algebra and caveats:
- Simple always has the lowest latency; the question is only whether its ms
  stale window is acceptable.
- Lock stack vs CAS per reader during a write: lock wins when n_d + s >
  1/2 n_r + S; roughly a tie at low load with n_r ≈ n_d; CAS faster with MVCC
  at low load. As readers scale, the lock stack wins — CAS readers are DB
  queries and DB latency rises with load; lock stack readers add none.

On deployment: the analysis in this document assumes **same-datacenter deployment** (Redis RTT
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

---

## 17. Lock Stack — Slow Reader Cache-Populate Failure Modes

Lock stack (M5) — M2b+M3+M5+M7+M8+M9+M10 — prevents stale reads **during** the
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

**Fix (formerly implemented; superseded by M13):** Add a lock check to the reader's cache-populate Lua
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

> **Superseded by M13 (Option E below).** With reader leases, the writer's DEL
> voids the slow reader's lease, so its populate is rejected whether or not the
> lock is still held. The lock check in `readerVersionSetScript` is no longer
> needed; `leaseSetScript` replaces it.

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

**STATUS: RESOLVED — Option E (reader lease, M13) chosen.** Options A–D are
kept below as the alternatives considered.

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

#### Option D: Reader lock-wait evidence (speculative, not pursued)

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

**STATUS (historical): interesting direction, but significant nuances with
multiple writers and lock identity.** Not pursued — Option E achieves what D was aiming for ("prove my DB read came
after the writer") with a token instead of inferred lock history.

#### Option E: Reader lease (M13) — CHOSEN

A reader that misses cache first places a random token into the empty slot
(`L:<token>`, short TTL), then reads DB, then writes its result **only if the
slot still holds its token**. The writer's DEL-first step wipes any lease, so
any reader whose DB read might predate the write loses its lease and its write
is dropped.

```
Reader:  readCheck Lua → GRANTED (slot now "L:<token>")
         DB SELECT → version N
         leaseSet Lua: slot == "L:<token>"? → SET data : drop
Writer:  lock → DEL cache (voids lease) → DB UPDATE → versionSet → unlock
```

**Scenario 2 re-traced with leases:**

```
1. Reader: miss → GRANTED lease L1 → DB SELECT → version N → stalls
2. Writer: lock → DEL cache (L1 gone) → DB UPDATE (N→N+1) → write-through FAILS → unlock
3. Slow reader: leaseSet(L1) → slot is empty, not L1 → REJECTED ✓
4. Next reader: miss → GRANTED L2 → DB → N+1 → leaseSet(L2) → cached ✓
```

**All cases:**

| Case | Result |
|---|---|
| Scenario 1 (writer between DB commit and write-through) | DEL voided the lease → rejected ✓ |
| Scenario 2 (write-through failed, lock released) | DEL voided the lease → rejected ✓ |
| Writer process dies **after** DB commit | Lock held until TTL (readers wait); slow reader's lease voided → rejected; after TTL the next reader leases and reads N+1 ✓ |
| Writer process dies **before** DB commit | After TTL the next reader leases, reads N, caches N. **No lost cache time** (tombstone's weakness gone) ✓ |
| Reader stalls past LeaseTTL | Slot is empty or holds another lease → rejected ✓ (random tokens rule out a match) |
| Many readers miss at once | One gets the lease and reads DB; the rest see `LEASED` and wait → no stampede, across instances ✓ |

**Why E over the others:**

| Option | Stale possible? | Cost | Timing-dependent? |
|---|---|---|---|
| A. Accept | Yes, until next DB populate / CacheTTL | None | — |
| B. Shorten lock to ReadOpTimeout on failure | No (if slow reader finishes within ReadOpTimeout) | Readers blocked up to ReadOpTimeout on failure; only covers the error path — a dead process can't shorten anything | Yes |
| C. Tombstone | No | Crash-before-DB: tombstone{N} rejects the correct N, cache useless for tombstone TTL; needs a version up front (extra DB read if cache was empty); stall past tombstone TTL reopens the gap | Yes (tombstone TTL) |
| D. Lock-wait evidence | Unresolved with multiple writers | Complex | Yes |
| **E. Reader lease** | **No** | **No extra RT vs lock stack (M5) (grant folded into readCheck); one extra vs Simple on a miss** | **No** |

The tombstone could never tell "crash before DB" from "crash after DB" — same
tombstone version, same reader version, opposite correct answers. Bumping the
tombstone to N+1 and switching ≥ to > gives identical outcomes. The lease
sidesteps this: it doesn't ask "is this data newer?", it asks "has any writer
touched this key since my DB read began?" — and that is exactly the question.

**Clarifications that fell out of the analysis:**

- **What counts as stale.** A read is only actually stale if it returns old data
  *after a write that finished before the read started*. A read that overlaps a
  write may legitimately return either value (this is also what MVCC gives a
  concurrent SELECT). The dangerous staleness is the kind that **outlives the
  write** — which is exactly what scenario 2 produces and what the lease stops.
- **Error path vs process death.** On the error path (write-through fails,
  process alive) the deferred unlock releases the lock *immediately*, so the
  slow reader only needs to stall for about one write — the more likely case.
  On process death the lock survives until its TTL. Before M13 these needed
  different fixes (Option B only helps the error path). With M13 neither
  matters — the DEL already voided the lease.

**Residual risks (outside this project's scale):**

- **Writer paused past its lock TTL** (stop-the-world GC, VM freeze): the lock
  expires, another writer runs, then the paused writer wakes and writes. Fix:
  fence the DB write with a conditional update
  (`UPDATE … SET …, row_version = ? WHERE id = ? AND row_version = ?`).
- **Redis failover losing a recent DEL** (async replication): the old value can
  reappear. Out of scope for a single Redis instance.

#### Lease design concepts (what each rule prevents)

These are not just implementation details — each rule exists to stop a
specific problem.

**1. Readers need no version compare; the writer keeps one.**
`leaseSetScript` only replaces the reader's own placeholder, which holds no
data. It can never overwrite real data, so "old overwrites new" is impossible
on the reader path. A successful populate proves no writer touched the key
between grant and populate:

| Writer relative to the reader | Outcome |
|---|---|
| Finished before the grant | Its commit is visible to the reader's (later) DB read → data current |
| Holding the lock at grant time | readCheck refuses the grant (LOCKED) |
| Locked after the grant | Its DEL (before its DB update) wiped the lease → populate dropped |

The writer's write-through, by contrast, writes over *whatever is there*,
including data. A writer paused past its lock TTL could wake and overwrite a
newer writer's value — M5's version compare on the writer's SET rejects that.
So M5 moves from readers to writer-only; it isn't removed.

**2. Lease expiry plays no part in correctness.** Correctness comes from the
writer's DEL. `LeaseTTL` only matters for liveness (freeing the slot if a
lease holder dies). An expired lease just drops a fill.

**3. One key, one DEL.** Lease and data share the key; the slot holds one or the
other, never both. The writer's existing DEL clears either. A separate lease
key would need two DELs and a two-key check — more to keep in sync.

**4. The DB read must be fresh, after the grant.** Autocommit, or a transaction
begun after GRANTED. A `REPEATABLE READ` snapshot opened before the grant can
predate a write that committed before the grant — and the lease would then
protect stale data.

**5. No watchdog on the lease — keep LeaseTTL short instead.**
The lock's watchdog exists because expiry mid-work breaks correctness (P8).
Lease expiry mid-work is harmless (point 2). So a crashed lease holder costs
waiters on *that key* at most `LeaseTTL`, after which one waiter is GRANTED.
Keep `LeaseTTL` ≈ DB query timeout + margin, and the DB query timeout **below**
`LeaseTTL` — otherwise, while the DB is slow, every holder loses its lease
before populating and nothing gets cached. The error path (process alive)
releases the lease immediately with a check-and-DEL. A watchdog would add a
goroutine per miss, and misses arrive in bursts.

**6. Plain GET on hits; Lua only on a miss.** Every script call pays a fixed
cost (SHA lookup, KEYS/ARGV setup, VM entry/exit) regardless of which branch
runs — roughly 2–5µs of Redis CPU vs ~1µs for a plain GET. Redis is
single-threaded and shared, so running the script on every read would roughly
double Redis CPU on the hottest path. GET-first costs one extra round trip on a
miss only, compared with always calling the script. A lease value (`L:` prefix) is decoded as a miss in application code.

**7. Waiting: capped exponential backoff.** Readers on LOCKED/LEASED poll with
intervals 0.5 → 1 → 2 → 4ms (capped) plus jitter.
- Latency: polls during the block overlap the writer's work and are free; the
  only extra is the gap between unlock/fill and the next poll — on average half
  the current interval. Short early intervals keep this small for normal waits.
- Redis load: `poll rate ≈ keys blocked now × waiters per key × 1/interval`.
  Backoff stops long waits (crashed writer, slow DB) from hammering Redis.
  Singleflight (M12) caps waiters at 1 per key per instance — the bigger lever.
  Sharding spreads different keys across shards but doesn't help one hot key.
- Optional, not implemented: a wake-up signal instead of polling (Redis pub/sub
  — one subscriber per instance, in-process fan-out, register before checking,
  long fallback timeout). Not `BLPOP` (wakes one waiter, holds a connection per
  waiter) and not an MQ (ms latency; durable delivery isn't needed — a missed
  wake-up costs one poll). More moving parts than it's worth at this scale.

**8. Cache "not found" (cache penetration).** If the DB has no row, the lease
holder writes a short-TTL negative entry (e.g. `0\n`, 30s) through the same
`leaseSetScript`. Otherwise every read of a missing ID loops lease → DB →
nothing — wasted DB load and an easy abuse path. The create path DELs the key.

**9. Eviction can remove the lock.** Lock, lease, and data keys all have TTLs,
so under memory pressure an eviction policy can evict the lock mid-write and
let a second writer in. Evicting a lease or data key is harmless. Keep memory
headroom and alert on evictions.

**10. Redis Cluster: hash tags.** readCheck touches the cache key and the lock
key; in Cluster both must share a slot: `{user:<id>}` and `{user:<id>}:lock`.

---

## 18. Approach Ranking (After Full Analysis)

After the full analysis — failure paths, latency formulas with separate variables
(n_r, n_d, p, l, u, s), network caveats, breakeven math, MVCC considerations,
heavy write scenarios — the ranking:

**1. Simple (M1+M10+M4) — best for most systems.**

Lowest total cost at every n_r, n_d, and R. No Redis write dependency — writes
succeed even when Redis is down. Simplest to implement and operate — no locks,
no watchdog, no Lua scripts, no version counters.

The ms stale window (P1) is the only correctness cost. It requires a **read**
of a key that is being written to land in the ~n_r window between DB commit and
DEL delivery — and with version-in-key (M4), that reader must also be using the
old version's key. Two updates in quick succession are not needed; a read
landing in that sub-millisecond gap is. The longer case is the DEL failing
(Redis error or writer crash between commit and DEL): the old key is then served
until its CacheTTL or until readers move to the new version key.

Unless zero staleness is a hard, demonstrable requirement, this is the answer.
The complexity of the other approaches is not free — more moving parts means
more failure modes, harder debugging, and operational overhead that must be
justified by the staleness requirement.

**2. Lock stack + lease, pipelined (M2b+M3+M7+M8+M9+M10+M13, with Lua batching).**

(M5 is kept only on the writer's SET; readers populate through the M13 lease —
see §17 Option E. Versus lock stack (M5): write cost unchanged and no extra read
round trip — the lease grant is one more SET inside the readCheck Lua.)

Zero staleness with cache that actually works during writes. Write cost matches
CAS (both ~2n_r + n_d + u). Readers during writes get cache hits (n_r + p via
M8, or 3/2 n_r + 1/2 n_d + 1/2 u + S average for blocked readers via M9)
instead of DB fallbacks. On pure per-reader latency during a write this is
roughly a tie with CAS at n_r ≈ n_d (§16) — the win comes from DB load, M8,
the post-write cache, and fresh data.

The pipelining reduces 5 Redis RTs to 2 by batching:
- Pre-DB: Lua script (SETNX + GET + DEL) → 1 RT
- Post-DB: Lua script (version-compare SET + DEL lock) → 1 RT

This makes the write path network cost match CAS while retaining all the lock
stack's reader-side benefits. The implementation complexity is higher (Lua
scripts must be correct, deployed, and maintained), but the write-path
network saving (3 fewer Redis round trips) is clear.

Best when: zero staleness required AND read traffic during writes is
non-trivial AND you can invest in the Lua script implementation.

**3. Lock stack + lease, naive / no pipelining (same mechanisms, sequential commands).**

Same zero-staleness guarantee, same reader benefits. Write path costs 3n_r
more than pipelined (5 Redis RTs vs 2). Each extra RT is a network round trip
to Redis — at same-DC (n_r ~0.1-0.5ms), that's ~0.3-1.5ms extra; at cross-DC,
it compounds.

Simpler to implement than the pipelined variant: just sequential Redis commands,
no Lua batching for the pre-DB phase. The post-DB Lua (version-compare SET,
M5) is still needed for correctness, but the pre-DB commands can be plain
SETNX → GET → DEL.

Latency vs CAS (from §16, with MGET and poll slack):
- Extra write cost over CAS: ~3n_r
- Per-reader saving over CAS: ~(n_d + s − 1/2 n_r − S)
- Breakeven: **R > 3n_r / (n_d + s − 1/2 n_r − S)**
- n_r ≪ n_d: R ≈ 1. n_r ≈ n_d: the denominator is small, so R can be large
  (10+) — naive lock is slower than CAS on latency at typical read rates.
  n_r ≫ n_d: never.

Best when: want the lock stack's guarantees without Lua pipelining complexity,
Redis is close relative to the DB, or latency during writes is not the deciding
factor (correctness, DB load, and post-write cache still favour it over CAS).

**4. CAS (M6).**

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

- **Hidden requirements.** "INCR + SET, medium complexity" undersells it.
  Correct CAS also needs a `row_version` column with a conditional DB update
  (INCR order ≠ commit order), lost-update handling, `MGET` on reads, a reader
  fill rule based on the DB row's version, and protection against the counter
  going backwards. That is close to the lock stack's machinery, while still
  keeping P4.

On per-reader latency during a write, CAS (with MGET) is competitive:
- vs naive lock stack: CAS wins unless n_d + s is well above 1/2 n_r + S and
  enough readers arrive during writes (§16 breakeven)
- vs pipelined lock stack: roughly a tie at n_r ≈ n_d; CAS wins when n_r ≫ n_d
  or on localhost; lock wins clearly when n_r ≪ n_d

So CAS's weaknesses are not latency — they are DB load (every reader during a
write hits DB), P4 under sustained writes, the wasted MVCC trip, sustained
failure impact, and the hidden requirements above.

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
| 3 | Lock stack naive | None | Works (write-through) | 5 RTs | cache hit after wait (correct) | Zero stale, simpler implementation, Redis close |
| 4 | CAS | None | Broken (DB fallback) | 2 RTs | DB round trip (wasted if MVCC) — latency competitive, DB load high | Zero stale, no lock (but its own complexity), low writes |

### Does MVCC change the ranking?

With MVCC (plain SELECT in InnoDB REPEATABLE READ / READ COMMITTED), the CAS
reader's DB SELECT doesn't block behind the writer's row lock. This removes the
1/2(n_d + u) wait term from CAS reader cost.

**Per-reader latency comparison during a write:**

```
CAS reader (MVCC, MGET, no wait): (n_r + p) + (n_d + s)
                                  = n_r + p + n_d + s

Lock stack reader (blocked avg):  1/2(n_d + u + n_r + l) + S + (n_r + l)
                                  = 3/2 n_r + 3/2 l + 1/2 n_d + 1/2 u + S
```

Difference (CAS − Lock stack):
= 1/2 n_d + s − 1/2 n_r − 1/2 u − S + p − 3/2 l

CAS reader is faster when this is negative (dropping small p, l):
**n_d + 2s < n_r + u + 2S**

This holds whenever DB UPDATE processing (u) dominates — which it usually
does when fsync is the bottleneck — and more easily when S is non-trivial.
It fails when n_d is large relative to n_r + u (DB far away, Redis close).

**Example** (illustrative — benchmark your own values): same DC, n_r ≈ n_d ≈
0.3, s ≈ 0.3, u ≈ 2 (ms): CAS reader ~0.9ms vs blocked lock stack reader ~2ms.
CAS is roughly 1ms faster per blocked reader.

**Why this doesn't change the ranking — three reasons:**

**1. M8 covers many lock stack readers for free.**

The lock stack reader formula above models the blocked case (cache miss after
DEL, waiting for M9 write-through). But M8 (serve-if-exists) means readers
arriving before DEL cache or after Lua SET cache (but before DEL lock) get a
cache hit at n_r + p with zero blocking.

```
Lock stack reader timing windows during write:
  [SETNX → DEL cache]       cache exists → M8 hit: n_r + p
  [DEL cache → Lua SET]     cache empty → blocked: 3/2 n_r + 1/2 n_d + 1/2 u + S avg
  [Lua SET → DEL lock]      cache exists (new data) → M8 hit: n_r + p
```

The blocked window (n_d + u) is the majority of the lock time, but a reader in
an M8 window pays n_r + p — less than CAS's n_r + n_d + s.

**2. After the write, lock stack cache is populated — CAS isn't.**

M9 (write-through inside lock) means the cache has new correct data before
unlock. Every reader after the write gets n_r + p. With CAS, the SET cache
hasn't happened yet (it's the last step). Readers between DB commit and SET
cache still see version mismatch → still fall through to DB (the first of them
can refill via the row_version == counter rule, but each one before that pays
a DB read). During heavy
writes, subsequent write INCRs keep pushing the version counter ahead — the
cache may never serve a hit.

The per-reader advantage during one write doesn't compound — CAS's saving is
one-time per write, while the lock stack's post-write cache benefits all future
readers until the next write.

**3. CAS's faster trip was wasted — it returned redundant data, and cost DB load.**

With MVCC, the CAS reader's DB SELECT returns the last committed version —
v(N), the same data the cache had. The writer's v(N+1) isn't committed yet.
The CAS reader paid n_r + n_d + s to confirm what the cache already knew, and
put one query on the DB per reader. The lock stack reader waited longer but got
v(N+1) from cache, with zero DB queries.

```
CAS reader during write (MVCC):
  MGET → mismatch → DB SELECT → v(N) (same as cache) → wasted trip, 1 DB query
Lock stack reader during write (blocked):
  Wait for lock → readCheck → v(N+1) (new data from M9) → fresh, 0 DB queries
```

**Conclusion:** At low DB load, with MVCC and MGET, a CAS reader caught in a
write is individually faster than a blocked lock stack reader. That advantage
disappears as readers scale: every CAS reader is a DB query, DB latency rises
with load (s, and u for writers), while lock stack readers add zero DB load and
their cost stays flat (§16 caveat). Add M8 hits, a populated cache after every
write, and fresh data instead of a redundant trip, and the ranking is
unchanged. Keeping S small (short first poll interval) narrows the low-load
gap.

---

## 19. Best Fit by Workload

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
| High read, high write, ms stale OK | M1+M10+M4 | ms stale self-healing, simple | Lock stack + lease | Correct but heavy — zero stale isn't needed here | M6 CAS | Cache broken under sustained writes, DB hammered |
| High read, high write, zero stale | Lock stack + lease | Zero stale + cache works + no stampede | M6 CAS | Zero stale, no lock (but its own complexity), cache degrades under writes | M11 write-through no lock | P7 = long stale, opposite of zero |
| Zero stale, no lock | **M6 CAS** | Only zero-stale without locks (still needs conditional update, MGET, row-version fill) | M1+M10+M4 | ms stale, not zero — but no lock and simplest | Lock stack + lease | Has locks (unwanted) |
| High write concurrency, same key | **No ideal** | Lock stack + lease: writer serialization. CAS: cache broken. Real tradeoff. | Lock stack + lease | Correct and cache works; writers slower (serialized) | M11 no lock | P7 race + no protection |
| Redis outage tolerance | M1+M10+M4 | Writes don't depend on Redis | Lock stack + lease with fallback | Degrades to Simple when Redis is down | M6 CAS | INCR before DB = writes blocked |

---

## 20. Summary

### The simple approach (M1+M10+M4: DB→delete + version-in-key)

- ms stale window, self-healing
- No Redis write dependency
- No lock overhead, no complexity
- Best for: most systems, especially high-read-low-write

### Lock stack + lease (M2b+M3+M7+M8+M9+M10+M13) — chosen for zero stale

(M5 version compare kept on the writer's write-through only.)

- Zero stale reads — M13 closes the last gap (slow reader refilling the empty
  slot after a failed writer, §17 Option E)
- Cache works under write pressure
- No DB stampede — write-through (M9) after writes, one lease holder per cold miss
- Zero reader DB load during writes; per-reader cost stays flat as readers scale
- Degrades gracefully to simple approach on Redis failure
- Best for: high-read-high-write with zero-staleness requirement

### CAS (M6)

- Zero stale reads — the only single mechanism that handles both P1 and P2
  without any lock
- CAS is really version-in-value (M5) with read-path enforcement: M5 compares
  on cache writes only (cold path), CAS compares on every read (hot path)
- Orphaned versions on failure — but self-healing on next successful write, not
  permanent
- Cache degrades under write pressure (constant version mismatch → DB fallback)
- Every reader caught in a write is a DB query — DB load and latency grow with
  readers
- Needs more than it looks to be correct: conditional DB update, one `MGET`,
  reader fill by the DB row's `row_version`, counter protection (§13 M6)

**When CAS is the right choice:** zero-staleness required, no lock wanted, and
low write pressure. Under low write pressure, versions mostly match → cache
hits, and the DB load from mismatches stays small. Note it is not "simple" once
done correctly — the hidden requirements bring it close to the lock stack's
machinery.

**When CAS is not the right choice:** high write pressure (cache broken, DB
hammered), many readers per write (every one hits the DB), or when combined
with other mechanisms (a lock makes CAS's read-path compare redundant).

### The tradeoff landscape

```
                    Zero staleness
                         ▲
                         │
         M6 CAS ─────────┤──────── Lock stack + lease
         (no lock,       │         (complex,
          hidden reqs,   │          cache works,
          cache degrades │          DB protected)
          under writes)  │
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
- For zero-stale without a lock and with low write pressure, CAS.
- For zero-stale with a cache that works under pressure and scales with readers,
  the lock stack + lease — its overhead is minor compared to
  the DB write that dominates the path regardless.

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
- Lock stack + lease: readCheck touches the cache key and the lock key →
  `{user:<id>}` and `{user:<id>}:lock`
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
- If zero stale is required, the chosen design is the lock stack + lease: M2b+M3+M7+M8+M9+M10+M13 (M5 kept on the writer's write-through only). See §17 Option E.
