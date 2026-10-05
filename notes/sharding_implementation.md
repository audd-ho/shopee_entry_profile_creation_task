# Sharding Implementation — Checklist & Guide

Practical implementation guide for the sharding design in `sharding_design.md`.
Covers what to build, in what order, what to watch for, and how to verify.

---

## 1. Prerequisites

### 1.1 Rebuild and Restart Servers

The tcp-server binary running on port 9000 is from Sep 22 and does not match
current source. **Rebuild and restart both servers before any sharding work.**

```bash
go build -o bin/tcp-server ./cmd/tcp-server
go build -o bin/http-server ./cmd/http-server

# Kill stale processes
kill <old_tcp_pid> <old_http_pid>

# Restart
./bin/tcp-server &
./bin/http-server &
```

### 1.2 Verify Current Tests Pass

Before changing anything, make sure the existing single-instance code works:
```bash
go test ./...
```

Fix any failures before starting sharding work — otherwise you won't know
whether a test failure is a sharding bug or a pre-existing issue.

---

## 2. MySQL Schema Setup

### 2.1 No Extra MySQL Instances Needed

All shards are separate databases on the same mysqld. One process, multiple
schemas. The Go code connects to each with a different DSN (different database
name, same host:port).

### 2.2 Create Databases

```sql
-- Lookup shards (2)
CREATE DATABASE IF NOT EXISTS lookup_0
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE DATABASE IF NOT EXISTS lookup_1
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Data shards (start with 2, add data_2 later for the scaling demo)
CREATE DATABASE IF NOT EXISTS data_0
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE DATABASE IF NOT EXISTS data_1
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

### 2.3 Create Tables in Each

**Each lookup shard:**
```sql
-- Run in lookup_0 AND lookup_1
CREATE TABLE username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

