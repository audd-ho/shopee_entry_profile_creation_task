# System Design — User Management System (Entry Task)

Living design document. Each section lists **Options → Considerations → Decision**. Update as decisions are locked in.

---

## 1. Background

Entry task to practice problem solving, system design, and the team's core tech stack (Go, MySQL, Redis).
Build a user management system supporting signup, login, and profile edit (nickname + profile picture).

---

## 2. Requirements

### Functional
- User signup with username + password
- User login (authenticate, return user info on success, error on failure)
- Edit nickname (UTF-8, unicode support)
- Upload profile picture
- User info: username (immutable), nickname, profile picture

### Non-Functional / Design Constraints
- **Two separate processes**: HTTP server (web + request forwarding) and backend server (business logic, DB, auth). Communicate over network.
- MySQL for user data, accessed via Go MySQL client.
- **Custom-built connection pool** (do not use `database/sql`'s built-in pool as the pool — build our own).
- Redis for caching.
- Design must address: robustness, security, performance.

### Performance Targets
- 10M+ user rows in test DB.
- 1000 concurrent HTTP requests supported.
- **No cache:** 3k login RPS from ≥200 unique users, no socket/timeout errors. (Optional stretch: 10k RPS.)
  - assumed log in without session cookie (no redis cache to store it tooß)
- **With cache:** 6k login RPS, same conditions. (Optional stretch: 20k RPS.)
  - assumed log in without session cookie

---

## 3. High-Level Architecture

```
   Browser
     │  HTTPS/HTTP
     ▼
┌──────────────┐     TCP (custom protocol)     ┌───────────────┐
│  HTTP Server │ ────────────────────────────► │  TCP Server   │
│  (Go)        │ ◄──────────────────────────── │  (Go)         │
│              │                               │               │
│ - Serves     │                               │ - Auth logic  │
│   HTML/JS    │                               │ - Session mgr │
│ - Parses     │                               │ - Rate limit  │
│   requests   │                               │               │
│ - Session    │                               │               │
│   cookie     │                               │               │
└──────────────┘                               └───┬───────┬───┘
                                                   │       │
                                          ┌────────▼──┐ ┌──▼──────┐
                                          │  MySQL    │ │  Redis  │
                                          │(10M+ rows)│ │         │
                                          │ custom    │ │ cache + │
                                          │ conn pool │ │ session │
                                          └───────────┘ └─────────┘

Image storage: filesystem (path stored in MySQL) — see §7
```

---

## 4. Design Decisions — Points of Consideration

Each subsection: **Options → Considerations → Decision**. Mark decisions with **TBD** until decided.

---

### 4.1 Cache Strategy — Cache-Aside vs Read-Through

**Options:**
- (A) **Cache-aside**: app checks Redis → miss → app queries MySQL → app writes to Redis → return.
- (B) **Read-through**: cache layer itself fetches from DB on miss.

**Considerations:**
- Plain Redis has no built-in DB connector; read-through requires modules (RedisGears) or a custom proxy — extra complexity, extra process.
  - Complexity refers to
    - Maintainability
    - Debuggability
    - Coupling
- Cache-aside is industry standard.
  - Provides greater control
    - TTL
    - invalidation
    - cache key design
- Cache-aside directly enables the 3k→6k RPS jump.
  - No need to have read through based on requirements
  - No reason to increase coupling and complexity(above) for these
  - Easier to scale with cache-aside as well, more decoupled

**Decision: Cache-aside.** 

**Sub-decisions to make:**
- What do we cache?
  - username to password bcrypt hash since login is the performance requirement as well
- TTL?
  - Login lookups: ~5-10 min.
  - Profile: longer, e.g. 1 hr.
- Invalidation policy on profile edit: delete-on-write vs write-through?
  - delete-on-write based on below
- Negative caching (cache "user not found" for a short TTL to protect against enumeration/lookup spam)
  - not needed for now, easy to implement in the future if need, not a priority
- **DB versioning**
  - Implement db versioning to remove cache/db sync problem
  - For updates:
    - only update if new_version > cache_version
    - if current_version <= cache_version, = cache miss and lookup db for new_version
  - For read:
    - only read if current_version == cache_version
    - if current_version < cache_version then db lookup for new_version + write to cache (goroutine) [ensure cache is latest + cache cant see < or > unless really check or use versioning in value, if versioning in key then is just != then write to cache is for both anyway]
    - if current_version > cache_version then db lookup for current_version + write to cache (goroutine)
  - version as part of cache key to ensure
    - old version key or prefix/versions instantly unreachable without key-deletion
    - TTL can clean up old keys since not accessed, unlike one single key that keeps having value changed
    - no potentially error prone race condition
  - versioning in DB to be column/row level and level of split of tables in db will be below

**Invalidation strategy — decided:**

Two options considered:

**(A) Delete-on-write (cache-aside invalidation):**
```
UPDATE users SET nickname = ? WHERE id = ?
(AFTER)
[separate goroutine]
DEL user:<username>:old_version
DEL profile:<user_id>:old_version
```
Next reader misses cache, refetches from DB, populates cache.

**(B) Write-through:**
```
UPDATE users SET nickname = ? WHERE id = ?
(AFTER)
[separate goroutine]
SET user:<username>:new_version <new full record>
SET profile:<user_id>:new_version <new profile>
```
Cache always fresh; readers hit cache immediately.

**Race analysis:** write-through has a subtle bug with concurrent writers:
```
T=1: A: UPDATE nickname = "A"       T=2: B: UPDATE nickname = "B"
T=3: B: SET user:alice {nickname: "B"}
T=4: A: SET user:alice {nickname: "A"}   ← overwrites with older data
Result: DB = "B", Cache = "A"  (divergent until TTL)
```
Delete-on-write is race-safe (deleting is idempotent; next read refetches fresh).

**Decision: Delete-on-write.** Reasons:
- Race-safe under concurrent writers
- Simpler (no need to serialize full record on every write)
- One extra DB read on next access — acceptable trade
- Consistent with "cache-aside" pattern from on top

**Invalidation triggers:**

| Action | Keys to invalidate |
|---|---|
| Nickname update | `user:<username>:old_row_version`, `profile:<user_id>:old_row_version` |
| Avatar update | `user:<username>:old_row_version`, `profile:<user_id>:old_row_version` |
| Signup | `user:<username>` (only relevant if negative caching implemented, not implemented) |
| Logout | `sess:<token>` (pure cache, no db involvement, no need for versioning) |
| Password change (future) | `user:<username>:old_row_version` + all sessions for that user |

**Negative caching note:** if we implement "user not found" negative caching, signup MUST invalidate that marker (`DEL user:<newuser>`), else login for a just-created user would incorrectly report not-found for the TTL window. Currently deferred; add if enumeration attacks become a concern.
- for the current set up, this is of a low priority but was considered.

- versioning design
  - per column versioning
    - pros
      - field A change does not mean field B change version, able to remain independent "decoupled"
      - scalable if more fields in the future that needs
    - cons
      - harder to maintain with more fields and more field versioning
      - greatly incentivise splitting table to like BNF form or something since to really be decoupled, need if field A change, dont affect field B in another table, FK by id
      - no reason to do so for current needs + only 2 fields can change in a row so it is manageable + QUERY normally require majority of the entire row so if split up, have to query many table or join tables, overhead not earned + cache for these 2 fields that change contain BOTH fields so v1 in nickname and v1 in avatar so when one change and ther other dont? so may require cache to further split double lookup + cache contain almost the entire row of the db anyway so versioning on the entire row makes more sense too
      - nickname change and avatar change are infrequent ops so unlikely to hold the lock of the table's ROW for too long to have any impact on the latency/overhead on the api endpoints
  - per row versioning
    - the inverse of the above actually, so this is preferred here for the DB

**Implementation note (resolved in §4.4.4):** the per-row version above is a dedicated `row_version` column. It was originally kept separate from an `avatar_version` column (§4.4) for exactly the reason below — but `avatar_version` was later removed entirely (§4.4.4 point 3a), so today `row_version` is simply the only version counter in the schema. `row_version` bumps on *any* mutating write (nickname or avatar) and is embedded only in the `profile:<user_id>:v<row_version>` Redis key — **not** `user:<username>`, which is deliberately left unversioned (see §5's Redis key table for why). The historical reasoning for keeping the two counters separate, back when both existed: conflating them would have made a nickname-only edit bump `avatar_version` too, pointing avatar URLs at a version whose file was never written — see §4.4.4 point 4 for the full race analysis, which remains accurate as a record of why that design used two counters, even though only one exists now.

---

### 4.2 Authentication & Session — Cookies vs JWT

**Options:**
- (A) **Session cookie + Redis session store** (`session_id → user_id`, HttpOnly, Secure, SameSite).
- (B) **JWT** (stateless, signed token).

**Considerations:**

| Aspect | Session Cookie | JWT |
|---|---|---|
| Revocation | Easy (delete Redis key) | Hard (needs blocklist, defeats stateless benefit) |
| Payload size | Small (opaque ID) | Larger (signed claims) |
| Fit for this task | Uses Redis naturally | Overkill for single-service backend |
| Rotation on profile change | Trivial | Requires re-issue |
| Complexity | Simple | Moderate |
| ***explicit single use?*** | supposed to be long lasting and easy ti remove/log out and revoke | not easy to revoke and meant more for single one time use like auth |

**Decision: Session cookie backed by Redis.** 

**Sub-decisions:**
- Cookie attributes: `HttpOnly`, `SameSite=Lax` (or `Strict`?), `Secure` (production only). 
- Session TTL: 24h? Sliding window (refresh on activity) or fixed?
  - sliding window, rolling, refresh for next 24h every time
- Session key format: `sess:<random-256-bit>` → JSON `{user_id, created_at, ip, last_seen}`. [in redis]
  - ip is for different ip, suspicious behaviour, but if want to expand can use, if not, can leave as so, no problem, last seen for rolling refresh

**Status:** sesssion cookie + TTL & sliding behavior.

---

### 4.3 Password Hashing

**Options:**
- (A) **bcrypt** (`golang.org/x/crypto/bcrypt`) — battle-tested, salt built-in.
- (B) **Argon2id** (`golang.org/x/crypto/argon2`) — OWASP-recommended, memory-hard.
- (C) SHA-256 + salt (weaker, faster).

**Considerations:**
- Never plaintext, never plain SHA.
- bcrypt cost 10 ≈ ~50-100ms/hash on modern CPU. Cost 12 ≈ ~250-400ms.
- Argon2id is stronger but slightly more complex to tune. + use too much ram (can OOM on local machine, current infra).
- **Perf impact:** login RPS test does 3k-20k login/sec. If every login re-hashes, at cost 10 we'd need ~150-1000 CPU cores just for bcrypt.
  - **Mitigation:** cache authenticated sessions in Redis; the RPS test likely reuses a small set of credentials → hashed password can be cached (as `username → bcrypt_hash`, verified in memory) OR test uses sessions after first login.
  - Need to clarify what the perf harness counts as a "login request".

**Decision: bcrypt, cost 10.**  (Simplicity, sufficient for this task.)

**Status:** — confirm cost after seeing perf harness behavior.
-  scaled to cost = 5 to hit perf requirements, and cost = 5 > SHA-256 still, and has salt anyway so resist against rainbow table

---

### 4.4 Image Storage — Filesystem vs BLOB vs Object Store

**Options:**
- (A) **Local filesystem**, path stored in MySQL.
- (B) **MySQL BLOB** column.
- (C) **Object store** (S3 / OSS / MinIO).

**Considerations:**

| Aspect | Filesystem | MySQL BLOB | S3/OSS |
|---|---|---|---|
| DB bloat | None (only path) | High — hurts 10M-row query perf | None |
| Backup complexity | Separate file backup | Included in DB dump (big) | Managed |
| Perf test impact | Zero (login doesn't touch images) | Some (larger rows in buffer pool) | Zero |
| Persistence | Yes | Yes (ACID) | Yes |
| Transactionality with DB | No (possible orphaned files) | Yes | No |
| Scale ceiling | Single-machine disk | DB size / IOPS | Effectively infinite |
| Operational overhead | Low (a directory) | Low | Higher (creds, SDK, cost) |
| Fit for entry task | ✅ Right size | ⚠️ Hurts perf | ❌ Overkill |
| Fit for production | Needs shared FS or migrate | Not recommended | ✅ |

**Bloat reality check:**
- Login RPS test does NOT upload images — image storage is perf-neutral for the benchmark.
- Filesystem "bloat" is just disk usage, doesn't affect hot paths.
- DB bloat is expensive (buffer pool waste, slower scans, big backups).

**Decision: Filesystem, path in MySQL.**

**Sub-decisions:**
- **Default avatar:** all new users start with `avatar_suffix = NULL`. Server serves a shared `default.png` (static asset, not per-user) in this state. Once user uploads, `avatar_suffix` becomes non-NULL and their uploaded file is served instead. (An earlier version of this schema gated the same thing on `avatar_version = 0` — see §4.4.4 for why that column was removed.)
- Max upload size: **2 MB**.
- Allowed types: jpeg, png, webp — sniff content, don't trust extension (see "Content sniffing" below).
- Write order: write file first (temp + atomic rename), then update DB row. On crash between the two → orphaned file (harmless, sweeper cleans up). Reverse order would leave DB pointing at nonexistent file (visible breakage). **This is now the actual, final order** — see §4.4.4 for why an earlier iteration briefly had it backwards, and what changed to make this line true again.

---

#### 4.4.1 Content Sniffing — Why & How

"Sniff content, don't trust extension" means inspect the actual bytes of the upload rather than trusting:
1. The filename extension (set by user, trivially faked)
2. The `Content-Type` header from the browser (also faked)

Real file formats have distinctive **magic numbers** in their first bytes:

| Format | First bytes (hex) |
|---|---|
| PNG | `89 50 4E 47 0D 0A 1A 0A` |
| JPEG | `FF D8 FF` |
| WebP | `52 49 46 46 ?? ?? ?? ?? 57 45 42 50` |

**Go implementation:** `http.DetectContentType(firstBytes)` reads up to 512 bytes and returns a MIME type. Reject if not in the allowlist.

**Attacks prevented:**
- Executables/HTML/PHP disguised as `.jpg`
- Server serving a "PNG" that browser sniffs as HTML → XSS
- Decompression bombs (partially — also add dimension check via `image.Decode`)

**Layers of defense (in order of value):**
1. **Size check first** — reject > 2 MB before reading full body
2. **Sniff first 512 bytes** with `http.DetectContentType`
3. **MIME allowlist**: `image/jpeg`, `image/png`, `image/webp`
4. **Fresh filename** — never trust user's filename (see 4.4.3)
5. **Optional stretch:** decode via `image.Decode` to catch malformed files & check dimensions (reject > 4096×4096)
6. **Optional stretch:** re-encode to canonical JPEG (strips EXIF, embedded scripts)

**When serving uploads:** set explicit `Content-Type` header AND `X-Content-Type-Options: nosniff` — the second tells the browser NOT to sniff the response (browser sniffing can turn a "PNG" into an executed HTML page).

**Two "sniffs" — don't confuse:**
- We sniff uploads (good, defensive)
- Browser sniffs responses (bad by default, disable with `nosniff` header) [`X-Content-Type-Options: nosniff`]

---

#### 4.4.2 Filename Scheme — Options Considered

We considered several schemes; each has trade-offs:

| Scheme | Non-enumerable? | Cache-bust? | FS race? | Debuggable? | Extra state |
|---|---|---|---|---|---|
| `<user_id>.jpg` | ❌ | Manual (?v=X query) | Torn write on concurrent upload | ✅ | None |
| `<user_id>_v<n>.jpg` | ❌ | Automatic (URL changes) | ✅ Different file per upload | ✅ | version int |
| `<user_id>_v<n>_<suffix>.jpg` (tried first, superseded — see §4.4.4) | ✅ | Automatic | ✅ Different file per upload | ✅ | version int + suffix |
| **`<random>.jpg`, scoped as `<user_id>_<suffix>.jpg` (chosen)** | ✅ | Automatic | ✅ Different file per upload | ✅ (user_id still readable in the name) | suffix only |

**Rejected: plain `<user_id>.jpg` (Option D "naive"):**
- Concurrent uploads write to the same file → torn writes (half file A, half file B) possible on filesystems.
- Even with atomic rename, no version = need manual cache-busting via query string.
- Enumerable, but that's acceptable for a public profile field.

**Rejected: `<user_id>_v<version>_<suffix>.jpg` (tried, then removed — full account in §4.4.4):**
- Solved non-enumerability, cache-busting, and the FS race, all at once — which is why it was the first real candidate.
- The version had to come from an atomic MySQL increment, and that requirement is what forced the database write to happen before the file existed. §4.4.4 covers the specific bug that ordering caused and why the fix was to drop the version rather than reorder around it.

**Rejected: fully random filename with no user_id structure:**
- Solves non-enumerability but requires storing the full path in the DB (~40-100 bytes/row × 10M ≈ 1 GB) rather than an 8-byte suffix, since nothing in the filename is derivable from anything the row already has.
- Loses debuggability (`ls` gives no info about which user owns which file). This is what the chosen scheme's `<user_id>_` prefix buys over pure randomness, at no extra cost — the id is already the row's primary key, so prepending it is free.

**Chosen: `<user_id>_<suffix>.jpg`** — user_id for readability and per-user grouping (and for the directory shard, §4.4.3), suffix alone for both non-enumerability and cache-busting:
- **suffix**: 8-char random hex (32 bits) generated in Go via `crypto/rand`, with **no MySQL round trip to produce it**. That last property is the entire reason this scheme replaced the version-numbered one — see §4.4.4.
- Each upload writes a **different file** (new random suffix) → no FS race, same guarantee the version number used to provide, now from randomness instead of DB atomicity.
- Only 8 bytes of extra state per user (`CHAR(8)`) vs storing a full path — actually less than the version-numbered scheme's 12 bytes, since there is no separate version column any more.

**Enumerability discussion:**
- Server exposes URLs, not the filesystem itself. Filesystem is on the server; nobody can `ls` remotely.
- BUT: an attacker with a browser can iterate URLs (`/uploads/0/10001/10001_????????.jpg`).
- Sequential AUTO_INCREMENT IDs are trivially guessable.
- The 32-bit random suffix defeats iteration attacks (~5 days per user at 10k tries/sec).
- Bumped to `CHAR(16)` = 64 bits for margin at negligible cost — production would use 16+ hex chars.

---

#### 4.4.3 Directory Sharding

**Problem:** 10M files in one directory kills `readdir` performance on most filesystems. Need to shard.

**Options considered:**

- **(A) Fixed bucket count** (bitmask on low bits): `bucket = user_id & 0x1FFF` → 8192 buckets, always.
  - Load stays even as users are added.
  - Directory count is fixed and predictable.
  - Matches Redis Cluster / consistent hashing convention.
- **(B) Fixed bucket size** (bitshift on high bits): `bucket = user_id >> 14` → 16384 users/bucket.
  - Each bucket has bounded max size.
  - Bucket count grows unboundedly with user count.
  - Sequential IDs cluster in the same bucket → locality benefit for cohort/scan access.

**Locality analysis (why neither wins for RANDOM access):**
- CPU cache: not relevant — bucket calc is one AND/SHIFT op, doesn't touch big memory.
- OS page cache: caches individual files on demand, not by directory. Random avatar access has no locality anyway (100k active users out of 10M → working set determined by activity, not sharding).
- Filesystem `readdir` cache (dentry cache): both schemes produce small enough per-dir counts that dentry cache is fine.

**For non-random access, the schemes diverge:**
- **Sequential/cohort access → fixed size wins.** Related IDs cluster in the same directory → OS caches the dir hot → subsequent access hits warm cache.
- **Hot-spot avoidance → fixed count wins.** A burst of new users scatters across all buckets (no single bucket overloaded).
- **Random access → neither wins.**

**Scale-dependent behavior (fixed count vs fixed size):**

The two schemes are **symmetric** — both defer unbounded growth to a different dimension. Neither eliminates it.

| Scale | Fixed count (8192 buckets) | Fixed size (16384 users/bucket) |
|---|---|---|
| Top-level dir count | Bounded at 8192 forever ✅ | **Grows unboundedly** with user count ❌ |
| Files-per-bucket | **Grows unboundedly** with user count ❌ | Bounded at 16384 forever ✅ |
| ~10k users | ~1 file/bucket. Sparse, harmless. | ~1 bucket in use. Also harmless. |
| ~10M users | ~1220 files/bucket. Fine. | ~610 buckets, ≤16384 each. Fine. |
| ~1B users | ~122k files/bucket. `open()` still O(1) via ext4/XFS hashed dir index; `readdir` slow but we don't use it. | ~61k top-level buckets. Random file access fine; `ls /uploads/` slow. |

**Both hit re-sharding pressure eventually** — fixed count when per-bucket count gets uncomfortably large (say 1M+), fixed size when top-level count gets uncomfortably large (say 100k+). Both fixes are the same: add a second level of sharding.

For our access pattern (open a file by exact name, random across active users, no sequential scans), modern filesystems make lookup O(1) regardless of directory size. So the "large sub-dir" problem barely bites at realistic scale.

**Hot-spot avoidance vs locality — the underlying trade-off:**

This is a version of the classic **hash partitioning vs range partitioning** trade-off from distributed systems:

| Property | Fixed count (hash) | Fixed size (range) |
|---|---|---|
| Hot-spot risk | Low — burst of new users scatters across all buckets | **High** — a burst (marketing campaign, viral event) lands in one bucket |
| Locality | Poor — cohort of related users scattered across 8192 dirs | **Good** — cohort clusters in same dir → OS caches dir hot |
| Range scans | Not possible without touching all buckets | Efficient (users N to M all in same bucket range) |
| Best fit | Distributed systems, write-heavy, hot-spot-averse (Redis Cluster, Cassandra, DynamoDB) | Read-heavy scans, locality-sensitive, single-machine (HBase range, Spanner) |

**Applied to our system:**

*Single machine (our current setup):*
- All buckets share the same disk, page cache, CPU. Hot-spot on one bucket = same total load on the machine; no capacity loss.
- Hash-based sharding is solving a problem we don't have.
- Locality *could* matter later for cohort access patterns (bursts of new signups, ops scans).
- Fixed count's main win is inapplicable here.

*If we ever go distributed:*
- Hot-spot avoidance across machines becomes critical.
- **Hybrid pattern is the correct answer**: hash across machines (Redis-Cluster-style), range within each machine (fixed size).
- Migration: add a hash router in front. Inner layout (fixed size) stays exactly the same.

**Decision: Fixed bucket size, `bucket = user_id >> 14` (16384 users per bucket).**

Reasoning:
1. **On single machine, hash-based sharding solves a problem we don't have.** Its supposed advantage (cross-shard load balancing) is meaningless when there's only one shard.
2. **Fixed size costs nothing on random-access workload.** Modern filesystems make both O(1) for `open()` by exact name.
3. **Locality benefit is real for any future cohort/scan workload.** Fixed count would need re-thinking to gain this; fixed size has it for free.
4. **Cleanly extends to horizontal scaling via the hybrid pattern.** When we add machines, we add a hash router in front — inner directory layout unchanged. Fixed count would either stay flat (missing locality) or need migration.
5. **Only cost is unconventionality** (`>> 14` less canonical than `& 0x1FFF`), mitigated by documenting the reasoning here.

**Path from now to horizontal scale:**
- Now (1 machine): `/uploads/<user_id >> 14>/<user_id>/<file>` — range partitioning, locality-friendly.
- Multi-machine future: `machine = hash(user_id) % N`; each machine keeps the same `<user_id >> 14>/<user_id>/` inner layout.
- Result: cross-machine load balanced via hash; within-machine cohort locality preserved via range.

**Nuance — within-machine sequentiality is *partially* diluted by cross-machine hashing:**

Once we hash across N machines, each machine holds a strided sample of the global ID space (users 3, 6, 9, 12, ... for one; 1, 4, 7, 10, ... for another). Users on any given machine are no longer globally consecutive. But:
- Cohort-of-signups still clusters — a burst of new users produces contiguous global IDs, which after hashing produces a strided sub-range on each machine, which after range-bucketing still lands mostly in the same bucket per machine.
- So bursts still get cohort-locality on their assigned machine.
- Global range scans (touching users 1000-2000) require fan-out to all machines. Unavoidable with any hash partitioning.

For our uniform random-access workload the residual locality doesn't produce a measurable win, but the hybrid pattern still adds no cost over pure hash — so it's the strictly better long-term design.

**Final layout** (updated: no version segment — §4.4.4 point 3a removed `avatar_version`; the suffix alone now identifies the file):
```
/uploads/<user_id >> 14>/<user_id>/<user_id>_<suffix>.jpg
```

Example for user_id = 12345:
```
12345 >> 14 = 0
/uploads/0/12345/12345_a3f8b91c.jpg
```

Example for user_id = 500000:
```
500000 >> 14 = 30
/uploads/30/500000/500000_ff11aa22.jpg
```

**Escalation path (both schemes hit the same wall):**
- 10M-100M users → single-level sharding is fine.
- 100M-1B → add a second level: `/uploads/<b1>/<b2>/<user_id>/` — bounds growth at both levels.
- 1B+ → distribute across multiple servers / object storage (S3/OSS/CDN).

**Decision: (A) Fixed count, `user_id & 0x1FFF` → 8192 buckets.**
- Idiomatic bitmask sharding (matches distributed-systems convention).
- At 10M users → ~1220 files per bucket (well below hot-spot threshold).
- Empty-dir cost negligible (8192 × 4 KB ≈ 32 MB).
- Path deterministic from user_id, no need to store bucket separately.
- At 10B scale we'd add a second sharding level (`/uploads/<bucket1>/<bucket2>/<user_id>/`); not needed for this task.

**Final layout** (updated: no version segment, same reason as above):
```
/uploads/<user_id & 0x1FFF>/<user_id>/<user_id>_<suffix>.jpg
```

Example for user_id = 12345:
```
12345 & 0x1FFF = 4153
/uploads/4153/12345/12345_a3f8b91c.jpg
```

---

#### 4.4.4 Concurrency & Race Conditions

**Three races to consider on concurrent uploads by the same user:**

1. **DB version race** — two uploads both read old version and both write "old + 1", losing an update.
   - **Fix:** atomic `UPDATE users SET avatar_version = avatar_version + 1` — MySQL guarantees no interleaving for `SET x = x + 1` on a single row.

2. **Filesystem torn-write race** — two uploads writing to the same file interleave partial data.
   - **Fix (a):** different filename per upload (version+suffix in name) → no shared file.
   - **Fix (b):** write to `<name>.tmp`, then atomic `os.Rename()` to final name → POSIX guarantees rename atomicity.
   - We use both.

3. **Crash between DB update and file write** — DB says v=9 but v=9 file doesn't exist.
   - **Impact:** client requests broken avatar until next successful upload, and — worse than a simple crash — this could also happen on an ordinary, non-crashing failure: the file write itself could fail (disk full, permissions) *after* the DB commit that made `avatar_version = 9` durable, with no way to roll that commit back. The client is told the upload failed, but the row now disagrees: it points at a version whose file was never written.
   - **This is what motivated removing `avatar_version` entirely — see 3a below**, rather than accepting the mitigation this point originally proposed (reversing the write order within the versioned scheme). The versioned scheme could not simply reverse its order: the version number came from an atomic MySQL increment, so the database step had to run first to produce the number the filename needed.

3a. **Why `avatar_version` was removed, not just reordered around (post-implementation decision):**
   - **The alternative that was NOT taken:** keep `avatar_version`, but restructure the flow as `SELECT current version → compute next version in Go → write file at that version → UPDATE to commit it`. This achieves file-first ordering while keeping a version column, but costs **two MySQL round trips** instead of the single atomic statement, and — more importantly than the extra round trip — **reopens the exact race the atomic `LAST_INSERT_ID` trick existed to close**: two concurrent uploads for the same user could both `SELECT` the same current version, both compute "version + 1", both write a file at that same computed version, and then both `UPDATE`. That is precisely the failure race point 1 above already fixes for the single-statement version; a SELECT-then-UPDATE structure throws that fix away to get file-first ordering, which is a bad trade, not just a slower one.
   - **The fix taken instead:** delete the `avatar_version` column and identify each upload's file by its random suffix alone. The suffix is already generated in Go via `crypto/rand`, with **no MySQL round trip at all** — nothing to `SELECT`, nothing to reserve, nothing to race over. That is what makes true file-first-then-DB-second ordering possible in a single MySQL statement: `UPDATE users SET avatar_suffix = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`, run *after* the file is already on disk.
     - **Uniqueness under concurrency without a counter:** two concurrent uploads by the same user still cannot collide, because each independently draws a fresh 32-bit random suffix — the same property a monotonic counter provided, now from randomness instead of DB atomicity. This is the piece that makes dropping the counter *safe*, not just simpler: without it, "no shared filename" would have had no guarantee at all under concurrent uploads.
   - **What this fixes:** the row can now only ever point at a suffix if the file for that suffix was *already* written successfully — there is no ordering under which MySQL commits before the file exists. A failed `UPDATE` (rare — id vanished, connection dropped) leaves an orphaned file and an **unchanged, still-valid** row; the client is correctly told the upload failed, and the persisted state agrees with that. Point 3's "correct DB, missing file" state is no longer reachable at all, not just less likely.
   - **What this costs:** the orphan sweeper (see "Orphan cleanup" below) loses the ability to tell which file is current by reading filenames alone — a random suffix carries no ordering information, unlike a version number. It now needs one MySQL read per swept user to learn the authoritative `avatar_suffix`, where the old scheme needed none. This is a real, ongoing cost (a DB read per cleanup pass instead of zero), traded for eliminating a data-corruption bug outright — worth it, but not free.
   - **The new race this ordering opens up, and its fix:** a file can now legitimately exist on disk *before* the database has been told about it — the normal state during the gap between the rename below landing and the `UPDATE` committing. A sweeper that decides what to keep by asking "does the DB currently point at this file?" would, during exactly that gap, see a brand-new file the DB doesn't yet reference and correctly-but-wrongly conclude it is an orphan. Deleting it there is worse than the bug this reordering fixed, because it needs no crash at all — an ordinary sweep tick landing at an unlucky moment is enough, and at 10M-user scale, anything that *can* happen on a given upload *will* happen periodically. **Fix: any sweeper must skip every file younger than a fixed grace period (`AvatarSweepGracePeriod` in code, 10 minutes), checked via filesystem mtime, before ever consulting the database — unconditionally, regardless of what the DB says.** Ten minutes is two to three orders of magnitude longer than an upload plus its `UPDATE` could plausibly take, so it costs nothing in the common case while closing the race structurally rather than probabilistically. No sweeper exists in the codebase yet; this requirement is recorded here and as a named constant in code specifically so whichever one is written next cannot miss it.

4. **`row_version` / `avatar_version` conflation race (historical — kept as a record of why the two counters were designed separately; `avatar_version` no longer exists, see point 3a):** §4.1 decided **per-row** cache versioning: a single version number that bumps on *either* nickname update or avatar upload, used to build the `profile:<user_id>:v<N>` Redis cache key (`user:<username>` is unversioned — §5). `avatar_version` (this section, when it still existed) bumped **only** on avatar upload and was embedded in the served file's **name**, not just a cache key.
   - **If these were the same column:** a nickname-only update would bump `avatar_version`, and the next avatar URL built from `id + avatar_version + avatar_suffix` would point at `<id>_v<new_version>_<suffix>.jpg` — a file that was never written (only the DB counter moved, no upload happened) → broken avatar image, even though the user never touched their avatar.
   - **Fix:** keep them as two separate columns with two separate write paths:
     - `avatar_version` — bumped only inside the avatar-upload flow (step 2 below). Used only to derive the on-disk filename / URL.
     - `row_version` — bumped on *every* mutating write to the row (nickname update **or** avatar upload). Used only to build Redis cache keys per §4.1. Never appears in a filename or file path.
   - Both use the same atomic `col = col + 1` pattern (MySQL guarantees no interleaving on a single row), so bumping both in the same `UPDATE` statement is one round trip, not two.

**Chosen upload flow — file first, then a single-statement DB commit (see 3a above for why this order, and why it needed the version column gone to be possible):**
```
1. Generate 8-char random suffix in Go (crypto/rand) — no MySQL round trip
2. Ensure dir exists: /uploads/<user_id >> 14>/<user_id>/  (idempotent MkdirAll)
3. Write file to <dir>/<user_id>_<suffix>.jpg.tmp
4. os.Rename(tmp, final) — atomic on POSIX
   (Steps 1-4 touch no database at all. If anything fails here, nothing in
   MySQL has been told about this upload, so the row is untouched.)
5. MySQL:
      UPDATE users
      SET avatar_suffix = ?,
          row_version   = LAST_INSERT_ID(row_version + 1)
      WHERE id = ?
      → then SELECT LAST_INSERT_ID() to get new_row_version
   (One statement, one round trip.)
6. Old files become orphans; async sweeper cleans them up periodically,
   subject to the mtime grace period in 3a — no sweeper implemented yet.
7. [separate goroutine, after step 5 commits] DEL profile:<user_id>:v<old_row_version>,
   DEL user:<username> — per §4.1 delete-on-write. (Open question, not yet
   decided: whether `user:<username>` needs to be part of this at all once it
   no longer caches nickname/avatar data — see §4.1's discussion of trimming
   that cache to login-only fields. Left as both keys here to match what is
   actually implemented today.)
```
If step 5 fails, the file written in steps 2-4 is simply an orphan — swept up later, same as any other orphan — and the row is exactly as it was before the request. There is no ordering under which this flow can leave MySQL pointing at a suffix whose file does not exist.

**Nickname update flow, for symmetry (no file involved, so only `row_version` moves):**
```
1. MySQL: UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?
   → then SELECT LAST_INSERT_ID() to get new_row_version
2. [separate goroutine] DEL user:<username>, DEL profile:<user_id>:v<old_row_version>
```
No file is involved, so unlike the avatar flow there was never a version-vs-suffix
question here — `row_version` is the only atomic counter this statement touches, so it
can use `LAST_INSERT_ID` directly, same as the avatar flow does now that
`avatar_version` is gone (§4.4.4, point 3a).

**Why `row_version` stays in MySQL, not Redis:**
- Redis is for ephemeral state (sessions, rate-limit counters). Wiping Redis must not lose user data.
- `row_version` and `avatar_suffix` are durable properties of the user → MySQL only.

**Why store `avatar_suffix` as a separate column (not full path):**
- Smaller row: 8 bytes (`CHAR(8)`) vs ~40-100 bytes for full VARCHAR path — narrower still than the version-numbered scheme's 12 bytes (`INT` + `CHAR(8)`), now that there is no separate version column.
- At 10M rows → meaningful savings on the users table → more of the table fits InnoDB buffer pool.
- Decouples directory layout from data — can change `/uploads/<bucket>/<user_id>/` scheme without touching data.
- Path is deterministic from `(id, avatar_suffix)` → constructed server-side on each read.

**Orphan cleanup:**
- Inline delete-on-upload would require reading the previous suffix (extra round trip or `FOR UPDATE` locking).
- Chose async sweeper instead: periodic job walks `/uploads/<bucket>/<user_id>/` and deletes everything that is not the row's current `avatar_suffix` — **with one change from the original plan, forced by point 3a's reordering:** the sweeper can no longer tell which file is current by reading filenames alone (a random suffix carries no ordering, unlike a version number), so it now needs one MySQL read per swept user's `avatar_suffix`, where the version-numbered scheme needed none. It must also skip anything younger than `AvatarSweepGracePeriod` (10 minutes, checked via mtime) **before** consulting the database at all, or it can delete a file that is mid-upload — see point 3a for the exact race this closes. No sweeper is implemented yet; this is the specification for whoever writes it.
- Simpler, safer upload path in exchange; orphans still harmless (just disk), still cleaned up on the sweeper's next pass.

**Schema updates for §5:**
- ~~Add `avatar_version INT UNSIGNED NOT NULL DEFAULT 0`~~ — **added, then removed** (post-implementation decision, §4.4.4 point 3a): an atomic MySQL-generated version number forced the database write to happen before the file existed, which could leave the row pointing at a version whose file was never written. Replaced by identifying each upload purely by its random `avatar_suffix`, which needs no MySQL round trip to generate and so imposes no ordering constraint on when the DB is updated relative to the file write.
- Add `avatar_suffix CHAR(8) NULL` (nullable; NULL when no avatar). Now the *sole* signal for whether a user has an avatar — there is no version column to check alongside it.
- Remove `avatar_path` (path is now derived, not stored)
- Add `row_version BIGINT UNSIGNED NOT NULL DEFAULT 0` — per §4.1's per-row cache-versioning decision, bumped on nickname update **or** avatar upload; only ever read to build/tear down Redis cache keys, never used in a filename or path. Unaffected by the `avatar_version` removal above — the two counters were already deliberately separate (point 4 above), and dropping one does not touch the other.

---

#### 4.4.5 Access Control on Served URLs

**Question:** are avatars publicly accessible URLs or auth-gated?

**Considerations:**
- Server exposes an HTTP route (e.g., `/uploads/*`) that reads files from disk and returns them. That URL is reachable by anyone with a browser, regardless of DB state — the DB doesn't gate static-file serving.
- Making URLs "unguessable" (via random suffix) is the practical defense; it's NOT the same as authorization.
- Adding per-request auth (check "does this session own this file?") means every avatar view hits our auth path → hurts perf and defeats CDN caching.

**Decision: public URLs with unguessable filenames.**
- Matches Twitter/GitHub/Discord/etc. — avatars are treated as public data.
- Random suffix prevents enumeration.
- Allows aggressive HTTP caching: `Cache-Control: public, max-age=31536000, immutable` (URL changes on new upload, so infinite cache is safe).
- Deferred to future: CDN in front for production. ***[not implemented]***

---

**Production-scale escalation path** (mention in TDD):
- Small scale → filesystem (this task)
- Medium → shared NFS or object store (S3/OSS/COS/MinIO)
- CDN in front for read-heavy workloads
- Never BLOB in DB at scale

**Status:** Filename scheme, sharding, race handling, and access control all decided.

---

### 4.5 Framework Choice

**Decision: TBD — confirm Go is intended.** If yes, proceed with Go.

---

**HTTP framework options (assuming Go):**
- (A) **`net/http` (stdlib only)** — no external framework.
- (B) **`gin`** — popular, fast, middleware ecosystem.
- (C) **`echo`** — similar to gin.
- (D) **`fiber`** — fasthttp-based, very fast but non-stdlib HTTP.
- (E) **`chi`** — minimal, stdlib-compatible router.

**Considerations:**

| Aspect | `net/http` | `gin`/`echo` | `fiber` | `chi` |
|---|---|---|---|---|
| Perf | Excellent | Excellent | Highest raw | Excellent |
| Learning value | Highest (understand primitives) | Medium | Low | Medium |
| Ecosystem/middleware | Manual | Rich | Rich | Rich |
| Stdlib compatibility | ✅ | ✅ | ❌ | ✅ |
| Boilerplate | More | Less | Less | Little |
| Fit for entry task | ✅ Shows fundamentals | Common in industry | Risky (non-stdlib) | Good balance |

`net/http` only if we want maximum from-scratch learning value.
- The 3k-6k RPS target is easily hit by any of these; framework won't be the bottleneck.

**Decision:** `net/http` for the learning angle. refresh mainly. Does not lose out to the others other than boilerplate anyway. [believed most appropriate for entry task? on my end]

---

**TCP server options:**
- Just stdlib `net` — no framework needed. Custom protocol on top (see §4.6).

**Decision: `net` stdlib.** ✅

---

### 4.6 Connection Pooling

Three connection sources are needed. Two are custom; one uses a library.

| Store / Link | Pool Source | Why |
|---|---|---|
| MySQL | **Custom** (§4.6.0) | Task explicitly requires custom pooling |
| Redis | Library (`go-redis`) (§4.6.1) | Library exists, task doesn't require custom |
| HTTP → TCP | **Custom** (§4.7) | No library exists — we invented the protocol |

Custom in each case means different things:
- MySQL pool: traditional acquire/release, one-request-per-connection while checked out.
- HTTP↔TCP pool: **multiplexed** — many concurrent requests share each connection via `req_id` correlation. Fewer connections needed.

---

#### 4.6.0 MySQL Connection Pool (Custom)

**Requirement:** build our own. **Key question:** what does "custom" actually mean here — how far down do we go?

**Options — layer to implement:**

- **(A) Wrap `database/sql`'s conn pool** — use `sql.DB` under the hood, add our own queue/checkout on top.
  - **Concern:** `database/sql` already IS a connection pool. Wrapping it is arguably not "building our own" — it's double-pooling.
  - Reviewer may say: "you're just re-exposing the built-in pool."

- **(B) Use `database/sql` with `MaxOpenConns=1` per handle, pool the handles ourselves.**
  - Awkward; fights the library.

- **(C) Use a MySQL driver at the protocol level (`go-sql-driver/mysql`'s `Connector`), get individual raw MySQL connections, manage the pool ourselves.**
  - True custom pool. Driver handles wire protocol; we handle lifecycle, checkout, health, prepared statements. [what would need to be done]
  - Reasonable interpretation of the requirement. [i think]

- **(D) Full DIY** — implement the MySQL wire protocol ourselves over `net.Conn`.
  - Way out of scope for an entry task. [i think?]

**Which does the task mean?**
- Task says: *"Connect by MySQL Go client. **Connection Pool should be built on your own.**"*
- Most reasonable read: use the driver (Go MySQL client) for wire protocol, but the pool logic — acquire, release, size limits, waiters, health — is the task ask. That's **Option C**.

**Considerations:**
- Pool size: too small → contention. Too big → MySQL connection limit (default 151), memory pressure.
- For 3k-6k RPS with fast queries (<5ms each), rule of thumb: pool_size ≈ p99_latency_ms × RPS / 1000. So 5ms × 6000 / 1000 = 30 connections. Pool of 50-100 is comfortable.
- Must support:
  - Bounded max open connections
  - Idle connection reuse (LIFO or FIFO)
  - Wait queue with timeout (avoid unbounded blocking → return error under overload)
  - Health check / stale-connection eviction (ping, max lifetime, max idle time)
  - Per-connection **prepared statement cache** (MySQL prepares are per-connection)
  - Graceful shutdown (drain in-flight)
- Metrics: in-use, idle, wait count, wait duration, evictions — for tuning. [if time permits]

**Concurrency model in Go:**
- Channel of idle connections + `sync.Mutex` for size tracking, OR
- Semaphore (buffered channel) for capacity + slice for idle list.

**Decision: Option C — direct driver connections, custom pool with prepared statement cache.** 

**Sub-decisions TBD:**
- Max pool size (start 50, tune during perf test).
- Wait timeout (e.g. 200ms → return 503).
- Health-check interval / max conn lifetime.
- Idle policy (LIFO for warmer connections, FIFO for even wear).

---

#### 4.6.1 Redis Connection Pool

**Does the "custom pool" rule apply to Redis too?**

Re-reading the task: *"Connect by MySQL Go client. **Connection Pool should be built on your own.**"* — the "build your own" clause is specifically under the MySQL bullet. Redis is mentioned separately (*"Cache should use Redis"*) with no custom-pool requirement.

**Decision: use `redis/go-redis/v9`'s built-in pool.** We focus custom-pool learning where it's specifically required (MySQL, §4.6.0). Optional stretch: build a matching custom pool for Redis if time permits after primary goals are met.

**Trade-off analysis:**

| Approach | Pros | Cons |
|---|---|---|
| **Use `go-redis` library pool (chosen)** | Battle-tested, handles reconnection/pipelining/cluster/timeouts. Zero risk. Frees time for other work. | Less "custom code" to show. Consistent API differs from our MySQL pool. |
| Build custom Redis pool | Learning consistency, deeper practice, one abstraction across both stores. Redis pool is actually simpler than MySQL (no prepared stmts, no transactions). | ~1 extra day. Risk of subtle bugs (RESP edge cases, reconnection storms). Duplicates existing well-tuned library. |
| Middle: thin wrapper around `go-redis` | ~2 hours. Get consistent API + custom metrics/timeouts across both stores. | Not truly custom; still relying on library for the hard parts. |

**Not choosing "build custom" is deliberate choice:**
- MySQL pool is the interesting one (prepared statements, driver-level integration, protocol nuance).
- Redis pool is mechanically similar but simpler → less learning per hour spent.
- Battle-tested library removes risk from perf-critical path.
- Task explicitly draws the line at MySQL.

**Recommended settings (tune during perf):**

| Setting | Default | For our workload | Why |
|---|---|---|---|
| `PoolSize` | 10 × NumCPU | 50-100 | Same sizing math as MySQL: `p99_latency × RPS / 1000` |
| `MinIdleConns` | 0 | 10-20 | Keep warm connections for burst traffic |
| `MaxConnAge` | 0 (unlimited) | 30 min | Rotate stale connections defensively |
| `PoolTimeout` | 4s | 200ms | Fail fast under overload → return 503 |
| `ReadTimeout` / `WriteTimeout` | 3s | 500ms | Redis is fast; long timeouts mask bugs |
| `IdleTimeout` | 5 min | 5 min | Default fine |

**Pipelining opportunity (perf optimization):**

At 6k+ RPS, batching multiple Redis commands per round trip is a real win. Example login flow:
```
Login handler needs:
  1. GET user:<username>      (cache lookup)
  2. INCR rl:login:ip:<ip>    (rate limit counter) [rate limiting not implemented]
  3. SETEX sess:<token>       (create session)
```

Un-pipelined: 3 round trips (~0.6 ms). Pipelined: 1 round trip (~0.2 ms). At scale this frees up significant time budget.

`go-redis` supports pipelines natively. May consider using where multiple commands are needed in one handler.

**Health checking:**
- `go-redis` handles this automatically (PING before use if connection idle beyond threshold).
- We just configure timeouts; don't need custom health-check logic.

---

### 4.7 HTTP ↔ TCP Protocol

**Scope clarification:** this section is about the link between the **HTTP server process and the TCP server process** (the two processes required by the task). Not about clients — client connections to the HTTP server are handled by `net/http` normally with keep-alive.

**Why pool this link at all?**

Every incoming HTTP request needs to reach the TCP server for business logic. If the HTTP server opened a fresh TCP connection per request:
- 6k RPS × ~1ms TCP handshake = ~6 seconds of setup overhead per wall second (i.e., unusable)
- Ephemeral port exhaustion (~65k ports, minute-long TIME_WAIT)
- Backend also has to handshake every time

With a small pool of persistent connections (8-16) and **multiplexing** (send request with `req_id`, match response by `req_id`), we amortize the handshake and share connections across concurrent requests. Same pattern as any HTTP/2 client, DB driver pool, etc.

**Options:**
- (A) **Length-prefixed binary frames** — `[4-byte length][1-byte msg_type][payload]`, payload is JSON or protobuf.
- (B) **JSON over TCP** (newline-terminated: `{req_id, op, data}\n` each way). Two ways to implement the read side:
  - **(B1) True JSON-lines** — reader uses `bufio.Scanner`/`bufio.Reader` split on `'\n'`, then `json.Unmarshal`s each line independently. The newline is the actual, enforced frame boundary; a scanner buffer size (`sc.Buffer(nil, maxFrameBytes)`) gives a hard per-frame size cap for free.
  - **(B2) Streaming `json.Decoder`** — writer uses `json.NewEncoder(w).Encode()` (which happens to emit a trailing `\n`, but only as a courtesy for humans running `nc`); reader uses `json.NewDecoder(r).Decode()` repeatedly on the raw byte stream. Framing comes from JSON's own self-terminating grammar (matched braces), not from the newline — the decoder never looks for `'\n'` at all.
- (C) **gRPC** (technically over HTTP/2, may violate "TCP server" constraint).
- (D) **HTTP over the TCP link** (defeats the purpose of the constraint).
- (E) **WebSocket.** Rejected for the same reason as (C)/(D): a WebSocket connection begins with an HTTP/1.1 `Upgrade: websocket` handshake, so the link would still be doing an HTTP exchange before any "real" traffic starts — not a real TCP protocol in the spirit of the constraint. It also solves a problem we don't have (framing) while adding one we don't need: mandatory client→server frame **masking**, exists purely to stop malicious browser JS from smuggling bytes through misbehaving HTTP proxies/caches — irrelevant between two Go processes we control. Still doesn't give multiplexing for free; `req_id` would have to be carried in the payload exactly as in (A)/(B). Needs an external library (`gorilla/websocket` or similar), same "not truly DIY" objection as (C).

**Considerations:**
- Task explicitly says HTTP and TCP are separate — should be a real TCP protocol, not HTTP-in-HTTP.
- Binary framing is fast, unambiguous, easy to parse, but adds implementation time.
- JSON payload is easy to debug; protobuf is faster + smaller but adds tooling.
- gRPC is battle-tested but its HTTP/2 substrate is arguably "not TCP" — risky interpretation. (on review now, i should have clarified)
- WebSocket has the same "not really TCP" issue as gRPC/HTTP-over-TCP, plus browser/proxy-oriented overhead (masking, ping/pong, close handshake) that buys nothing server-to-server.
- Need to handle: request/response correlation, connection reuse, timeouts, framing errors.
- B1 vs B2 trade-off: B1's explicit `'\n'` boundary gives a well-defined resync point if a frame is malformed (skip to the next newline and keep going) and a natural max-frame-size cap via the scanner buffer. B2 has neither — `internal/protocol/frame.go` notes exactly this: once `json.Decoder` desyncs on unexpected bytes, "it will keep reading wrong... and get stuck infinitely" rather than recovering, and `MaxFrameBytes` is currently unused because the decoder has no size ceiling to hand it to.

**Decision: (B2) Streaming `json.Decoder`/`json.Encoder` over TCP — for v1, this is what's implemented.**

Reasoning:
- Chosen over B1 purely for ease of implementation — `encoding/json`'s `Decoder`/`Encoder` already handle buffering and value boundaries, so there was no `bufio.Scanner` + line-splitting + per-line `Unmarshal` to write by hand. Goal was getting the HTTP↔TCP link working end-to-end first.
- Meets all requirements: real TCP protocol, custom framing, custom multiplexing via req_id.
- Perf is fine for our 6k RPS target; JSON parse overhead is negligible compared to bcrypt/DB.
- Debuggable: can `nc` to the TCP server and type JSON to test (the trailing `\n` from `Encode` makes manual typing convenient, even though the decoder doesn't require it).
- Preserves the interesting design work (multiplexing over pooled connections) without spending time on binary encoding details.

**Known follow-up (B2's resync/size-cap gap):** since correctness got priority over hardening for v1, a malformed frame can desync a connection with no defined recovery point, and there's no enforced max-frame-size. If revisited: either switch the read side to B1 (scanner + explicit buffer cap) without touching the write side, or move straight to (A) length-prefixed binary frames, which also removes the ambiguity around JSON's un-bounded frame size.

**Deferred / stretch:**
- Upgrade to **(A) length-prefixed binary frames** if time permits after perf targets are met. Binary framing:
  - Removes JSON parsing from hot path
  - Gives us practice with real protocol design
  - Also fixes B2's missing resync point and size cap (see above)
- Not on the critical path for the entry task; shelved unless there's time.

**Message shape:**
```
Request:  {"req_id": <int>, "op": "<string>", "data": {...}}\n
Response: {"req_id": <int>, "status": "ok"|"err", "error": "...", "data": {...}}\n
```
The trailing `\n` above is what `json.Encoder.Encode` emits; it's cosmetic for B2 (the decoder ignores it as whitespace) and would only become load-bearing if the read side were switched to B1.

Ops we'll need:
- `signup`, `login`, `logout`
- `get_user`, `update_nickname`, `update_avatar`
- (any others as we discover them)

**Multiplexing:**
- HTTP server maintains a `map[req_id] chan Response` for pending requests.
- On send: allocate req_id, register channel, write frame.
- On receive (single reader goroutine per connection): read frame, look up channel by req_id, deliver response, remove entry.

**Sub-decisions (tune during perf):**
- Number of persistent TCP connections HTTP→TCP: start with 8. [on review, not bottleneck so 8 can stay as 8 for now]
- Per-request timeout: 2s?
- Idle connection health check / reconnect on failure. [maintained connection for reconnect is implemented but health check pings for idle connection not implemented as of now]

---

### 4.8 Rate Limiting [rough design but not implemented]

**Options:**
- (A) Redis counter per IP + per username, sliding window / fixed window.
- (B) Token bucket in-memory on TCP server.
- (C) None (rely on network layer).

**Considerations:**
- Login is the abuse target → rate limit at TCP server (auth authority).
- Redis lets us share state if we ever scale to multiple TCP servers.
- Fixed window is simpler (`INCR` + `EXPIRE`); sliding is more accurate but more ops.

**Proposed thresholds (when implemented):**
- Per IP: 20 failed logins / 5 min → 429.
- Per username: 5 failed / 15 min → temporary lock.
- Successful login resets the username counter.

**Decision: shelved for now. Redis fixed-window counters when implemented.**

**Rationale for shelving:**
- Correctness cost: zero — nothing depends on rate limiting.
- Security cost: no brute-force protection on login. Acceptable for a localhost entry task; noted as production future work.
- Perf argument (short-circuit bcrypt on failed attempts) is speculative; only helps if the load harness triggers rate limits, which likely won't happen with valid credentials.
- Time cost of implementing it competes with the core deliverables (MySQL pool, HTTP↔TCP protocol, perf tuning).

**Revisit trigger:**
- If have slack after primary goals met.
- If perf tuning reveals bcrypt-on-invalid-attempts as a real bottleneck.
- If security is called out during review.

**When implemented, plumbing points:**
- TCP server login handler: check `rl:login:ip:<ip>` and `rl:login:user:<username>` before bcrypt.
- Redis atomic ops: `INCR key; EXPIRE key ttl; if count > threshold: return 429`.
- On successful login: `DEL rl:login:user:<username>`.

**Status: shelved (documented, not implemented).**

---

### 4.9 CSRF Protection

**Options:**
- (A) `SameSite=Lax` (or `Strict`) session cookie only.
- (B) Double-submit CSRF token (cookie + header).
- (C) Both (defense in depth).

**Considerations:**
- Cookie-based auth is CSRF-vulnerable by default.
- `SameSite=Lax` covers most cases in modern browsers (95%+ browser share).
- Double-submit adds a second layer without server-side token storage.
- Profile edit is the only mutating authenticated action → limited attack surface.

**Cost analysis:**

| Approach | Cost | Protection |
|---|---|---|
| SameSite=Lax alone | **One line** (cookie attribute) | ~95% of CSRF attacks blocked in modern browsers |
| Add double-submit token | ~half day (token gen + middleware + client wiring) | Covers old browsers, defense-in-depth |

**Decision: (A) SameSite=Lax only. Double-submit token shelved for now.**

Reasoning:
- SameSite=Lax is essentially free (one line when setting the cookie) and blocks the vast majority of CSRF attacks in modern browsers.
- Double-submit tokens add real work (~half day) for marginal additional protection.
- Profile edit is the only mutating endpoint that matters — limited attack surface.
- Reviewer sees SameSite set correctly → understands we thought about CSRF; not naked.

**Cookie attributes locked in:**
```go
http.Cookie{
    Name:     "session",
    Value:    token,
    HttpOnly: true,                  // JS cannot read
    SameSite: http.SameSiteLaxMode,  // cross-site POST won't include it
    Secure:   false,                 // true in production
    Path:     "/",
}
```

**Status: ✅ SameSite=Lax locked in. ⏸ Double-submit token shelved.**

**Revisit trigger for double-submit token:**
- If Days 11-14 have slack after primary goals met.
- If security review specifically flags it.

**Status: TBD** on `Strict` vs `Lax` (Strict breaks external links to the site; Lax is the pragmatic default).

---

### 4.10 Input Validation

**Rules:**
| Field | Rule |
|---|---|
| username | `^[a-zA-Z0-9_]{3,32}$`, ASCII, lowercase-normalized |
| password | min 8 chars, max 72 (bcrypt limit), reject all-same-char |
| nickname | valid UTF-8, 1-32 **runes** (not bytes), strip control chars |
| image | ≤ 2 MB, MIME sniff, allowlist: jpeg/png/webp |

**Enforcement:** both HTTP server (early reject) and TCP server (source of truth).

**Decision: as above.** 

- not sure if auth middleware to check for presence of cookie is counted as input validation as well but is present too, BUT not really "input"

---

### 4.11 SQL Injection Prevention

**Approach:** parameterized queries only. Never string-concat SQL.

```go
// GOOD
db.Query("SELECT id, nickname FROM users WHERE username = ?", username)
```

**Enforcement:** code review + optional `grep` for `fmt.Sprintf.*(Query|Exec)`.

**Decision: enforced by discipline + review.** ✅

---

### 4.12 Prepared Statements

**Where:** per-connection cache inside the custom pool (see §4.6).

**Why:** MySQL prepares are per-connection; caching them saves parse cost on every query. At 3k-20k RPS this is meaningful.

**Common statements to prepare** (no `avatar_version` — removed, §4.4.4 point 3a):
- `SELECT id, password_hash, nickname, avatar_suffix, row_version FROM users WHERE username = ?`
- `SELECT id, nickname, avatar_suffix, row_version FROM users WHERE id = ?`
- `UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`
- `UPDATE users SET avatar_suffix = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?`
- `INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)`

**Decision: prepare-on-first-use per connection, cache for connection lifetime.** ✅

---

### 4.13 TLS

**Decision: ⏸ Shelved — production concern, not implemented in code for this task.**
- Test harness is localhost; HTTP is fine.
- Production notes: HTTPS at HTTP server or LB, `Secure` flag on cookie, optionally mTLS on HTTP↔TCP link.
- Revisit trigger: if deploying beyond localhost.

---

## 5. Data Model

### `users` table

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,      -- bcrypt output is 60 bytes
  nickname       VARCHAR(128) NOT NULL,       -- 32 runes × up to 4 bytes
  avatar_suffix  CHAR(8)      NULL,           -- 8 hex chars (32 bits random); NULL when no avatar
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,  -- bumped on ANY update (nickname or avatar); cache-key versioning only, see §4.1/§4.4.4
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

There is no `avatar_version` column. An earlier iteration of this schema had
one; it was added, then removed once implemented — see §4.4.4 point 3a for
the full account of why (short version: it forced the database write to
happen before the file existed, which could leave a row pointing at a file
that was never written; a random suffix alone needs no MySQL round trip to
generate, so removing the counter is what let the write order become
file-first-then-database instead).

**Notes:**
- `utf8mb4` for full unicode (emojis in nicknames etc.).
- `utf8mb4_bin` collation → case-sensitive username; use lower-cased username for lookups if we want case-insensitive.
- Unique index on `username` for O(log n) lookup at 10M rows.
- `VARBINARY(60)` for bcrypt (fixed size).
- Avatar path is **derived**, not stored: `/uploads/<id >> 14>/<id>/<id>_<avatar_suffix>.jpg` (see §4.4). `avatar_suffix` being NULL is the sole signal for "no avatar" — there is no version to check alongside it.
- Row footprint is smaller than the version-numbered scheme (one fewer `INT` column) as well as than storing a full path — meaningful at 10M rows for buffer pool efficiency.
- `row_version` is unrelated to the avatar scheme above, and unaffected by removing `avatar_version` — the two were already deliberately separate counters (§4.4.4 point 4). `row_version` exists purely to build/tear down Redis cache keys per §4.1's per-row versioning decision, and never appears in an avatar filename.

### Redis keys

| Key | Value | TTL |
|---|---|---|
| `sess:<token>` | JSON `{user_id, username, row_version, created_at, ip?, last_seen}` | 24h sliding |
| `user:<username>` (NOT versioned — see note below) | JSON `{id, password_hash, nickname, avatar_suffix, row_version}` | 10 min |
| `profile:<user_id>:v<row_version>` | JSON `{nickname, avatar_suffix}` | 1 h |
| `rl:login:ip:<ip>` | counter | 5 min |
| `rl:login:user:<uname>` | counter | 15 min |

**Why `user:<username>` is NOT versioned, unlike `profile:<user_id>`:** a versioned
key needs the caller to already know which version to ask for *before* forming the
GET — otherwise caching buys nothing, since learning the version would itself cost
a lookup. For `profile:<user_id>` that is free: the session record is being read on
every authenticated request anyway (to resolve identity), so it carries a
`row_version` hint at near-zero marginal cost. Login has no such source — it is,
by definition, the *first* touch of Redis for this identity, before any session
exists — so versioning this key would force either a second Redis round trip or a
MySQL read on every single login, defeating the cache that matters most for the
login-RPS targets. `row_version` is still carried inside the *value* here (not the
key), purely so a cache-hit login can seed the new session's hint accurately rather
than always seeding 0 and guaranteeing one wasted miss on that session's first
profile read.

**Open question, not yet decided:** whether `user:<username>` should be trimmed to
only what login actually needs — `{id, password_hash, row_version}` — dropping
`nickname` and `avatar_suffix`, which nothing on the login path reads (`/api/me`'s
`nickname`/`avatar_url` come from `profile:<user_id>`, already fetched separately,
after login). If trimmed, nickname/avatar edits would no longer need to invalidate
`user:<username>` at all — only a password change would, since password would be
the only field left in it that can go stale. That in turn would remove `username`
from the arguments `InvalidateOnNicknameChange`/`InvalidateOnAvatarChange` need,
and raises a follow-on question covered below: whether the session record still
needs to carry `username`, or whether `profile:<user_id>` can supply it instead.

**Why `row_version` is embedded in the key, not the value** (per §4.1): the previous version's key becomes unreachable the instant `row_version` bumps in MySQL, without needing a delete to race against a concurrent read. The delete-on-write goroutine (§4.1, §4.4.4) still deletes the old key so it doesn't linger to TTL, but correctness doesn't depend on that delete winning any race.

**Cache-key design rationale:**

- **Not per-field caching.** Every hot-path read (login, `/api/me`) needs most fields together — per-field GETs would multiply Redis ops for the same data. Single JSON blob per logical access pattern is faster.
- **Two keys per user, not one.** Splitting by access pattern:
  - `user:<username>` — indexed by username for login (contains password_hash).
  - `profile:<user_id>` — indexed by user_id for `/api/me` (no hash needed).
- **Split benefits:**
  - Different TTLs (password hash long-lived, profile shorter since edits are more frequent).
  - Cleaner invalidation: profile edits invalidate only `profile:<user_id>`; login-critical `user:<username>` stays hot.
  - Different index keys naturally — username lookup for login, id lookup for profile.
- **`sess:<token>` and rate-limit keys** are separate concerns (session state, counters); no user data leaks in them.

**Why session stores `username` alongside `user_id` (current implementation; see the open question above — this whole rationale is what's under review):**

Every authenticated request must look up the session in Redis for auth (verify token exists, get identity). By storing **both** `user_id` and `username` in the session record, we get both identifiers in one Redis GET. As implemented today, this matters for cache invalidation on profile edits:

```
Nickname update flow:
  1. Redis GET sess:<token>  → {user_id: 42, username: "alice", row_version: 5, ...}
  2. MySQL UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = 42
     → new_row_version returned directly (old_row_version = new_row_version - 1; exact, not a guess)
  3. Redis DEL profile:42:v<old_row_version>     (uses user_id from session)
  4. Redis DEL user:alice                        (uses username from session; NOT versioned — see above)
```

Without storing username in the session, step 4 would require an extra MySQL query to look up the username. The ~20 extra bytes per session record is negligible; the extra query is not — *if* step 4 stays as it is. But step 4 only exists because `user:<username>` currently caches `nickname` and `avatar_suffix` alongside the password hash: if it is trimmed to login-only fields (the open question above), a nickname or avatar edit has nothing left in `user:<username>` to invalidate, step 4 disappears entirely, and `username` stops being something invalidation needs from the session — the only remaining consumer would be formatting the API response in `get_user`, which `profile:<user_id>` could supply just as well if `username` were added to that cache's value instead. That would make the session record's job purely "who is this and are they still logged in", with no double duty. Whether that is a net improvement, or just moves the same fact to a different place, is exactly the open question above.

See §4.4.4 for why `row_version` is a separate counter from the (now-removed) `avatar_version` and why it's the one embedded in the `profile:` key.

### User table: no vertical split

**Single `users` table, all columns together.** Considered vertical partitioning (splitting hot/cold columns into separate tables) but rejected:
- Row width is ~134 bytes (~130 before `row_version` was added, minus the ~4-byte `avatar_version` `INT` removed in §4.4.4 point 3a) → 10M rows still fits comfortably in typical buffer pool (~1.3-1.4 GB). Neither change moves the needle on this decision.
- All columns are accessed together on the hot path (login pulls hash + nickname + `avatar_suffix` + `row_version` — the latter needed to build the Redis cache key, not just for the row's own data).
- Splitting would add JOIN cost on every read for no memory or lock-contention benefit.
- Query-level column selection (`SELECT` only what you need) already handles per-request data minimization.
- Vertical partitioning would only pay off with rows >1 KB or clearly bimodal hot/cold access, neither of which applies.

**Status: TBD** — finalize schema after §4.1 sub-decisions.

---

## 6. API — HTTP Interface

| Method | Path | Purpose | Auth |
|---|---|---|---|
| GET  | `/` | Landing (login/signup UI) | No |
| POST | `/api/signup` | Create user | No |
| POST | `/api/login` | Authenticate, set cookie | No |
| POST | `/api/logout` | Destroy session | Yes |
| GET  | `/api/me` | Current user info | Yes |
| POST | `/api/profile/nickname` | Update nickname | Yes + CSRF |
| POST | `/api/profile/avatar` | Upload avatar | Yes + CSRF |
| GET  | `/uploads/<path>` | Serve avatar (public, unguessable filename via random suffix) | No |
| GET  | `/default.png` | Default avatar for users who haven't uploaded one (static, shared) | No |

**Note:** avatar URLs are publicly accessible but non-enumerable due to the random suffix in the filename. See §4.4.5 for rationale. The default avatar is a single shared static asset served when `avatar_suffix` is unset (NULL).

---

## 7. Security Summary

| Threat | Mitigation | Status |
|---|---|---|
| Password theft from DB dump | bcrypt cost 5 (scaled down from cost 10 to hit the login-RPS targets, §4.3), per-user salt | ⚠️ Weaker margin than recommended cost 10-12 — a DB dump is cheaper to brute-force offline — but still salted (no rainbow tables) and stronger than unsalted SHA-256 |
| Session hijack | HttpOnly + SameSite cookie, Redis-backed, TTL | ✅ |
| Brute-force login | Redis rate limit per IP + per user | ⏸ Shelved (revisit if time permits) |
| CSRF | SameSite=Lax cookie (double-submit token shelved) | ✅ (Lax) / ⏸ (token) |
| SQL injection | Parameterized queries only | ✅ |
| Malicious file upload | Size cap, MIME sniff, random filename, no exec on upload dir | ✅ |
| Enumeration (which users exist) | Uniform error message + dummy bcrypt verify on unknown username (constant-time, see note below) | ✅ |
| MITM | TLS termination | Production only |
| DoS | Rate limit + pool caps + timeouts | Partial |

**Note on the bcrypt-cost / enumeration interaction:** bcrypt embeds its cost in the hash itself, so a login's actual speed is set by the cost the row was **seeded** at (5, per §4.3), not by the server's `BCRYPT_COST` config — that config only governs hashing a *new* signup and the dummy verify above. Keeping `BCRYPT_COST` matched to the seeded cost is what makes the dummy-verify mitigation actually constant-time; measured mismatch (real rows at cost 5, dummy verify at cost 10) would make a real login ~4.5ms and a fake one ~65ms — a 14x timing gap that leaks exactly which usernames exist, defeating the row above.

---

## 8. Performance Plan

### Bottleneck predictions
1. **bcrypt hash** on login — mitigate via session caching (subsequent requests skip hash).
2. **MySQL row lookup** — mitigated by unique index + cache.
3. **HTTP↔TCP RTT** — mitigate via connection pooling and multiplexing.
4. **Filesystem writes** — not on hot path.

### Test methodology
- Seed 10M users into MySQL directly (bulk insert with `LOAD DATA` or generated INSERTs, batched).
- Warm cache for the "with cache" test.
- Run load with `wrk` / `vegeta` / `hey` — ≥200 unique users cycling.
- Measure: RPS, p50/p95/p99 latency, error rate (target 0%), socket/timeout count.
- Tune: pool size, Redis TTL, GC settings (`GOGC`), TCP tuning if needed.

### Optional stretch (10k / 20k)
- Prepared statement cache
- Redis pipelining
- HTTP keep-alive tuning
- Bigger pool + more TCP server workers

### Profiling & Binary-Level Measurement

Beyond RPS/latency, we want to profile the running binary at the process/CPU/memory level. This is both a good practice for production perf work and useful practice for cross-language profiling (transfers to C++).

**What to measure:**

| Category | Metric | Why |
|---|---|---|
| CPU | On-CPU time per function, flame graph | Find hot functions (bcrypt? JSON encode? mutex?) |
| CPU | Cache hits/misses (L1/L2/LLC) | Data locality — hot structs, false sharing |
| CPU | Branch mispredictions | Hot code with unpredictable branches |
| CPU | IPC (instructions per cycle) | General efficiency indicator |
| CPU | Context switches, run-queue latency | Scheduler pressure, over-parallelism |
| Memory | RSS, heap size, alloc rate | Leaks, GC pressure |
| Memory | Alloc/sec, alloc bytes/sec | Reduce with pooling (sync.Pool), pre-allocated buffers |
| Memory | GC pause p99, GC cycles/sec | Tune `GOGC`, reduce allocs |
| Threads/goroutines | Count over time, blocking profile | Leaks, contention |
| Locks | Contention profile (mutex/rw wait time) | Pool mutex, cache mutex, map contention |
| Syscalls | syscalls/sec, top syscalls | I/O patterns, hidden overhead |
| Network | Sockets in various states, backlog | HTTP/TCP tuning |

**Tools (Go, on Linux):**

| Tool | What it gives |
|---|---|
| `net/http/pprof` (built-in) | CPU, heap, goroutine, mutex, block profiles — via HTTP endpoint |
| `go tool pprof` | Analyze/visualize profiles, flame graphs |
| `runtime/trace` + `go tool trace` | Goroutine scheduler timeline, GC, syscalls |
| `perf` (Linux) | Hardware counters: cache misses, branch mispredicts, IPC |
| `perf record -g` + FlameGraph | System-wide flame graph (kernel + user) |
| `bpftrace` / `bcc` | Ad-hoc kernel-level tracing (I/O latency, TCP retransmits) |
| `strace -c` | Syscall summary (quick sanity check, not for prod) |
| `top` / `htop` / `pidstat` | Basic RSS, CPU, context switches |
| `vmstat`, `iostat` | System-wide memory/IO pressure |
| `ss -tinm` | TCP socket details (rtt, cwnd, retrans) |

**Tools that transfer to C++:**
- `perf` — identical usage
- `perf` hardware counters — identical
- FlameGraph tooling — identical
- `bpftrace` / `bcc` — identical
- `valgrind` (cachegrind, callgrind) — C++-specific but same mental model as pprof
- Intel VTune / AMD µProf — C++-focused, similar concepts

**Profiling methodology:**
1. **Baseline**: run perf test, capture CPU + heap profile + `perf stat` (IPC, cache misses, branch mispredicts).
2. **Identify top consumers**: flame graph → biggest boxes = biggest wins.
3. **Hypothesize**: e.g. "JSON marshal is 30% of CPU → try pre-allocated encoder."
4. **Change one thing**, re-run, compare.
5. **Iterate** until diminishing returns or target RPS met.
6. **Document**: each optimization → before/after numbers.

**Specific things to look for in this system:**
- **Hot path (login):** allocation per request? JSON marshal overhead? mutex contention on the pool checkout?
- **Cache locality:** are pool structs cache-line-aligned? false sharing between counters?
- **Branch predictability:** hot inner loops predictable?
- **GC pressure:** allocs per login? Can we reuse buffers with `sync.Pool`?
- **Goroutine explosion:** one goroutine per HTTP request is fine; one per TCP frame might not be.
- **Syscall count:** are we doing extra `read`/`write` due to small buffers?

**Perf test binary instrumentation checklist:**
- [ ] Enable pprof endpoint on both HTTP and TCP server (localhost-only!)
- [ ] Build with `-gcflags="all=-N -l"` for a debug build alongside the release build (for symbol resolution in perf)
- [ ] Ensure symbols are not stripped in the perf binary
- [ ] Have a script that captures: `pprof cpu 30s`, `pprof heap`, `perf stat -e cycles,instructions,cache-references,cache-misses,branch-misses`, `perf record -F 99 -g`

**Deliverable:** a `perf/` folder with before/after profiles and a short write-up of each optimization.

**Note:** if we go with a language other than Go, the pprof/trace tools change but `perf`, FlameGraph, bpftrace, and the methodology all remain the same.

---

## 9. Open Questions / Still TBD

- [ ] **Language: confirm Go** (§4.5) — inferred from "MySQL Go client" but not explicit
- [ ] **HTTP framework**: `net/http` vs `chi` vs `gin` (§4.5)
- [ ] Session TTL & sliding-vs-fixed (§4.2)
- [ ] What exactly to cache and cache TTLs (§4.1)
- [ ] Custom pool sizing + idle policy (§4.6)
- [ ] TCP protocol connection pool size + timeouts (§4.7)
- [ ] `SameSite=Lax` vs `Strict` (§4.9)
- [ ] bcrypt cost — confirm after seeing perf harness behavior (§4.3)
- [ ] Trim `user:<username>` to login-only fields (`id`, `password_hash`, `row_version`), dropping `nickname`/`avatar_suffix` since nothing on the login path reads them (§5's Redis key table). Follow-on if trimmed: whether nickname/avatar invalidation still needs `user:<username>` at all, and whether the session record still needs to carry `username` or can rely on `profile:<user_id>` for it instead (§5, "Why session stores `username`").

**Resolved:**
- ✅ Filename scheme + directory sharding (§4.4) — `<user_id>_<suffix>.jpg` (no version segment — `avatar_version` was tried, then removed post-implementation, §4.4.4 point 3a) in `/uploads/<user_id >> 14>/<user_id>/` (fixed-size range partitioning, extends to hybrid pattern on horizontal scaling)
- ✅ Avatar URL public with unguessable filename (§4.4.5)
- ✅ Redis pool via library (`go-redis`), not custom (§4.6.1)
- ✅ HTTP↔TCP framing: newline-delimited JSON for v1; binary framing shelved (§4.7)

**Shelved (documented, not implemented; revisit if time permits):**
- ⏸ Rate limiting (§4.8) — brute-force protection deferred
- ⏸ Length-prefixed binary framing (§4.7) — custom binary header for HTTP↔TCP protocol
- ⏸ Double-submit CSRF token (§4.9) — SameSite=Lax covers ~95% in modern browsers

---

## 10. Future Work / Production Considerations

- Migrate images to object storage (S3/OSS/COS/MinIO) + CDN
- TLS everywhere (HTTPS, mTLS between servers)
- Horizontal scale: multiple TCP servers behind an internal LB; Redis Cluster; MySQL read replicas
- Observability: structured logs, Prometheus metrics, distributed tracing
- Zero-downtime deploys, graceful shutdown, health checks
- Password reset flow, email verification, 2FA
- Argon2id upgrade path for password hashing
- Audit log for sensitive actions

---

## 11. Deliverables Checklist

- [ ] `system_design.md` (this doc)
- [ ] Technical Design Document on Confluence (mirrors this doc)
- [ ] Repository with HTTP server, TCP server, seeder, and setup instructions
- [ ] Test cases (unit + integration)
- [ ] Performance test results (without cache, with cache; baseline + stretch)
- [ ] `perf/` folder: profiles, flame graphs, before/after write-ups

---

## 12. Session Handoff — Pick Up Here Next Time

**Where we stopped:** Design doc drafted. Locked-in decisions (✅) captured. TBDs deferred to next session.

**Next-session agenda (in suggested order):**

1. **Confirm language** (§4.5) — is Go actually required/expected?
2. **Confirm framework** (§4.5) — `net/http` vs `chi` vs `gin`?
3. Walk through remaining TBDs in §9:
   - Session TTL & sliding-vs-fixed
   - Cache contents + TTLs, invalidation policy, negative caching
   - Upload filename scheme + directory sharding
   - Custom pool sizing + LIFO/FIFO idle policy
   - HTTP↔TCP protocol details (framing, req_id, pool size)
   - Rate limit thresholds
   - SameSite Lax vs Strict
   - Avatar URL public vs auth-gated
   - bcrypt cost — needs perf-harness clarification
4. **After TBDs resolved:** scaffold the repository (Go modules, directory layout).
5. **Then:** DB schema + seeder script for 10M rows.
6. **Then:** custom connection pool skeleton.
7. **Then:** HTTP↔TCP protocol implementation.
8. **Then:** feature implementation (signup → login → profile edit).
9. **Then:** perf test rig + baseline measurements + profiling.

**Context to preserve for next session:**
- Task requirements (see §2)
- All ✅ locked-in decisions in §4
- Reasoning notes captured in this doc (options tables)
- Open TBDs in §9

**How to resume:** open `system_design.md`, review §9 (TBDs), start with the language question.
