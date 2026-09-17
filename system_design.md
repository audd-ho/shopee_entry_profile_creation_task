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
- **With cache:** 6k login RPS, same conditions. (Optional stretch: 20k RPS.)

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
                                          │ (10M+ rows)│ │         │
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
- Cache-aside is industry standard, gives us control over TTL, invalidation, cache key design.
- Task says "cache should use Redis" — implying standard Redis, cache-aside.
- Cache-aside directly enables the 3k→6k RPS jump.

**Decision: Cache-aside.** 

**Sub-decisions to make:**
- What do we cache? (username → password hash + user row? full profile? both?)
  - username to password hash since login is the performance requirement [profile image etc?]
- TTL? (Login lookups: ~5-10 min. Profile: longer, e.g. 1 hr.)
- Invalidation policy on profile edit: delete-on-write vs write-through?
- Negative caching (cache "user not found" for a short TTL to protect against enumeration/lookup spam)?

**Invalidation strategy — decided:**

Two options considered:

**(A) Delete-on-write (cache-aside invalidation):**
```
UPDATE users SET nickname = ? WHERE id = ?
DEL user:<user_id>
```
Next reader misses cache, refetches from DB, populates cache.

**(B) Write-through:**
```
UPDATE users SET nickname = ? WHERE id = ?
SET user:<user_id> <new record>
```
Cache always fresh; readers hit cache immediately.

**Race analysis:** write-through has a subtle bug with concurrent writers:
```
T=1: A: UPDATE nickname = "Alice"       T=2: B: UPDATE nickname = "Alicia"
T=3: B: SET user:42 {nickname: "Alicia"}
T=4: A: SET user:42 {nickname: "Alice"}   ← overwrites with older data
Result: DB = "Alicia", Cache = "Alice"  (divergent until TTL)
```
Delete-on-write is race-safe (deleting is idempotent; next read refetches fresh).

**Decision: Delete-on-write.** Reasons:
- Race-safe under concurrent writers
- Simpler (no need to serialize full record on every write)
- One extra DB read on next access — acceptable trade
- Consistent with "cache-aside" pattern from top of section

**Invalidation triggers (with the split cache — see §5):**

| Action | Keys to invalidate |
|---|---|
| Nickname update | `user:<user_id>` |
| Avatar update | `user:<user_id>` |
| Signup | `creds:<username>` (only relevant if negative caching implemented) |
| Logout | `sess:<token>` |
| Password change (future) | `creds:<username>` + all sessions for that user |

**Note:** profile edits touch only `user:<user_id>`. `creds:<username>` is untouched because password didn't change — its cached hash is still valid. This keeps the login hot path warm across profile edits.

**Negative caching note:** if we implement "user not found" negative caching, signup MUST invalidate that marker (`DEL creds:<newuser>`), else login for a just-created user would incorrectly report not-found for the TTL window. Currently deferred; add if enumeration attacks become a concern.

**Status: TBD** on remaining sub-decisions (TTLs, negative caching).

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

**Status: TBD** on TTL & sliding behavior.

---

### 4.3 Password Hashing

**Options:**
- (A) **bcrypt** (`golang.org/x/crypto/bcrypt`) — battle-tested, salt built-in.
- (B) **Argon2id** (`golang.org/x/crypto/argon2`) — OWASP-recommended, memory-hard.
- (C) SHA-256 + salt (weaker, faster).

**Considerations:**
- Never plaintext, never plain SHA.
- bcrypt cost 10 ≈ ~50-100ms/hash on modern CPU. Cost 12 ≈ ~250-400ms.
- Argon2id is stronger but slightly more complex to tune.
- **Perf impact:** login RPS test does 3k-20k login/sec. If every login re-hashes, at cost 10 we'd need ~150-1000 CPU cores just for bcrypt.
  - **Mitigation:** cache authenticated sessions in Redis; the RPS test likely reuses a small set of credentials → hashed password can be cached (as `username → bcrypt_hash`, verified in memory) OR test uses sessions after first login.
  - Need to clarify what the perf harness counts as a "login request".

**Decision: bcrypt, cost 10.**  (Simplicity, sufficient for this task.)