**Each data shard:**
```sql
-- Run in data_0 AND data_1 (and later data_2)
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,       -- snowflake, no AUTO_INCREMENT
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,
  nickname       VARCHAR(128) NOT NULL,
  avatar_suffix  CHAR(8)      NULL,
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

### 2.4 Automation

Consider writing a `cmd/setup-shards/main.go` or a `scripts/setup_shards.sql`
that creates all databases and tables idempotently. Run it before the seeder
and before tests.

### 2.5 The Existing `userdb` Database

The current single-instance `userdb` with 10M rows stays untouched during
development. The sharded schemas are new, separate databases. Once sharding
is working and verified, the old `userdb` becomes unused.

---

## 3. Build Order — Dependency Chain

Build in this order. Each step depends on the ones above it.

### Step 1: Snowflake ID Generator

**New package:** `internal/idgen`

**What it does:**
- Holds `machineID` (from config), `lastMs`, `sequence`, `mu sync.Mutex`.
- `Next() uint64` — generates a 64-bit snowflake ID.
- Uses a custom epoch (the service launch date, stored as a constant).

**Why first:** `InsertUser` needs it before `AUTO_INCREMENT` goes away. Everything
downstream (shard routing, seeder) depends on having real snowflake IDs.

**Test:** generate 100k IDs in a loop, verify all unique, verify time-sorted
(each ≥ previous), verify machine bits are correct.

**Gotcha — clock skew:** if `time.Now()` returns a millisecond earlier than
`lastMs` (NTP correction, VM migration), either refuse to generate (fail-fast)
or use `lastMs` until the clock catches up. The fail-fast approach is simpler
and safer for an entry task.

### Step 2: Consistent Hash Ring

**New package:** `internal/sharding` (or add to `internal/db`)

**What it does:**
- Holds a sorted slice of `(hash, shardIndex)` pairs (vnodes).
- `ShardFor(userID uint64) int` — hash the ID, binary search for the first
  vnode ≥ hash, return its shard index.
- `NewRing(shardCount int, vnodesPerShard int)` — builds the ring.

**Considerations:**
- Use a good hash function (FNV-1a, xxhash, or even `crc32` — anything uniform).
  Don't use `userID % N` — that defeats the purpose of consistent hashing.
- For the lookup shards, a separate simpler router is fine: `hash(username) % 2`.
  No need for consistent hashing on the lookup side since the lookup shard count
  is fixed for this demo.

**Test:** create a ring with 2 shards, verify distribution is roughly 50/50 over
10k random IDs. Add a third shard, verify ~1/3 of the 10k IDs changed shards
(not ~2/3).

### Step 3: Shard-Aware DB Handle

**Changes to:** `internal/db/users.go` (and potentially a new `internal/db/shard.go`)

**What it does:**
- Replaces the single `Handle = *Pool` with a shard-aware type:
  ```go
  type ShardedHandle struct {
      dataShards   []*Pool          // one pool per data shard
      lookupShards []*Pool          // one pool per lookup shard
      ring         *sharding.Ring   // consistent hash for data routing
  }
  ```
- `ShardFor(userID uint64) *Pool` — consistent hash → pool index.
- `LookupShardFor(username string) *Pool` — `hash(username) % len(lookupShards)`.

**Migration path for existing code:**
- Option A: replace `Handle` entirely with `ShardedHandle`. All query helpers
  take `ShardedHandle`. Bigger change, cleaner result.
- Option B: keep `Handle = *Pool` for individual query helpers (they still
  operate on one pool), and add `ShardedHandle` as the top-level type that
  the handlers use to pick the right pool before calling helpers. Smaller diff.

Option B is probably easier — the query functions (`GetUserByID`,
`UpdateNickname`, etc.) don't need to know about sharding. They take a pool and
run a query. The handler decides which pool to pass.

**Gotcha — prepared statements:** each pool has its own connections with their own
statement caches. The same query text gets prepared separately on each shard's
connections. No change needed in `pool.go` / `conn.go`, but the total number of
prepared statements across the system multiplies by shard count. Not a problem
at this scale.

### Step 4: Lookup Helpers + Cache

**New file:** `internal/db/lookup.go`

**Functions:**
```go
func InsertLookup(ctx, lookupPool *Pool, username string, userID uint64) error
func GetLookup(ctx, lookupPool *Pool, username string) (uint64, error)
```

`InsertLookup` returns `ErrDuplicateUsername` on PK collision (same mechanism
as the existing `isDuplicateKey` check).

**New cache helpers:** `internal/cache/lookup.go`

```go
func (c *Client) GetLookupCache(ctx, username) (uint64, bool, error)
func (c *Client) SetLookupCache(ctx, username, userID) error
```

Key: `lookup:<username>`, value: `{"user_id": 12345}`, TTL: 10 min.
Write-once data — no versioning, no invalidation needed (a username maps to one
user_id forever). SET on signup (after successful insert), populated on login
cache miss.

### Step 5: Update InsertUser

**Changes to:** `internal/db/users.go`

Before:
```go
func InsertUser(ctx, h Handle, username string, hash []byte, nickname string) (int64, error)
```

After:
```go
func InsertUser(ctx, h *Pool, id uint64, username string, hash []byte, nickname string) error
```

- Takes `id` as a parameter (snowflake, generated by caller).
- Takes `*Pool` (the specific shard pool, chosen by caller).
- No `LastInsertId()` — the ID is already known.
- The SQL gains an `id` placeholder:
  ```sql
  INSERT INTO users (id, username, password_hash, nickname) VALUES (?, ?, ?, ?)
  ```

### Step 6: Update Signup Handler

**Changes to:** `internal/tcpserver/auth.go` → `handleSignup`

New flow:
```go
1. Validate input (unchanged)
2. bcrypt.Hash (unchanged)
3. id := s.idgen.Next()                              // NEW
4. lookupPool := s.db.LookupShardFor(username)        // NEW
5. InsertLookup(ctx, lookupPool, username, id)         // NEW — catches duplicates
6. dataPool := s.db.ShardFor(id)                       // NEW
7. InsertUser(ctx, dataPool, id, username, hash, nick) // CHANGED — takes id
8. If step 7 fails: DeleteLookup(ctx, lookupPool, username)  // NEW — rollback
9. [background] SetLookupCache(ctx, username, id)      // NEW
```

**Gotcha — rollback:** if the data-shard INSERT fails after the lookup INSERT
succeeded, the lookup reservation must be deleted. Use explicit error handling
(not defer — defer runs on all exits, but you only want rollback on failure).

### Step 7: Update Login Handler

**Changes to:** `internal/tcpserver/auth.go` → `lookupUserForLogin`

New flow (prepended to existing logic):
```go
1. cache.GetLookupCache(ctx, username)                 // NEW
   HIT  → have userID
   MISS → lookupPool := s.db.LookupShardFor(username)  // NEW
          GetLookup(ctx, lookupPool, username)           // NEW
          → not found? return ErrUserNotFound
          → found? SetLookupCache, have userID

