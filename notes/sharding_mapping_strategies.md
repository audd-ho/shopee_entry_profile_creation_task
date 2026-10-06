# Key → Shard Mapping Strategies (Partitioning & Rebalancing)

Learnings from a deep-dive on `internal/sharding/ring.go`: how consistent
hashing is really implemented, how migration works, and how it compares to
fixed buckets and stored ranges.

**What to call these:** they are **partitioning schemes** (a.k.a. key-to-shard
mapping / rebalancing strategies — DDIA ch.6 terminology). They are not new
"sharding methods" alongside hash/range/directory; they are the different ways
of implementing the *mapping step* once a key has been hashed (or taken raw),
and they blur the line between "hash-based" and "directory-based".

---

## 1. The Unifying Model

Every scheme is the same two steps:

```
step 1: key ──hash──▶ point in some space      (2^64, or % 1024, % 16384 ...)
step 2: point ──map──▶ shard                    ← ALL the differences live here
```

Step 2 has two questions:
1. **Where are range boundaries?** (computed / random / fixed / chosen)
2. **Who owns each range, and is that stored or computed?**

| Scheme | Boundaries | Owners | Shared state | Lookup |
|---|---|---|---|---|
| `hash % N` | Fixed by N | Fixed by N | N | O(1) |
| Hashed vnode ring (our `ring.go`) | Computed: `hash(shard, i)` | Computed | Shard list only | O(log V) binary search |
| Cassandra ring (random / allocated tokens) | Random or algorithm-chosen | Stored per node, gossiped | Token table | O(log V) |
| Fixed buckets (Redis slots, "1000 tables") | Fixed forever (2^64/Q slices) | Stored, movable | Q-entry array | O(1) array index |
| Stored ranges (Vitess, CockroachDB, HBase, Spanner) | Stored, splittable anywhere | Stored, movable | Range table, changes often | O(log R) binary search |

Fixed buckets are just stored ranges with boundaries restricted to multiples of
2^64/Q. The vnode ring is just a range table whose boundaries fall wherever the
hash puts them.

---

## 2. Initial Misconceptions (Corrected)

| Thought | Reality |
|---|---|
| "You pick a ring range like 0–1000 or 0–10,000,000." | No range is picked. The ring **is** the hash's output space: 2^64 for a 64-bit hash, wrapping naturally. Small numbers are only for diagrams. |
| "How do you 'walk clockwise'?" | Sort vnode hashes; binary-search for the first vnode hash ≥ key hash ("round up"); if past the end, wrap to index 0. |
| "Vnode positions are random." | Pseudo-random but **deterministic**: `hash(shard, i)` is identical on every server and restart. Required — every app server builds the ring independently and must agree. Truly random positions would route the same user to different shards. |
| "Shard index as identity breaks on removal." | Only if you **renumber**. Removing shard 2 and keeping 0,1,3,4 is fine — a stable number is as good as a stable name. Our current `NewRing(shardCount, …)` API just can't express gaps; it would need `NewRing(shardIDs []int, …)`. |
| "A shard going down = remove it from the ring." | For a DB, no. Down → replica failover, ring untouched. Removal is a planned decommission with data migration. (For a cache, removal is fine — misses just refill.) |
| "Bucket table lookup is a second hash (hashtable)." | It's a plain **array**: bucket ids are dense 0..Q-1, so `table[bucket]` is a direct index. No hashing, no collisions. Only one hash in the whole path. |
| "Prod doesn't use consistent hashing for DBs." | Self-sharding DBs (Cassandra, DynamoDB, Riak, Scylla) do. App-level sharding over MySQL/Postgres usually uses fixed buckets. Both scale; they just scale differently. |
| "Computed/random placement means nothing is stored." | Only true for **deterministic** placement like `hash(shard, i)` — then only the shard list is stored. Cassandra's random tokens and its 4.0 allocated tokens depend on state at join time → stored and gossiped. And even our ring needs all servers to agree on the shard list. |
| "Range-map lookup O(log n) is the cost of fine granularity." | ~10–15 comparisons = nanoseconds vs milliseconds for the DB call. Irrelevant. The real cost is operational machinery (§6). |
| "Fine granularity = millions of ranges." | 2^64 controls *where* you can cut, not *how many* cuts. Range count follows data size (CockroachDB splits at ~512 MB → 10 TB ≈ 20k ranges). Each range carries overhead, so they're kept big on purpose. |

---

## 3. The Hashed Vnode Ring (our `ring.go`)

```go
// build: for each shard s, for each v in 0..149
h := xxhash(s || v)            // deterministic point in 2^64
vnodes = append(vnodes, {h, s})
sort(vnodes by h)

// lookup
h := xxhash(key)
idx := first i where vnodes[i].hash >= h   // binary search
if idx == len(vnodes) { idx = 0 }          // wrap
return vnodes[idx].shard
```