**Status: TBD** — confirm cost after seeing perf harness behavior.

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

**Decision: Filesystem, path in MySQL.** ✅

**Sub-decisions:**
- **Default avatar:** all new users start with `avatar_version = 0` and `avatar_suffix = NULL`. Server serves a shared `default.png` (static asset, not per-user) in this state. Once user uploads, `avatar_version` becomes ≥ 1 and their uploaded file is served instead.
- Max upload size: **2 MB**.
- Allowed types: jpeg, png, webp — sniff content, don't trust extension (see "Content sniffing" below).
- Write order: write file first (temp + atomic rename), then update DB row. On crash between the two → orphaned file (harmless, sweeper cleans up). Reverse order would leave DB pointing at nonexistent file (visible breakage).

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
- Browser sniffs responses (bad by default, disable with `nosniff` header)

---

#### 4.4.2 Filename Scheme — Options Considered

We considered several schemes; each has trade-offs:

| Scheme | Non-enumerable? | Cache-bust? | FS race? | Debuggable? | Extra state |
|---|---|---|---|---|---|
| `<user_id>.jpg` | ❌ | Manual (?v=X query) | Torn write on concurrent upload | ✅ | None |
| `<user_id>_v<n>.jpg` | ❌ | Automatic (URL changes) | ✅ Different file per upload | ✅ | version int |
| `<random>.jpg` | ✅ | Automatic | ✅ Different file per upload | ❌ | Full path in DB |
| **`<user_id>_v<n>_<suffix>.jpg`** | ✅ | Automatic | ✅ Different file per upload | ✅ | version int + suffix |

**Rejected: plain `<user_id>.jpg` (Option D "naive"):**
- Concurrent uploads write to the same file → torn writes (half file A, half file B) possible on filesystems.
- Even with atomic rename, no version = need manual cache-busting via query string.
- Enumerable, but that's acceptable for a public profile field.

**Rejected: plain random filename:**
- Solves non-enumerability but requires storing full path in DB (~40-100 bytes/row × 10M = ~1 GB).
- Loses debuggability (`ls` gives no info about which user owns which file).

**Chosen: `<user_id>_v<version>_<suffix>.<ext>`** — hybrid:
- **user_id**: readability, per-user grouping
- **version**: monotonic counter, natural cache-busting (URL changes per upload), atomically incremented in MySQL
- **suffix**: 8-char random hex (32 bits), makes URLs non-enumerable despite predictable IDs
- Each upload writes a **different file** → no FS race
- Only 12 bytes of extra state per user (INT + CHAR(8)) vs storing a full path

**Enumerability discussion:**
- Server exposes URLs, not the filesystem itself. Filesystem is on the server; nobody can `ls` remotely.
- BUT: an attacker with a browser can iterate URLs (`/uploads/0/10001/10001_v1_????????.jpg`).
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

**Final layout:**
```
/uploads/<user_id >> 14>/<user_id>/<user_id>_v<version>_<suffix>.jpg
```

Example for user_id = 12345, version = 9:
```
12345 >> 14 = 0
/uploads/0/12345/12345_v9_a3f8b91c.jpg
```

Example for user_id = 500000, version = 3:
```
500000 >> 14 = 30
/uploads/30/500000/500000_v3_ff11aa22.jpg
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

**Final layout:**
```
/uploads/<user_id & 0x1FFF>/<user_id>/<user_id>_v<version>_<suffix>.jpg
```

Example for user_id = 12345, version = 9:
```
12345 & 0x1FFF = 4153
/uploads/4153/12345/12345_v9_a3f8b91c.jpg
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
   - **Impact:** client requests broken avatar until next successful upload.
   - **Mitigation:** none perfect; write file first, then DB update (reverse order gives a "broken but visible" state which is less confusing than "correct DB, missing file").
   - Rare event; self-heals on next upload.