2. dataPool := s.db.ShardFor(userID)                    // NEW

3. Existing auth cache + DB logic, but using dataPool instead of s.db
```

The rest of the login flow (bcrypt verify, session creation) is unchanged.

### Step 8: Update Post-Login Handlers

**Changes to:** `internal/tcpserver/auth.go` → `handleGetUser`,
`internal/tcpserver/profile.go` → `handleUpdateNickname`, `handleUpdateAvatar`

Minimal change — add shard routing before the DB call:
```go
session, _ := s.resolveSession(ctx, d.Token)
dataPool := s.db.ShardFor(session.UserID)  // NEW — one line
// ... rest unchanged, but pass dataPool instead of s.db to query helpers
```

### Step 9: Update Config

**Changes to:** `internal/config/config.go`

New config fields:
```go
type MySQLConfig struct {
    // Single-instance (kept for backward compat / non-sharded mode)
    DSN     string
    MaxOpen int
    MaxWait time.Duration

    // Sharded mode
    DataShardDSNs   []string  // e.g. from DATA_SHARD_DSNS="dsn0,dsn1"
    LookupShardDSNs []string  // e.g. from LOOKUP_SHARD_DSNS="dsn0,dsn1"
}

type IDGenConfig struct {
    MachineID int  // 0-1023, from MACHINE_ID env var
}
```

**`cmd/tcp-server/main.go`:** open multiple pools (one per DSN), build the
`ShardedHandle`, pass it to `NewServer`.

### Step 10: Update Seeder

**Changes to:** `cmd/seed/main.go`

The current seeder bulk-inserts into one `userdb` with AUTO_INCREMENT. The
sharded seeder needs to:

1. Generate snowflake IDs for each user (or a deterministic equivalent —
   sequential IDs are fine for seed data since the consistent hash doesn't
   care about time-sorting).
2. Hash each ID to a data shard: `shard = ring.ShardFor(id)`.
3. Hash each username to a lookup shard: `lookupShard = hash(username) % 2`.
4. Batch inserts per-shard (can't multi-row INSERT across shards).
5. Insert into both the lookup shard AND the data shard for each user.

**Approach:** collect rows into per-shard buffers, flush each buffer as a
multi-row INSERT when it reaches batch size. At the end, flush remaining
partial batches.

**Gotcha:** the shared bcrypt hash optimization still works — all seeded users
share one pre-computed hash. But the INSERT batches are now per-shard, so a
batch of 1000 may split into ~500 per shard (roughly even with 2 shards). The
multi-row INSERT is still within each shard, just smaller per-shard.

**Gotcha:** if using deterministic sequential IDs for seeding (1, 2, 3, ...
instead of snowflake), the consistent hash distribution still works — it hashes
the ID, it doesn't care what the input looks like. But if you later want to
verify "would the migration from 2→3 shards move the right users," the seed
IDs should match what the production flow would generate. Consider using the
snowflake generator for seeding too, with a fixed machineID=0.

---

## 4. The 2 → 3 Migration Demo

### 4.1 Setup

After steps 1-10 are working with 2 data shards:

```sql
CREATE DATABASE IF NOT EXISTS data_2
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Same users table schema as data_0 and data_1
CREATE TABLE data_2.users ( ... );
```

### 4.2 Update the Consistent Hash Ring

The ring needs to know about 3 shards instead of 2. This can be:
- A config change: `DATA_SHARD_DSNS="dsn0,dsn1,dsn2"` → restart.
- Or a runtime update if you build hot-reload (probably overkill for the demo).

### 4.3 Migration Phases

**Phase 1 — Dual-write, read-old:**
```
Config: MIGRATION_PHASE=dual_write_read_old
        OLD_SHARD_COUNT=2
        NEW_SHARD_COUNT=3

For each write:
  old_shard = ShardFor_v1(userID)  // 2-shard ring
  new_shard = ShardFor_v2(userID)  // 3-shard ring
  Write to both (old first, then new)
  If old == new: effectively one write (harmless duplicate)

For each read:
  Read from old_shard only (ShardFor_v1)