**Why vnodes (150 per shard):**
- One point per shard → arc sizes vary wildly (one shard might own 40%).
  With V points per shard, imbalance shrinks ~1/√V; 150 → roughly ±10%.
- Adding a shard: its 150 points steal small slivers from *every* shard, not a
  big chunk from one neighbour. Removing: its arcs spread over many successors.

**Why bother vs `hash % N`:** going N → N+1 with modulo moves ~all keys; the
ring moves only ~1/(N+1). If the shard count never changes, modulo is equally
fine.

**64-bit collisions** between vnode hashes: negligible.

**Related schemes:**
- **Jump consistent hash** (Google 2014): ~5-line function, zero memory, minimal
  movement, but you can only add/remove the *last* bucket.
- **Rendezvous / HRW:** pick the shard with the highest `hash(key, shard)`.
  O(N) per lookup, no vnodes.
- **Maglev:** lookup-table hashing for load balancers.

---

## 4. Migration — The Ring Never Moves Data

The ring is only a function `key → shard`. A ring change tells you **which**
keys now belong elsewhere; moving them is a separate process you build.

### 4.1 Down vs Removed

- **Cache node dies:** drop it from the ring; its keys miss and refill from DB.
  This is what consistent hashing was invented for.
- **DB shard goes down:** do **not** touch the ring. Replication + failover
  (MHA/Orchestrator, Patroni) promotes a replica; "shard 3" now points at a new
  host. No replica/backup → data gone; the ring can't help.
- **DB shard decommissioned:** planned migration (reverse of adding). Each of
  its arcs passes to the next vnode clockwise.

### 4.2 Adding a Shard — Two Rings + Phases

Keep `old` and `new` rings. A key moves iff `old.ShardFor(k) != new.ShardFor(k)`.
For each new vnode, the moving arc is `(predecessor, newVnode]`, previously
owned by the next vnode clockwise — so you know exactly where each key comes from.

Rule: **exactly one source of truth at any moment.**

| Phase | Writes | Reads | Source of truth |
|---|---|---|---|
| Prepare | old | old | old (new ring published as pending, v2) |
| Backfill | old + copy to new (dual-write or CDC/binlog replay) | old | old |
| Verify | same | old (optionally shadow-compare new) | old |
| Flip | **new** only, after a brief write pause to drain | new, fall back to old on miss | new |
| Cleanup | new | new | new (delete moved rows from old, drop old ring) |

- **Dual-write** keeps the new copy current so cutover is instant.
- **Read fallback** covers rows not yet copied (or lazy copy-on-read migration).
- Writing to both with neither authoritative → duplicates / lost updates.
- Store the key hash as an indexed column → backfill is a range query, not a scan.
- **Redis Cluster** does exactly this: slot marked `MIGRATING` (source) /
  `IMPORTING` (target); source answers `ASK` → client retries on target.
- In code: `Ring` being immutable fits — a router holds `current`, `next`, and a
  phase flag.

### 4.3 The Uniqueness Gotcha (username lookup)

Same mechanism, higher stakes:
- Normal read: a false miss is harmless (fall back, slightly slower).
- Uniqueness check: a false miss **creates a duplicate**. Servers switch rings at
  different moments → server A (old ring) inserts `alice` on shard 1, server B
  (new ring) checks shard 5, sees nothing, inserts `alice` on shard 5. Each DB's
  `UNIQUE` only covers its own rows.

Fix: check both owners **and** ensure only one shard accepts writes for a key at
any moment, so one DB's unique constraint enforces it. Applies to every
secondary-key lookup.

---

## 5. Fixed Buckets (Logical Shards + Mapping Table)

```
bucket = xxhash(user_id) % 1024     ← only hash; never changes
shard  = bucketTable[bucket]        ← plain array; this is what you edit
```

- A key's bucket never changes; rebalancing = move whole buckets, update a row.
- Same minimal-movement benefit as the ring, simpler bookkeeping.
- Used by: Redis Cluster (16384 slots), Shopee-style `user_id % 1000` tables
  spread over N DBs, Citus; Vitess is the range variant (§6).

**Why it usually wins for app-level MySQL sharding:**
1. **Clean migration unit** — `bucket_id` column or table-per-bucket (`user_0042`);
   state is simple: `bucket 42: migrating 3 → 7`.
2. **Placement control** — move a hot bucket, weight bigger machines, drain a host.
   A hash ring can't express "give shard 7 less load".
3. **Small versioned routing state** — 1024 entries in etcd/config with a version;
   servers can verify they agree (exactly what §4.3 needs).
4. **Hash results never change** — caches, analytics, other systems computing
   `bucket(user_id)` stay valid forever.

**Downsides:**
1. **Granularity fixed on day one; a bucket can't be split.** Rule of thumb:
   buckets ≥ 10–100× max shards. 1024 on 1000 shards → some shards hold 2× others.
   1024 on 10 shards → 102/103 each, ~1% imbalance — fine.