**Chosen upload flow (single DB round trip via combined UPDATE):**
```
1. Generate 8-char random suffix in Go (crypto/rand)
2. MySQL:
      UPDATE users
      SET avatar_version = LAST_INSERT_ID(avatar_version + 1),
          avatar_suffix  = ?
      WHERE id = ?
      → then SELECT LAST_INSERT_ID() to get new_version
   (One statement + trivial select — effectively one round trip.)
3. Ensure dir exists: /uploads/<user_id >> 14>/<user_id>/  (idempotent MkdirAll)
4. Write file to <dir>/<user_id>_v<new_version>_<suffix>.jpg.tmp
5. os.Rename(tmp, final) — atomic on POSIX
6. Old files become orphans; async sweeper cleans them up periodically.
```

**Why version stays in MySQL, not Redis:**
- Redis is for ephemeral state (sessions, rate-limit counters). Wiping Redis must not lose user data.
- Avatar version is a durable property of the user → MySQL only.

**Why store `avatar_suffix` as a separate column (not full path):**
- Smaller row: 12 bytes (INT + CHAR(8)) vs ~40-100 bytes for full VARCHAR path.
- At 10M rows → ~500-900 MB saved on the users table → more of the table fits InnoDB buffer pool.
- Decouples directory layout from data — can change `/uploads/<bucket>/<user_id>/` scheme without touching data.
- Path is deterministic from `(id, avatar_version, avatar_suffix)` → constructed server-side on each read.

**Orphan cleanup:**
- Inline delete-on-upload would require reading previous suffix (extra round trip or `FOR UPDATE` locking).
- Chose async sweeper instead: periodic job walks `/uploads/<bucket>/<user_id>/`, keeps only the current `<user_id>_v<avatar_version>_<avatar_suffix>.<ext>` file, deletes the rest.
- Simpler upload path; orphans harmless (just disk).

**Schema updates for §5:**
- Add `avatar_version INT UNSIGNED NOT NULL DEFAULT 0` (0 = no avatar uploaded)
- Add `avatar_suffix CHAR(8) NULL` (nullable; NULL when no avatar)
- Remove `avatar_path` (path is now derived, not stored)

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
- Deferred to future: CDN in front for production.

---

**Production-scale escalation path** (mention in TDD):
- Small scale → filesystem (this task)
- Medium → shared NFS or object store (S3/OSS/COS/MinIO)
- CDN in front for read-heavy workloads
- Never BLOB in DB at scale

**Status: ✅ locked-in.** Filename scheme, sharding, race handling, and access control all decided.

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

**Recommendation:** `net/http` + `chi` for routing, OR `net/http` only if we want maximum from-scratch learning value.
- `gin` is fine and common but hides some plumbing — an entry task is a chance to see it.
- The 3k-6k RPS target is easily hit by any of these; framework won't be the bottleneck.

**Decision: TBD.** `net/http` for the learning angle. refresh mainly. Does not lose out ot the others other than boilerplate anyway.

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
  - True custom pool. Driver handles wire protocol; we handle lifecycle, checkout, health, prepared statements.
  - Reasonable interpretation of the requirement.

- **(D) Full DIY** — implement the MySQL wire protocol ourselves over `net.Conn`.
  - Way out of scope for an entry task. Do not do this.

**Which does the task mean?**
- Task says: *"Connect by MySQL Go client. **Connection Pool should be built on your own.**"*
- Most reasonable read: use the driver (Go MySQL client) for wire protocol, but the pool logic — acquire, release, size limits, waiters, health — is ours. That's **Option C**.
- We should be prepared to explain in the TDD why (C) is the right level of abstraction.

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
- Metrics: in-use, idle, wait count, wait duration, evictions — for tuning.

**Concurrency model in Go:**
- Channel of idle connections + `sync.Mutex` for size tracking, OR
- Semaphore (buffered channel) for capacity + slice for idle list.
- Standard patterns — well-documented.

**Decision: Option C — direct driver connections, custom pool with prepared statement cache.** ✅ (Pending confirmation of Go.)

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

**Not choosing "build custom" is deliberate, not lazy:**
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
  1. GET creds:<username>     (credentials cache lookup)
  2. INCR rl:login:ip:<ip>    (rate limit counter)
  3. SETEX sess:<token>       (create session)