```

**Phase 2 — Backfill:**

A script/job that:
1. Iterates all rows in data_0 and data_1.
2. For each row: compute `ShardFor_v2(id)`.
3. If `ShardFor_v2(id) != current_shard_index` → upsert into the correct shard.
4. Idempotent (INSERT ... ON DUPLICATE KEY UPDATE or just INSERT IGNORE).

```bash
go run ./cmd/migrate-shards \
  --old-shards "data_0_dsn,data_1_dsn" \
  --new-shards "data_0_dsn,data_1_dsn,data_2_dsn" \
  --batch 1000
```

**Phase 3 — Dual-write, read-new:**
```
Config: MIGRATION_PHASE=dual_write_read_new

For each write: still write to both
For each read:  Read from new_shard (ShardFor_v2)
```

Grayscale: route a percentage of reads to the new ring. Verify correctness
before going to 100%.

**Phase 4 — Cutover:**
```
Config: MIGRATION_PHASE= (empty / removed)

ShardFor uses the 3-shard ring only.
Stop writing to old locations for moved users.
Optionally: delete migrated rows from their old shards to reclaim space.
```

### 4.4 Verification Script

A script that verifies every row is on the correct shard:

```bash
go run ./cmd/verify-shards --shards "data_0_dsn,data_1_dsn,data_2_dsn"
```

For each shard:
1. `SELECT id FROM users` (paginated).
2. For each id: `assert ShardFor(id) == this_shard_index`.
3. Report misplaced rows (should be zero after cutover).

---

## 5. Connection Counts

### 5.1 During Development (1 TCP Server)

All on one mysqld instance:

| Pool | Count | Per-shard conns | Total |
|---|---|---|---|
| 2 data shards × 10 conns | 2 pools | 10 each | 20 |
| 2 lookup shards × 5 conns | 2 pools | 5 each | 10 |
| **Total** | | | **30** |

After adding data_2:

| Pool | Count | Per-shard conns | Total |
|---|---|---|---|
| 3 data shards × 10 conns | 3 pools | 10 each | 30 |
| 2 lookup shards × 5 conns | 2 pools | 5 each | 10 |
| **Total** | | | **40** |

MySQL default `max_connections` = 151 per instance. Since all databases share
one mysqld, ALL pools draw from the same limit. 40 connections is fine.

### 5.2 Watch Out For

- **Test connections:** if `go test` opens its own pools while the server is
  running, those add to the count. Use a small pool size in tests (2-3 conns).
- **The seeder:** if running concurrently with the server, the seeder's
  connections add to the count. The current seeder uses `MaxOpenConns(2)`.
- **Idle connection accumulation:** the pool sweeper retires idle connections
  after 2 minutes. Under light dev load, connections are created but rarely
  reclaimed. Monitor with `SHOW STATUS LIKE 'Threads_connected'`.

---

## 6. Redis — Shared, Not Sharded (Until It Needs to Be)

### 6.1 Current Setup: One Shared Redis Instance

Redis is **decoupled from both the TCP servers and the DB shards.** All TCP
servers connect to the same Redis instance. All cache keys are shard-unaware:

| Key | Keyed by | Why it can't be per-shard |
|---|---|---|
| `auth:<username>` | username | Any TCP server handles any login |
| `user:<id>:v<N>` | user_id | Any TCP server handles any get_user |
| `sess:<token>` | random token | Sessions have no shard affinity — token is random |
| `lookup:<username>` | username | Any TCP server handles any login |

If cache were **per-DB-shard** (Redis instance 0 caches only shard 0 users),
a TCP server would need to know which Redis to query for a given user — adding
a second routing layer on top of the DB shard routing. And session tokens are
random, so there's no way to shard them by user without a lookup.

If cache were **per-TCP-server** (each TCP server has its own Redis), a user
who hits TCP server 0 on login and TCP server 1 on the next request would miss
the cache on server 1. Cache hit rate drops by a factor of N.

One shared Redis = maximum hit rate, no routing, sessions work regardless of
which TCP server handles the next request.

### 6.2 Nothing to Set Up for This Entry Task

- One Redis instance (already running as a systemd service on this machine).
- `go-redis` `NewClient` with one address (already configured).
- All sharding-related cache keys (`lookup:<username>`) use the same Redis
  instance as the existing keys (`auth:`, `user:`, `sess:`).
- No Redis configuration changes needed.

### 6.3 When Redis Itself Needs to Scale — Redis Cluster

If the single Redis instance becomes a bottleneck (memory, throughput, or
network), switch to **Redis Cluster**. Redis Cluster handles its own internal
sharding — you do NOT shard Redis yourself.

**How Redis Cluster works:**
- The key space is divided into **16384 hash slots**: `CRC16(key) % 16384`.
- Each Redis node owns a range of slots.
- Adding a node → Redis migrates some slots to it automatically (same concept
  as consistent hashing, built into Redis).
- The client library (`go-redis`) handles routing transparently — it knows which
  node owns each slot and sends the command directly there.

**What changes in Go code:**

```go
// Before (single instance — what we have now):
rdb := redis.NewClient(&redis.Options{
    Addr: "127.0.0.1:6379",
})