2. **Changing Q = rehash everything** — unless Q is a power of two: `% 1024 → % 2048`
   splits bucket `b` into `b` and `b+1024`, locally per shard.
3. **Mapping must be stored and kept consistent** across all servers.
4. **Placement needs thought/automation** — but with a good hash buckets are
   ~equal size, so "spread evenly" usually suffices.
5. **Granularity has practical caps** — table-per-bucket makes 16k tables heavy;
   Redis capped at 16384 partly because nodes gossip a slot bitmap (16384 bits = 2 KB).

Neither ring nor buckets fix a single **hot key** (celebrity user) — one key
always maps to one place. Needs caching or splitting that key's data.

**Supporting evidence:** the original Dynamo paper (2007) compared placement
strategies and found best balance with "Q fixed equal partitions assigned to
nodes" — i.e. buckets.

---

## 6. Stored Ranges (Hash → 2^64 → Range Table)

The "best of both worlds" idea: hash into 2^64, split into ranges anywhere, store
`range → shard`. This **is** Vitess's design.

- Gains: cut boundaries exactly where heat/size is — no granularity ceiling.
- Lookup O(log R) — irrelevant cost.
- Real cost — machinery:
  - Split/merge logic + tracking size and load per range.
  - Uneven range sizes → balancing must weigh, not count.
  - Range table changes often → clients' cached tables go stale → shards must
    reject "not my range" and clients retry (CockroachDB, HBase do this).
  - A strongly-consistent metadata service (CockroachDB stores it in system ranges).
  - Moving data (not updating the table) is the true expense → systems throttle
    rebalancing and only move when imbalance is worth the copy.

**Hash-then-range vs raw-key range:**
- Vitess hashes first → even load, but range scans hit every shard.
- CockroachDB / Spanner / HBase usually range over the **raw key** → sorted
  order, efficient `BETWEEN` scans, but sequential keys (auto-increment,
  timestamps) pile onto the last range → hence automatic splitting.

---

## 7. Cassandra — Yes, a Consistent Hash Ring

- Token = ring position; Murmur3 over −2^63..2^63−1; key → next token clockwise;
  replicas = next RF distinct nodes clockwise (rack/DC-aware variants).
- **Pre-1.2:** one token per node, hand-set `initial_token`, evenly spaced
  (`i × 2^64/N`). Adding nodes = compute tokens + `nodetool move`; often doubled
  the cluster to bisect ranges. (What "fixed tokens" loosely meant.)
- **1.2 (2013):** `num_tokens: 256`, random vnodes — same idea as `ring.go`.
- **3.0 / 4.0:** 256 random vnodes hurt availability (with RF=3, any 2 nodes down
  likely kills quorum for *some* range), repair/streaming overhead, and still
  imbalance. 4.0 defaults: `num_tokens: 16` + `allocate_tokens_for_local_replication_factor: 3`
  → new node's tokens computed to balance given existing tokens.
- Still not buckets: adding a node creates new boundaries rather than reassigning
  fixed ones. Stays a ring partly for history, partly because it's masterless —
  each node announces its own tokens; no central table.

---

## 8. The Core Trade-off: Control vs Machinery

| | Control | Shared state | Machinery |
|---|---|---|---|
| Hashed ring | None (automatic) | Shard list | Least |
| Fixed buckets | Coarse (1/Q) | Small array, rarely changes | Moderate |
| Stored ranges | Fine (cut anywhere) | Range table, changes often | Most |

- **Ring:** routing is *computed* → automatic rebalance, no central store, any node
  count. But no control: can't target a hot range, and migration moves scattered
  slivers you didn't choose. Routing update is trivial; the data movement is messy.
- **Buckets:** routing is *stored* → need a versioned, agreed-upon table and a
  placement plan. In return: choose exactly which buckets move and from where;
  targeted 1/Q moves (one hot bucket) on top of 1/shard_count scaling moves.
- **Stored ranges:** buckets with unlimited granularity → most control, most
  infra (split logic, metadata service, stale-route retries, throttled moves).
- **If you're storing the mapping anyway, use buckets or ranges** — you've paid
  the storage/consistency cost, so take the control.

| Situation | Fit |
|---|---|
| Cache, CDN, LB — nodes churn, data loss OK | Hashed ring |
| Masterless peer DB (Cassandra-style) | Ring with stored/allocated tokens |
| MySQL/Postgres sharded by app or proxy | **Fixed buckets + mapping table** |
| Building the distributed DB itself | Stored ranges |

---

## 9. For This Project

- `ring.go` as written is fine for a fixed/append-only shard count.
- To support removal: `NewRing(shardIDs []int, …)` and a shard-ID → connection map
  that tolerates gaps.
- Design-doc line worth adding: *"For operational resharding we'd move to fixed
  logical buckets (power-of-two count) + a versioned mapping table, migrated via
  backfill → dual-write → flip → cleanup, with single-writer ownership to keep
  username uniqueness."*