```

Un-pipelined: 3 round trips (~0.6 ms). Pipelined: 1 round trip (~0.2 ms). At scale this frees up significant time budget.

`go-redis` supports pipelines natively. Consider using where multiple commands are needed in one handler.

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
- (B) **JSON lines** over TCP (newline-delimited: `{req_id, op, data}\n` each way).
- (C) **gRPC** (technically over HTTP/2, may violate "TCP server" constraint).
- (D) **HTTP over the TCP link** (defeats the purpose of the constraint).

**Considerations:**
- Task explicitly says HTTP and TCP are separate — should be a real TCP protocol, not HTTP-in-HTTP.
- Binary framing is fast, unambiguous, easy to parse, but adds implementation time.
- JSON payload is easy to debug; protobuf is faster + smaller but adds tooling.
- gRPC is battle-tested but its HTTP/2 substrate is arguably "not TCP" — risky interpretation.
- Need to handle: request/response correlation, connection reuse, timeouts, framing errors.

**Decision: (B) JSON lines over TCP — for v1.**

Reasoning:
- Simplest framing (`\n` delimits messages) — fewer bytes of custom code to write.
- Meets all requirements: real TCP protocol, custom framing, custom multiplexing via req_id.
- Perf is fine for our 6k RPS target; JSON parse overhead is negligible compared to bcrypt/DB.
- Debuggable: can `nc` to the TCP server and type JSON to test.
- Preserves the interesting design work (multiplexing over pooled connections) without spending time on binary encoding details.

**Deferred / stretch:**
- Upgrade to **(A) length-prefixed binary frames** if time permits after perf targets are met. Binary framing:
  - Removes JSON parsing from hot path
  - Eliminates ambiguity around newlines-in-strings
  - Gives us practice with real protocol design
- Not on the critical path for the entry task; shelved unless there's time in the last week.

**Message shape:**
```
Request:  {"req_id": <int>, "op": "<string>", "data": {...}}\n
Response: {"req_id": <int>, "status": "ok"|"err", "error": "...", "data": {...}}\n
```

Ops we'll need:
- `signup`, `login`, `logout`
- `get_user`, `update_nickname`, `update_avatar`
- (any others as we discover them)

**Multiplexing:**
- HTTP server maintains a `map[req_id] chan Response` for pending requests.
- On send: allocate req_id, register channel, write frame.
- On receive (single reader goroutine per connection): read frame, look up channel by req_id, deliver response, remove entry.

**Sub-decisions TBD (tune during perf):**
- Number of persistent TCP connections HTTP→TCP: start with 8-16.
- Per-request timeout: 2s?
- Idle connection health check / reconnect on failure.

---

### 4.8 Rate Limiting

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
- If Days 11-14 have slack after primary goals met.
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

**Decision: as above.** ✅

**Status: TBD** — tweak limits after review.

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

**Common statements to prepare:**
- `SELECT id, password_hash, nickname, avatar_version, avatar_suffix FROM users WHERE username = ?`
- `SELECT id, nickname, avatar_version, avatar_suffix FROM users WHERE id = ?`
- `UPDATE users SET nickname = ? WHERE id = ?`
- `UPDATE users SET avatar_version = LAST_INSERT_ID(avatar_version + 1), avatar_suffix = ? WHERE id = ?`
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
  avatar_version INT UNSIGNED NOT NULL DEFAULT 0,  -- 0 = no avatar uploaded
  avatar_suffix  CHAR(8)      NULL,           -- 8 hex chars (32 bits random); NULL when no avatar
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

**Notes:**
- `utf8mb4` for full unicode (emojis in nicknames etc.).
- `utf8mb4_bin` collation → case-sensitive username; use lower-cased username for lookups if we want case-insensitive.
- Unique index on `username` for O(log n) lookup at 10M rows.
- `VARBINARY(60)` for bcrypt (fixed size).
- Avatar path is **derived**, not stored: `/uploads/<id >> 14>/<id>/<id>_v<avatar_version>_<avatar_suffix>.jpg` (see §4.4).
- Row footprint is ~12 bytes smaller than storing full path — meaningful at 10M rows for buffer pool efficiency.

### Redis keys

| Key | Value | TTL |
|---|---|---|
| `sess:<token>` | JSON `{user_id, username, created_at, ip?, last_seen}` | 24h sliding |
| `creds:<username>` | JSON `{id, password_hash}` | 10 min |
| `user:<user_id>` | JSON `{nickname, avatar_version, avatar_suffix}` | 1 h |
| `rl:login:ip:<ip>` | counter | 5 min |
| `rl:login:user:<uname>` | counter | 15 min |

**Cache-key design rationale:**

- **Naming aligns with endpoint.** Each cache key prefix matches the endpoint that reads it:
  - `POST /api/login` uses `creds:<username>` — credentials cache, indexed by the field the login request supplies (`username`)
  - `GET /api/user` uses `user:<user_id>` — user-display cache, indexed by the field the authenticated request supplies (`user_id` from session)
  - Reading the code / Redis dump, a reviewer can jump directly from endpoint → cache key.
- **Not per-field caching.** Each cache key holds all the fields its endpoint needs — single GET per lookup, single JSON blob. Per-field GETs would multiply Redis ops for the same data.
- **No field duplication.** With the minimal-login-response design (§6.1), login never returns profile fields → `creds:<username>` never needs `nickname` / `avatar_*`. Fields appear in exactly one cache key.
- **Simpler invalidation.** Profile edits invalidate ONLY `user:<user_id>` — `creds:<username>` is untouched (password unchanged). Login's hot-path cache stays warm across profile edits.
- **Different TTLs match volatility.** Credentials change rarely (password reset) → longer TTL is fine. Profile can change frequently (nickname edits) → shorter TTL bounds staleness on cache-hit-then-edit races.
- **Different index keys naturally.** Login has only `username`; `/api/user` has only `user_id` (via session). Each cache is indexed by what the caller has, no cross-lookup needed.

**Why session stores `username` alongside `user_id`:**

The session record's job is to translate a token into an identity. Storing both `user_id` and `username` lets one Redis GET yield everything a downstream handler needs — no extra DB lookup to resolve username from user_id. Only ~20 extra bytes per session record. Also useful for potential future needs like displaying "Logged in as alice" without additional queries, or logging by username.

**Note:** with the split cache design, we don't strictly *need* `username` in the session for invalidation anymore — profile edits touch only `user:<user_id>`. But keeping it costs almost nothing and simplifies future features that might reference the current user by name.

**Split flow — login:**

```
POST /api/login {username, password}
  1. Redis GET creds:<username>   →  {id, password_hash}   (or miss → DB → refill)
  2. bcrypt.CompareHashAndPassword(hash, password)
  3. Generate random session token
  4. Redis SET sess:<token>                                (create session)