// After (cluster — when scaling is needed):
rdb := redis.NewClusterClient(&redis.ClusterOptions{
    Addrs: []string{"redis-0:6379", "redis-1:6379", "redis-2:6379"},
})
```

Both implement the same `redis.Cmdable` interface. The `cache.Client` wrapper
in `cache/redis.go` would change its embedded field type from `*redis.Client`
to `redis.UniversalClient` (an interface both satisfy). All the typed helpers
(`GetAuthCache`, `SetUserCache`, `GetSession`, etc.) are unchanged — they call
`rdb.Get()`, `rdb.Set()`, `rdb.Del()` which work identically on both.

**What you do NOT need to do:**
- No key prefix changes. Redis Cluster hashes the full key — `auth:alice` and
  `user:123:v5` go to their respective slots automatically.
- No shard-aware key design. Don't add shard IDs to keys (like `shard0:auth:alice`)
  — that fights Redis Cluster's own hashing and gains nothing.
- No manual slot assignment. Redis Cluster auto-balances slots across nodes.
- No changes to TTLs, invalidation logic, or the cache-aside pattern.

**What you DO need to set up on the Redis side (when the time comes):**

1. **Install Redis on 3+ nodes** (minimum 3 masters for Redis Cluster; each can
   have a replica for availability):
   ```bash
   # On each node, in redis.conf:
   cluster-enabled yes
   cluster-config-file nodes.conf
   cluster-node-timeout 5000
   ```

2. **Create the cluster:**
   ```bash
   redis-cli --cluster create \
     redis-0:6379 redis-1:6379 redis-2:6379 \
     --cluster-replicas 0
   ```
   This auto-assigns the 16384 slots evenly across the 3 nodes (~5461 slots each).

3. **Adding a node later:**
   ```bash
   redis-cli --cluster add-node redis-3:6379 redis-0:6379
   redis-cli --cluster rebalance redis-0:6379
   ```
   Redis migrates slots from existing nodes to the new one. Clients see a brief
   `MOVED` redirect during migration — `go-redis` handles this automatically
   (follows the redirect and updates its slot map).

4. **No application downtime.** Slot migration happens live. Clients that hit a
   migrating key get an `ASK` redirect to the new node — the client library
   follows it transparently. No need to stop the servers.

**Multi-key operations caveat:** Redis Cluster requires that all keys in a
pipeline or transaction live on the same slot. Our pipelines are scoped to one
key (the session GET+PTTL pipeline operates on one `sess:<token>` key), so this
is not an issue. If you later need to pipeline operations across different keys,
use **hash tags** (`{user:123}:profile` and `{user:123}:settings` both hash on
`user:123` and land on the same slot) — but this is not needed for the current
key design.

### 6.4 For This Entry Task: Do Nothing

Redis stays as-is. Single instance, single `NewClient`, no cluster config. The
design is cluster-ready (shard-unaware keys, no multi-key transactions across
different users), so the migration to Redis Cluster when needed is a config
change + infrastructure setup, not a code rewrite. Document this as a future
scaling path, same as DB replication — considered, compatible, not implemented.

---

## 7. Things That Do NOT Change (Including Redis)

Verify these still work after sharding, but don't modify them:

- [ ] `db/pool.go` — custom pool lifecycle, acquire/release, sweeper
- [ ] `db/conn.go` — prepared statement cache, Row/Scan, cloneValues
- [ ] `cache/session.go` — session CRUD, sliding TTL, pipelined GET+PTTL
- [ ] `cache/invalidate.go` — delete-on-write (same keys, same logic)
- [ ] `cache/user.go` — `auth:<username>` and `user:<id>:v<N>` (shard-unaware)
- [ ] `model/session.go` — session struct (carries user_id, row_version)
- [ ] `model/user.go` — User struct, AvatarRelPath, AvatarURL, cached projections
- [ ] `protocol/` — frame encoding, ops, data structs
- [ ] `httpserver/` — HTTP handlers, TCP client pool, middleware, cookie handling
- [ ] `internal/validate/` — input validation
- [ ] `internal/crypto/` — bcrypt, random token/suffix generation
- [ ] Avatar filesystem sharding (`user_id & 0x1FFF`) — orthogonal to DB sharding
- [ ] `LAST_INSERT_ID(row_version + 1)` trick — per-connection, per-shard, unchanged
- [ ] Redis — single shared instance, all cache keys are shard-unaware (see §6)

---

## 8. Testing Checklist

### 8.1 Unit Tests

- [ ] `idgen`: IDs are unique, time-sorted, machine bits correct, sequence wraps
- [ ] Consistent hash ring: even distribution, ~1/N movement on add/remove
- [ ] Lookup shard routing: `hash(username) % 2` is deterministic and even
- [ ] `InsertLookup` / `GetLookup`: PK constraint catches duplicates
- [ ] `InsertUser` with explicit ID: row inserted correctly, no AUTO_INCREMENT

### 8.2 Integration Tests

- [ ] Signup: user lands on correct data shard AND correct lookup shard
- [ ] Login: lookup resolves username → correct shard → correct user
- [ ] Login with lookup cache warm: skips MySQL lookup, still routes correctly
- [ ] Get user: session user_id → correct shard → correct profile
- [ ] Update nickname: correct shard, row_version bumps, cache invalidated
- [ ] Update avatar: file-first ordering still works, correct shard
- [ ] Signup duplicate username: caught by lookup shard PK, not data shard

### 8.3 Migration Tests (2 → 3)

- [ ] Dual-write phase: writes land on both old and new shard
- [ ] Backfill: all moved rows appear on the new shard
- [ ] Read-new phase: reads come from the correct new shard
- [ ] Cutover: verify script reports zero misplaced rows
- [ ] End-to-end: signup + login + profile edit all work after migration

### 8.4 Perf Tests

- [ ] Login RPS with 2 data shards (should be ≥ single-instance performance)
- [ ] Login RPS with 3 data shards after migration (should be similar or better)
- [ ] Lookup overhead: measure the added latency from the username lookup step
- [ ] Connection pool stats: no pool exhaustion across shards

---

## 9. File Change Summary

| File | Change type | Description |
|---|---|---|
| `internal/idgen/` | **New package** | Snowflake ID generator |
| `internal/sharding/` | **New package** | Consistent hash ring |
| `internal/db/lookup.go` | **New file** | InsertLookup, GetLookup, DeleteLookup |
| `internal/db/shard.go` | **New file** | ShardedHandle, ShardFor, LookupShardFor |
| `internal/db/users.go` | **Modified** | InsertUser takes ID param; query helpers take *Pool |
| `internal/cache/lookup.go` | **New file** | GetLookupCache, SetLookupCache |
| `internal/config/config.go` | **Modified** | DataShardDSNs, LookupShardDSNs, MachineID |
| `internal/tcpserver/server.go` | **Modified** | Server holds ShardedHandle + IDGenerator |
| `internal/tcpserver/auth.go` | **Modified** | Signup: snowflake + lookup + shard routing. Login: lookup + shard routing |
| `internal/tcpserver/profile.go` | **Modified** | Add `s.db.ShardFor(session.UserID)` before DB calls |
| `cmd/tcp-server/main.go` | **Modified** | Open multiple pools, build ShardedHandle |
| `cmd/seed/main.go` | **Modified** | Per-shard batching, snowflake IDs, populate lookups |
| `cmd/setup-shards/` | **New** | Schema creation script |
| `cmd/migrate-shards/` | **New** | 2→3 backfill job |
| `cmd/verify-shards/` | **New** | Post-migration verification |
| `schema.sql` | **Modified** | Add lookup table DDL, remove AUTO_INCREMENT from users |

---

## 10. Order of Operations — Summary

```
1. Rebuild + restart servers (fix stale binary issue)
2. Verify existing tests pass
3. Create shard databases + tables (§2)
4. Build snowflake ID generator (step 1)
5. Build consistent hash ring (step 2)
6. Build shard-aware DB handle (step 3)
7. Build lookup helpers + cache (step 4)
8. Update InsertUser (step 5)
9. Update signup handler (step 6)
10. Update login handler (step 7)
11. Update post-login handlers (step 8)
12. Update config (step 9)
13. Update seeder (step 10)
14. Run seeder against sharded schemas
15. Verify all tests pass
16. Run perf tests with 2 data shards
17. Add data_2, run migration (§4)
18. Verify migration with verify-shards
19. Run perf tests with 3 data shards
```


### IMPLEMENTATION / SETUP

0. Find the separator your unit uses
   - `systemctl cat mysqld@.service | grep -i suffix`
   - Shown below as \<SEP> ('@' or '.'). Instance name = app_shard_N, used identically everywhere.

1. Stop default server
  - 
    ```
    sudo systemctl stop mysqld
    sudo systemctl disable mysqld
    ```
  - 
2. Set Up shard instances
  - shard N (/etc/my.cnf.d/app_shard_instance_N.cnf)
    ```
    [mysqld<SEP>app_shard_instance_N] ('.' or '@' depends on step 0 but the name needs to align with header in step 3 and start in step 4)
    datadir = /var/lib/mysql_app_shard_instance_N
    socket = /var/lib/mysql_app_shard_instance_N/mysql.sock
    port = 3306 # some port
    pid-file = /run/mysqld/app_shard_instance_N.pid
    log-error = /var/log/mysql/app_shard_instance_N_error.log
    ```
3. Initialise directories being used/referred to
  - Make shard 1...N (data) directories
    ```
    # Create and set permissions for shard_instance directories
    sudo mkdir -p /var/lib/mysql_app_shard_instance_1 ... /var/lib/mysql_app_shard_instance_N
    sudo chown -R mysql:mysql /var/lib/mysql_app_shard_instance_1 ... /var/lib/mysql_app_shard_instance_N
    sudo chmod 750 /var/lib/mysql_app_shard_instance_1 ... /var/lib/mysql_app_shard_instance_N
    ```
  - Initialise shard 1...N data directories
    ```
    # Initialize shard_instance data directories ('.' or '@' depends on step 0 but the name needs to align with header in step 2 and start in step 4)
    sudo mysqld --defaults-group-suffix=.app_shard_instance_1 --initialize-insecure --user=mysql
    ...
    sudo mysqld --defaults-group-suffix=.app_shard_instance_N --initialize-insecure --user=mysql
    ```
4. Start N shard services
  - 
    ```
    # Start and enable 1...N instances ('@' for systemctl terminal command, no '.' variant)
    sudo systemctl enable --now mysqld@app_shard_instance_1
    ...
    sudo systemctl enable --now mysqld@app_shard_instance_N
    ```


#### One important detail to watch out for
- Any global setting left in [mysqld] inside mysql-server.cnf or entry.cnf that explicitly specifies paths (like datadir = /var/lib/mysql) must be overridden in both [mysqld@app1] and [mysqld@app2].
- Because MySQL evaluates [mysqld] first, if @app1 doesn't explicitly override datadir, it will try to fall back to whatever path was in [mysqld]. As long as both @app1 and @app2 define their own datadir, socket, port, pid-file, and log-error, your generic [mysqld] block will purely serve as shared server config!



##### Part 1: .cnf Configuration File Parameters
```
1. Section Header: [mysqld@app1]

    What it does: Defines a configuration group.

    Why it matters: Tells MySQL to apply these parameters only when the process is invoked with the matching suffix --defaults-group-suffix=@app1 (which systemd passes automatically when running mysqld@app1).

2. datadir = /var/lib/mysql_app1

    What it does: Specifies the physical directory on disk where this MySQL instance stores its database files, table data, indexes, and internal state.

    Why it matters: Isolates each instance's databases. If two instances share a datadir, they will corrupt each other's data files.

3. socket = /var/lib/mysql_app1/mysql.sock

    What it does: Specifies the Unix domain socket file path used for local, non-network communication.

    Why it matters: When you run mysql locally on the server without -h 127.0.0.1, it connects through this file instead of TCP/IP. Placing it inside the instance's folder prevents name collisions with other instances.

4. port = 3306

    What it does: Sets the TCP/IP port number on which this instance listens for network connections.

    Why it matters: Two processes on Linux cannot bind to the same network port simultaneously. Each instance must have a unique port (e.g., 3306 for app1, 3307 for app2).

5. pid-file = /run/mysqld/app1.pid

    What it does: Specifies the path to the Process ID (PID) file, which holds the active Linux process ID number of the running MySQL daemon.

    Why it matters: Systemd uses this file to track, send control signals to, or gracefully stop the exact process corresponding to this database instance.

6. log-error = /var/log/mysql/app1_error.log

    What it does: Sets the file path where MySQL logs diagnostic messages, warnings, startup steps, and critical crashes.

    Why it matters: Keeping logs separate per instance makes troubleshooting startup failures or database errors much easier.