Response: {success: true} + cookie
```

Login touches only `creds:<username>` — never `user:<user_id>`. Fast, focused.

**Split flow — /api/user:**

```
GET /api/user
  1. Redis GET sess:<token>        →  {user_id, username, ...}
  2. Redis GET user:<user_id>      →  {nickname, avatar_version, avatar_suffix}
     (miss → DB → refill cache)
  3. Compose avatar_url from user_id, version, suffix
Response: {user_id, username, nickname, avatar_url}
```

The `/api/user` path never touches `creds:<username>` — no reason to fetch password hash on a display query.

**Split flow — profile edit (nickname or avatar):**

```
POST /api/profile/nickname
  1. Redis GET sess:<token>        →  {user_id, ...}
  2. MySQL UPDATE users SET nickname = ? WHERE id = ?
  3. Redis DEL user:<user_id>      (delete-on-write invalidation)
     — creds:<username> untouched
Response: {success: true}
```

Invalidation surface is exactly one key. The login hot path (`creds:<username>`) is undisturbed by profile edits.

### User table: no vertical split

**Single `users` table, all columns together.** Considered vertical partitioning (splitting hot/cold columns into separate tables) but rejected:
- Row width is ~130 bytes → 10M rows fits comfortably in typical buffer pool (~1.3 GB).
- All columns are accessed together on the hot path (login pulls hash + nickname + avatar_*).
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
| POST | `/api/login` | Authenticate; sets cookie; returns `{success: true}` only | No |
| POST | `/api/logout` | Destroy session | Yes |
| GET  | `/api/user` | Current authenticated user's info | Yes |
| POST | `/api/profile/nickname` | Update nickname | Yes + CSRF |
| POST | `/api/profile/avatar` | Upload avatar | Yes + CSRF |
| GET  | `/uploads/<path>` | Serve avatar (public, unguessable filename via random suffix) | No |
| GET  | `/default-avatar.svg` | Default avatar (static, shared) | No |

### 6.1 Login Response Design — Minimal vs Full

**Options considered:**

- **(A) Full user info on login response**
  - Login returns `{token, user_id, username, nickname, avatar_url}` plus session cookie.
  - Frontend has everything immediately, no follow-up call needed.
- **(B) Minimal login response, separate `/api/user` for identity (chosen)**
  - Login returns only `{success: true}` plus session cookie.
  - Frontend always calls `/api/user` on protected pages to get identity/profile.

**Trade-off analysis:**

| Aspect | (A) Full-on-login | (B) Minimal + `/api/user` |
|---|---|---|
| Round trips (fresh login → profile) | 1 | 2 |
| Round trips (returning user, cookie still valid) | 1 (`/api/user`) | 1 (`/api/user`) |
| Client code paths | Two: "just logged in, use response" vs "cookie already valid, call `/api/user`" | One: always call `/api/user` |
| Endpoint responsibility | Login also carries profile data | Login only authenticates; `/api/user` only returns identity |
| Data freshness | Fresh at login; stale if user updates elsewhere | Always fresh (fetched on demand) |
| Testability | Login response coupled to user schema | Each endpoint tested independently |

**Decision: (B) Minimal login response.** Reasoning:

- **Single-responsibility**: `POST /api/login` = "authenticate credentials, issue session." `GET /api/user` = "given this cookie, tell me who I am." Neither does double duty.
- **Symmetric flow**: fresh login and returning-user-with-cookie both go through `/api/user` on the profile page. One client code path handles both cases.
- **Freshness**: `/api/user` always fetches current state (cache or DB). No stale data from a login response held in JS.
- **The `/api/user` path must exist anyway** — a user opening `/profile.html` with an existing valid cookie (returning next day) needs identity retrieval. Given that code path must exist, doing it on fresh login too is consistent, not wasteful.

**Perf-test concern (weighed and dismissed):**
- Task requires "3000/6000 login requests per second."
- Load harness (`wrk`/`vegeta`) hits `POST /api/login` directly with N connections; it does NOT follow redirects or make browser-style follow-up calls unless explicitly scripted.
- So the harness sends exactly 6k login requests/sec regardless of whether the app internally chases with `/api/user` on the client side. The perf test cost of the minimal-login design is **zero** from the harness's perspective.
- The extra `/api/user` call happens only when a real browser interacts with the app — not during synthetic load tests.

**Latency note (real browser flow):**
- Fresh login → redirect to `/profile.html` → `/api/user` call adds ~1 round trip (~1-2ms local, ~50-150ms production).
- Acceptable UX cost for the design clarity gained.

**Status: ✅ locked-in.**

---

## 7. Security Summary

| Threat | Mitigation | Status |
|---|---|---|
| Password theft from DB dump | bcrypt cost 10, per-user salt | ✅ |
| Session hijack | HttpOnly + SameSite cookie, Redis-backed, TTL | ✅ |
| Brute-force login | Redis rate limit per IP + per user | ⏸ Shelved (revisit if time permits) |
| CSRF | SameSite=Lax cookie (double-submit token shelved) | ✅ (Lax) / ⏸ (token) |
| SQL injection | Parameterized queries only | ✅ |
| Malicious file upload | Size cap, MIME sniff, random filename, no exec on upload dir | ✅ |
| Enumeration (which users exist) | Uniform error message on login failure | ✅ |
| MITM | TLS termination | Production only |
| DoS | Rate limit + pool caps + timeouts | Partial |

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

**Resolved:**
- ✅ Filename scheme + directory sharding (§4.4) — `<user_id>_v<version>_<suffix>.jpg` in `/uploads/<user_id >> 14>/<user_id>/` (fixed-size range partitioning, extends to hybrid pattern on horizontal scaling)
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