```

##### Part 2: Shell Command Breakdown
```
Section A: Directory Setup
sudo mkdir -p /var/lib/mysql_app1 /var/lib/mysql_app2

    sudo: Executes the command with root (administrative) privileges.

    mkdir: Makes new directories.

    -p (parents): Creates parent directories if they don't exist and avoids throwing errors if the target directory already exists.

    Target paths: Creates the two isolated storage folders for app1 and app2.

sudo chown -R mysql:mysql /var/lib/mysql_app1 /var/lib/mysql_app2

    chown: Changes file/directory ownership.

    -R (recursive): Applies ownership changes to the folder and all files/subdirectories inside it.

    mysql:mysql: Assigns user ownership to the mysql user and group ownership to the mysql group.

    Why: MySQL runs under an unprivileged system user (mysql). If the mysql user does not own these folders, the server will crash with permission errors.

sudo chmod 750 /var/lib/mysql_app1 /var/lib/mysql_app2

    chmod: Changes access permissions on directories.

    750: Sets explicit access privileges:

        7 (Owner - mysql user): Read, write, execute/traverse (rwx).

        5 (Group - mysql group): Read, execute/traverse (r-x).

        0 (Others - rest of the world): Zero permissions (---).

    Why: Ensures other standard system users on Linux cannot inspect or steal raw database files.

Section B: Database Initialization
sudo mysqld --defaults-group-suffix=@app1 --initialize-insecure --user=mysql

    mysqld: The actual MySQL server daemon executable program.

    --defaults-group-suffix=@app1: Instructs MySQL to read configuration parameters under [mysqld@app1] in /etc/my.cnf.d/, pulling the correct datadir, socket, and settings.

    --initialize-insecure: Generates system tables (like the mysql internal schema) and bootstraps the data directory. The insecure flag sets up the root user with a blank password for convenient local dev setup.

    --user=mysql: Ensures file creation during initialization happens as the mysql system user.

sudo mysqld --defaults-group-suffix=@app2 --initialize-insecure --user=mysql

    Does the exact same bootstrapping process for Instance 2 using settings under [mysqld@app2].

Section C: Systemd Service Management
sudo systemctl enable --now mysqld@app1

    systemctl: The Linux service manager CLI tool.

    enable: Registers mysqld@app1 so Linux automatically boots this database instance on system startup.

    --now: Immediately starts the service right now in addition to enabling it (saving you from running a separate systemctl start command).

    mysqld@app1: Uses systemd's templated service unit, running instance 1.

sudo systemctl enable --now mysqld@app2

    Enables and immediately starts Instance 2 on port 3307.
```

For now:
```
sudo mkdir -p /var/lib/mysql_entry_app_shard_instance_1 /var/lib/mysql_entry_app_shard_instance_2
sudo chown -R mysql:mysql /var/lib/mysql_entry_app_shard_instance_1 /var/lib/mysql_entry_app_shard_instance_2
sudo chmod 750 /var/lib/mysql_entry_app_shard_instance_1 /var/lib/mysql_entry_app_shard_instance_2

sudo mysqld --defaults-group-suffix=.entry_app_shard_instance_1 --initialize-insecure --user=mysql
sudo mysqld --defaults-group-suffix=.entry_app_shard_instance_2 --initialize-insecure --user=mysql

sudo systemctl enable --now mysqld@entry_app_shard_instance_1
sudo systemctl enable --now mysqld@entry_app_shard_instance_2
```

* Data shards: ports 3211 (data_0) and 3212 (data_1)
* Lookup shards: ports 3221 (lookup_0) and 3222 (lookup_1)


use `getenforce` to check for need of semanage commands, if not then this is it
else need to
```
sudo semanage fcontext -a -t mysqld_db_t "/var/lib/mysql_app_shard_N(/.*)?"
sudo semanage fcontext -a -t mysqld_log_t "/var/log/mysql(/.*)?"
sudo restorecon -Rv /var/lib/mysql_app_shard_N /var/log/mysql
sudo semanage port -a -t mysqld_port_t -p tcp 330N
```

or something about there