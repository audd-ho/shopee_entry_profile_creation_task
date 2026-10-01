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
                                          │  shards   │ │         │
                                          │ (data +   │ │ cache + │
                                          │  lookup)  │ │ session │
                                          │ custom    │ │lock stk │
                                          │ conn pool │ │         │
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

**Sub-decisions — resolved (see the dedicated subsection after §4.1's versioning discussion, "Auth cache vs. user cache — the final split"):**
- What do we cache?
  - **Two separate caches, not one.** `auth:<username>` holds only `{id, password_hash}` — exactly what verifying a password and minting a session need. `{user:<user_id>}` holds `<version>\n<JSON>` (version-in-value, managed by the lock stack — see `lock_stack_implementation_guide.md`) with the JSON containing `{username, nickname, avatar_suffix}` — everything a page needs to display, fetched only after a session exists. Neither cache holds a field the other one needs; there is no overlap.
  - The username→bcrypt-hash idea below was the starting point, and is still correct as far as it goes — it just turned out to be half the picture once profile data (nickname, avatar) needed caching too, and splitting by *when it's needed* (before vs. after a session exists) rather than caching one wide record per user turned out to matter for invalidation, not just for payload size.
- TTL?
  - `auth:<username>`: 10 min.
  - `{user:<user_id>}`: 1 hr (set by lock stack's `CacheTTL` / `LOCKSTACK_CACHE_TTL_SECONDS`).
- Invalidation policy on profile edit: delete-on-write vs write-through?
  - **Superseded:** now handled by the lock stack (lock → DEL → DB UPDATE → write-through → unlock). See `lock_stack_implementation_guide.md`. The original delete-on-write reasoning below remains valid as historical context for why bare write-through was rejected.
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

**Invalidation triggers (updated for the lock stack + auth:/user: split):**

| Action | Cache operation |
|---|---|
| Nickname update | Lock stack write path on `{user:<user_id>}`: lock → DEL → DB UPDATE → Lua version-compare write-through → unlock. `auth:<username>` untouched. |
| Avatar update | Same lock stack write path on `{user:<user_id>}`. `auth:<username>` untouched. |
| Signup | none (no negative caching — see below) |
| Logout | `DEL sess:<token>` (pure cache, no db involvement, no need for versioning) |
| Password change (future — no such feature exists today) | `auth:<username>` + all sessions for that user |

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

**Implementation note (resolved in §4.4.4, updated for lock stack):** the per-row version above is a dedicated `row_version` column. It was originally kept separate from an `avatar_version` column (§4.4) for exactly the reason below — but `avatar_version` was later removed entirely (§4.4.4 point 3a), so today `row_version` is simply the only version counter in the schema. `row_version` bumps on *any* mutating write (nickname or avatar) and is now embedded in the **value** of the `{user:<user_id>}` Redis key (format: `<version>\n<JSON data>`), managed by the lock stack's Lua scripts — **not** in the key name (the earlier version-in-key design `user:<id>:v<N>` was superseded by the lock stack). `auth:<username>` remains unversioned and does not carry `row_version`. The historical reasoning for keeping the two counters separate, back when both existed: conflating them would have made a nickname-only edit bump `avatar_version` too, pointing avatar URLs at a version whose file was never written — see §4.4.4 point 4 for the full race analysis, which remains accurate as a record of why that design used two counters, even though only one exists now.

---

#### 4.1.1 Auth cache vs. user cache — the final split (post-implementation)

The table above already reflects the outcome; this subsection is the reasoning, since it took a few rounds to land on.

**The starting design cached one record per user under `user:<username>`,** containing everything a login might eventually need to display: `id`, `password_hash`, `nickname`, `avatar_suffix`, and (once §4.1's versioning was added) `row_version`. That is a natural first cut — one user, one cache entry — but it conflates two things with genuinely different lifecycles: *authenticating* (needs only the password hash) and *displaying a profile* (needs nickname/avatar, never the hash). Splitting them into `auth:<username>` and `{user:<user_id>}` (now managed by the lock stack with version-in-value) is what the current design does, and each field question below was resolved by asking which of those two jobs actually needs it.

**Why `row_version` does not live in `auth:<username>` or in the session — superseded by the lock stack:**

> **Historical note (superseded):** the reasoning below was written when the user cache used version-in-key (`user:<id>:v<N>`), which required the session to carry a `row_version` hint so readers could form the correct cache key. An intermediate version of this design put that hint in `auth:` to seed it at login, then moved it to a fresh MySQL read on every login when that proved stale-prone. **The lock stack eliminates the need entirely:** the user cache now uses a single key `{user:<user_id>}` with version-in-value (format `<version>\n<data>`), so there is no version to guess — every reader hits the same key. The session no longer carries `row_version` at all, and the login path no longer needs a `GetRowVersion` call. The original reasoning is preserved below as a record of why earlier designs needed it and why each alternative failed.

<details>
<summary>Historical reasoning (version-in-key era, now superseded)</summary>

- A hint cached in `auth:` can only ever be as fresh as the last time that key was populated. Once nickname/avatar edits stopped needing to touch `auth:` at all (nothing left in it that they could make stale), nothing kept that hint honest between populations — it could go stale for the entirety of its 10-minute TTL, and the only way to fix that without giving up the split would have been to invalidate `auth:` on every profile edit anyway, which defeats the point of splitting it out in the first place (and doubles the Redis writes per edit, one for each cache, back to where the split started).
- The alternative resolved on: **fetch `row_version` fresh from MySQL on every login**, unconditionally — not a cached guess, not a hardcoded `0`. Cost: one extra indexed `SELECT row_version FROM users WHERE id = ?` on an `auth:` cache **hit** (the common case); zero extra on a **miss**, since the miss-path query (`SELECT id, password_hash, row_version FROM users WHERE username = ?`) already returns it in the same round trip.
- **Why pay that cost rather than seed a session's hint at `0` and let it self-correct lazily:** the real product flow is login immediately followed by the landing page's own `get_user` call — not two independently-optional events, one glued pathway. The deciding factor was determinism of the combined pathway a real user experiences.

</details>

**What this means for invalidation:** `auth:<username>` now holds only `id` and `password_hash`, both immutable in this service's current feature set (no change-password endpoint exists). So it needs **no invalidation path at all** today, beyond its own TTL — nickname and avatar edits are handled entirely by the lock stack on `{user:<user_id>}`. `InvalidateOnPasswordChange` is kept in the codebase even though it has no caller, against the day a change-password feature exists — deleting working code for an out-of-scope feature just means writing it again later.

**What this means for the session record:** with nothing left that needs `username` for invalidation (see the previous paragraph), and `get_user`'s response able to read `username` from `{user:<user_id>}` just as well, `Session.Username` was removed. With the lock stack replacing version-in-key, `Session.RowVersion` was also removed — the user cache uses a single key per user, so there is no version hint to carry. The session record now holds only `{user_id, created_at, last_seen, ip}` — purely "who is this, and are they still logged in."

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
  - `row_version` was removed from the session when the lock stack replaced version-in-key — the user cache now uses a single key `{user:<id>}`, so no version hint is needed.
  - ip is for different ip, suspicious behaviour, but if want to expand can use, if not, can leave as so, no problem, last seen for rolling refresh

**Status:** session cookie + TTL & sliding behavior.

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

4. **`row_version` / `avatar_version` conflation race (historical — kept as a record of why the two counters were designed separately; `avatar_version` no longer exists, see point 3a):** §4.1 decided **per-row** cache versioning: a single version number that bumps on *either* nickname update or avatar upload, now embedded in the value of the `{user:<user_id>}` Redis key via the lock stack (previously it was embedded in the key name as `user:<user_id>:v<N>`; `auth:<username>` is unversioned and, since §4.1.1, does not carry `row_version` at all — §5). `avatar_version` (this section, when it still existed) bumped **only** on avatar upload and was embedded in the served file's **name**, not just a cache key.
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
7. Cache invalidation is now handled by the lock stack's write path (lock → DEL
   `{user:<user_id>}` → DB UPDATE → Lua write-through → unlock), not a separate
   goroutine. `auth:<username>` is NOT touched (resolved, §4.1.1): it holds
   nothing an avatar edit could make stale.
```
If step 5 fails, the file written in steps 2-4 is simply an orphan — swept up later, same as any other orphan — and the row is exactly as it was before the request. There is no ordering under which this flow can leave MySQL pointing at a suffix whose file does not exist.

**Nickname update flow, for symmetry (no file involved, so only `row_version` moves):**
```
1. MySQL: UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?
   → then SELECT LAST_INSERT_ID() to get new_row_version
2. Cache invalidation handled by the lock stack write path (lock → DEL `{user:<user_id>}` → write-through → unlock) — auth:<username> not touched, same reasoning as the avatar flow above
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
  1. GET auth:<username>      (cache lookup)
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
-- Per data shard (data_0, data_1, ...); see schema_data.sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,              -- snowflake ID, app-generated (§13.3)
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,      -- bcrypt output is 60 bytes
  nickname       VARCHAR(128) NOT NULL,       -- 32 runes × up to 4 bytes
  avatar_suffix  CHAR(8)      NULL,           -- 8 hex chars (32 bits random); NULL when no avatar
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,  -- bumped on ANY update (nickname or avatar); version-in-value for lock stack cache, see §4.1/§4.4.4
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)              -- safety net within-shard; global uniqueness via lookup shards
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

```sql
-- Per lookup shard (lookup_0, lookup_1); see schema_lookup.sql
CREATE TABLE username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
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
- `row_version` is unrelated to the avatar scheme above, and unaffected by removing `avatar_version` — the two were already deliberately separate counters (§4.4.4 point 4). `row_version` exists purely for the lock stack's version-in-value cache consistency (embedded in the value of `{user:<user_id>}`, compared by Lua scripts), and never appears in an avatar filename.

### Redis keys

| Key | Value | TTL |
|---|---|---|
| `sess:<token>` | JSON `{user_id, created_at, ip?, last_seen}` | 24h sliding |
| `auth:<username>` | JSON `{id, password_hash}` | 10 min |
| `{user:<user_id>}` | `<version>\n<JSON>` where JSON = `{username, nickname, avatar_suffix}`. Managed by lock stack (version-in-value, lease-guarded reads). Hash tag `{…}` ensures data key and lock key colocate in Redis Cluster. | 1 h (`LOCKSTACK_CACHE_TTL_SECONDS`) |
| `{user:<user_id>}:lock` | `<writer_id>` — per-key write lock held during the lock stack write path | `LOCKSTACK_LOCK_TTL_MS` (watchdog-extended) |
| `lookup:<username>` | JSON `{user_id}` — immutable once written, no versioning needed | 10 min |
| `rl:login:ip:<ip>` | counter | 5 min |
| `rl:login:user:<uname>` | counter | 15 min |

**Why `auth:<username>` is not versioned:** `auth:` holds only `{id, password_hash}`, both immutable in the current feature set (no change-password endpoint). It needs no versioning and no invalidation beyond its own TTL. Profile edits are handled entirely by the lock stack on `{user:<user_id>}`.

**`row_version` is now embedded in the value, not the key** (lock stack design, superseding the earlier version-in-key `user:<id>:v<N>` approach): the user cache uses a single key `{user:<user_id>}` with the value formatted as `<version>\n<JSON data>`. The lock stack's Lua `versionSetScript` atomically compares versions before writing, and the writer's DEL-first ordering (M7) plus the reader lease (M13) prevent slow-reader overwrites. This eliminated the need for version-in-key and, consequently, the need for the session to carry a `row_version` hint. See `lock_stack_implementation_guide.md` for the full design.

> **Historical note (superseded):** the earlier version-in-key design (`user:<id>:v<N>`) made the old key unreachable the instant `row_version` bumped in MySQL, without needing a delete to race against a concurrent read. The delete-on-write goroutine still deleted the old key so it didn't linger to TTL, but correctness didn't depend on that delete winning any race. That design required the session to carry `row_version` so readers could form the correct key, which in turn required a fresh MySQL `GetRowVersion` read on every login — all of which the lock stack's version-in-value approach removes.

**Cache-key design rationale:**

- **Not per-field caching.** Every hot-path read (login, `/api/me`) needs most fields together — per-field GETs would multiply Redis ops for the same data. Single JSON blob per logical access pattern is faster.
- **Two keys per user, not one — split by *when* the data is needed, not just by size.** The original split reasoning was "password hash vs. everything else"; the reasoning that stuck (§4.1.1) is sharper: `auth:` holds exactly what authenticating requires and nothing a profile edit could ever touch, `user:` holds exactly what a page display requires and nothing that must never leave the server.
  - `auth:<username>` — indexed by username, for login only. Contains the password hash; contains nothing else.
  - `{user:<user_id>}` — indexed by user_id, for everything after login. Contains username, nickname, avatar (version-in-value); never the hash. Managed by the lock stack.
- **Split benefits:**
  - Different TTLs (password hash long-lived relative to how rarely it's read again once cached; profile shorter, since edits are more frequent than password changes would be).
  - Clean invalidation: a nickname/avatar edit touches `user:<user_id>` only. `auth:<username>` needs no invalidation path at all today, because nothing in it can go stale in this service's current feature set (§4.1.1).
  - Different index keys naturally — username lookup for login, id lookup for everything after.
- **`sess:<token>` and rate-limit keys** are separate concerns (session state, counters); no user data leaks in them.

**Why the session record stores only `{user_id, created_at, last_seen, ip}`:** `username` was removed once `auth:<username>` stopped needing invalidation on profile edits (§4.1.1) — no reader needs it from the session, since `{user:<user_id>}` carries it. `row_version` was removed when the lock stack replaced version-in-key — the user cache uses a single key per user, so there is no version hint to carry (see §4.1.1). The session's only job is now "who is this, and are they still logged in."

See §4.4.4 for why `row_version` is a separate counter from the (now-removed) `avatar_version` and why it's the one embedded in the user cache value.

### User table: no vertical split

**Single `users` table, all columns together.** Considered vertical partitioning (splitting hot/cold columns into separate tables) but rejected:
- Row width is ~134 bytes (~130 before `row_version` was added, minus the ~4-byte `avatar_version` `INT` removed in §4.4.4 point 3a) → 10M rows still fits comfortably in typical buffer pool (~1.3-1.4 GB). Neither change moves the needle on this decision.
- The two hot-path queries pull different subsets now (§4.1.1): login (`GetUserByUsername`) reads `id, password_hash, row_version` only; `get_user` (`GetUserByID`) reads `id, username, nickname, avatar_suffix, row_version`. Both are still single-row, single-table reads by an indexed column — the split changed which columns are selected, not how many tables are touched, so it is not an argument for splitting the table itself. `row_version` rides along on both queries because both need it to build a Redis cache key, not for the row's own data.
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

- [ ] `SameSite=Lax` vs `Strict` (§4.9)

**Resolved:**
- ✅ **Language:** Go (§4.5)
- ✅ **HTTP framework:** `net/http` (§4.5)
- ✅ **Session:** 24h sliding-window TTL (§4.2). Session holds `{user_id, created_at, last_seen, ip}` — `row_version` removed with lock stack.
- ✅ **Cache strategy:** two caches (`auth:<username>` + `{user:<user_id>}` via lock stack), TTLs 10 min / 1 hr. Lock stack write path (lock → DEL → DB UPDATE → write-through → unlock) replaces simple delete-on-write (§4.1).
- ✅ **bcrypt cost:** 5 (§4.3), matched to seeded data.
- ✅ **Custom pool sizing:** data shards 10 conns/shard, lookup shards 5 conns/shard, 200ms max wait (§4.6).
- ✅ **TCP protocol pool:** 8 connections, multiplexed (§4.7).
- ✅ Filename scheme + directory sharding (§4.4) — `<user_id>_<suffix>.jpg` (no version segment — `avatar_version` was tried, then removed post-implementation, §4.4.4 point 3a) in `/uploads/<user_id >> 14>/<user_id>/` (fixed-size range partitioning, extends to hybrid pattern on horizontal scaling)
- ✅ Avatar URL public with unguessable filename (§4.4.5)
- ✅ Redis pool via library (`go-redis`), not custom (§4.6.1)
- ✅ HTTP↔TCP framing: streaming JSON for v1; binary framing shelved (§4.7)
- ✅ Split the login cache from the profile cache (§4.1.1) — `user:<username>` became `auth:<username>` (trimmed to `{id, password_hash}` only), `profile:<user_id>` became `{user:<user_id>}` (version-in-value via lock stack). `Session.Username` and `Session.RowVersion` both removed.
- ✅ **Horizontal sharding** (§13) — implemented: 2 lookup shards, 2 data shards (scalable to 3+), snowflake IDs, consistent hash ring.
- ✅ **Lock stack** — per-key Redis lock with watchdog, DEL-first ordering, Lua version-compare write-through, reader lease (M13). See `lock_stack_implementation_guide.md`.

**Shelved (documented, not implemented; revisit if time permits):**
- ⏸ Rate limiting (§4.8) — brute-force protection deferred
- ⏸ Length-prefixed binary framing (§4.7) — custom binary header for HTTP↔TCP protocol
- ⏸ Double-submit CSRF token (§4.9) — SameSite=Lax covers ~95% in modern browsers

---

## 10. Future Work / Production Considerations

- Migrate images to object storage (S3/OSS/COS/MinIO) + CDN
- TLS everywhere (HTTPS, mTLS between servers)
- Redis Cluster (when single-instance Redis becomes a bottleneck); MySQL read replicas per shard
- Multiple TCP server instances behind an internal LB (architecture already supports it — stateless, machine_id-based snowflakes)
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

---

## 13. Horizontal Sharding Design (Implemented)

Implemented for this entry task — 2 lookup shards and 2 data shards (scalable to
3+) on localhost, all as separate MySQL schemas on the same mysqld instance. The
Go code treats each shard as an independent pool, so migrating to separate hosts
is a DSN change, not a code change. This section documents the full design
reasoning so the seams are visible during review.

---

### 13.1 High-Level Sharded Topology

```
   Browser
     │  HTTPS/HTTP
     ▼
┌──────────────┐
│  HTTP Server │  (N instances, stateless, round-robin LB)
│  (Go)        │
└──────┬───────┘
       │  TCP (custom protocol, multiplexed)
       │  Round-robin — any TCP server handles any user
       ▼
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│ TCP Server 0 │   │ TCP Server 1 │   │ TCP Server 2 │
│  machine_id=0│   │  machine_id=1│   │  machine_id=2│
│              │   │              │   │              │
│ Generates    │   │ Generates    │   │ Generates    │
│ snowflake IDs│   │ snowflake IDs│   │ snowflake IDs│
│ locally      │   │ locally      │   │ locally      │
└──┬───┬───┬───┘   └──┬───┬───┬───┘   └──┬───┬───┬───┘
   │   │   │          │   │   │          │   │   │
   │   │   │          │   │   │          │   │   │
   │   │   └──────────┼───┼───┼──────────┼───┼───┤
   │   └──────────────┼───┼───┤          │   │   │
   └──────────────────┤   │   │          │   │   │
                      ▼   ▼   ▼          ▼   ▼   ▼
                 ┌─────────────────────────────────────┐
                 │        Every TCP server holds        │
                 │        a pool to every shard         │
                 └──┬──────────┬──────────┬────────────┘
                    │          │          │
               ┌────▼───┐ ┌───▼────┐ ┌───▼────┐
               │ Shard 0 │ │ Shard 1│ │ Shard 2│   MySQL shards
               │ users   │ │ users  │ │ users  │   (sharded by user_id)
               └─────────┘ └────────┘ └────────┘

  ┌─────────────────────────┐       ┌──────────┐
  │  Username Lookup        │       │  Redis   │
  │  (shared, unsharded)    │       │  (shared)│
  │  username → user_id     │       │  sessions│
  │  Written on signup      │       │  + caches│
  │  Read on login          │       │          │
  └─────────────────────────┘       └──────────┘
```

**Key properties:**
- **TCP servers are stateless.** Any instance handles any user. The HTTP load
  balancer remains dumb round-robin — no application-layer routing.
- **Every TCP server connects to every DB shard** (one pool per shard). Shard
  selection happens inside the TCP server as a one-line pool lookup:
  `pool := s.shards[ShardFor(userID)]`. No TCP-to-TCP forwarding.
- **The username lookup is the only shared, unsharded data store.** All other
  queries route by `user_id`, which is on the session for all post-login paths.

---

### 13.2 Shard Key — `user_id` via Consistent Hashing

**Decision: shard by `user_id`.**

| Shard by | Login (`WHERE username=?`) | Post-login (`WHERE id=?`) | Uniqueness |
|---|---|---|---|
| `username` | Single-shard (fast) | Needs username→id→shard mapping | Free (one shard owns each username) |
| **`user_id` (chosen)** | Needs username→id lookup, then single-shard | Single-shard (fast) | Needs external uniqueness check |

**Reasoning:**
- The vast majority of queries are post-login and by `user_id` (get_user,
  update_nickname, update_avatar on every page load). Optimising the shard key
  for the 99% case over the 1% case (signup/login by username) is correct.
- Login is already the slow path (bcrypt dominates). One extra lookup to resolve
  username→user_id is negligible next to a 2ms bcrypt compare.
- The username→id mapping is small, write-once-read-many, and fits in memory.
- Integer comparisons for shard routing and B-tree traversal are single CPU
  instructions regardless of magnitude — `5 < 6` costs the same as
  `483927481 < 892034712` (both fit in one 64-bit register, one `CMP`). There
  is no performance advantage to small sequential IDs over large snowflake IDs.

---

#### 13.2.1 Consistent Hashing (Ring Hash) — How It Works

**Why not simple modulo (`user_id % N`)?**
- Changing N (adding/removing a shard) reshuffles ~(N-1)/N of all users. With
  3 shards → 4, about 75% of users land on a different shard and need migration.
- Consistent hashing moves only ~1/N (~25%) when adding one shard.

**The ring:**

Nodes (shards) and keys (user_ids) are hashed onto a circular number line
(0 to 2³²-1). A key hashes to a position and walks **clockwise** to the first
node it hits. That node owns it.

```
        Shard 0 (pos 100)
       /
  0 ──●────────────●── Shard 1 (pos 400)
  │      Ring           │
  │                     │
  └────●────────────────┘
     Shard 2 (pos 700)

user_id 42 hashes to 250 → walks clockwise → hits Shard 1
user_id 99 hashes to 500 → walks clockwise → hits Shard 2
user_id 7  hashes to 50  → walks clockwise → hits Shard 0
```

**Adding a shard (the key property):**

Say Shard 3 joins at position 550:

```
Before:  keys 401-700 → Shard 2
After:   keys 401-550 → Shard 3  (moved FROM Shard 2 only)
         keys 551-700 → Shard 2  (unchanged)
         keys 0-100   → Shard 0  (unchanged)
         keys 101-400 → Shard 1  (unchanged)
```

Only keys between the new node and its predecessor move. They come from **one
neighbor** (the clockwise successor), not from all nodes. Everything else stays
put. This is the core win.

**Removing a shard:** the reverse. Its keys walk clockwise to the next node. One
neighbor absorbs them. Still ~1/N movement.

**The balance problem and virtual nodes (vnodes):**

With only 3 physical nodes, the arc lengths between them are uneven — one node
could own 60% of the ring by chance. Fix: each physical node gets 100-200
positions (vnodes) on the ring. Statistically, the arcs even out.

```
Node A: positions 100, 1500, 3200, 4800, ...  (150 vnodes)
Node B: positions 400, 1800, 3500, 5100, ...  (150 vnodes)
Node C: positions 700, 2100, 3800, ...        (150 vnodes)
```

A key hashes to 1600 → walks clockwise → hits vnode at 1800 → that's physical
Node B. Adding a physical node distributes its vnodes around the ring and each
one steals a small arc from a different neighbor — smooth, even rebalancing
across all existing shards.

**Implementation in Go:** there are libraries (e.g. `hashicorp/consistent`), or
it's ~50 lines: a sorted slice of `(hash, shardID)` pairs, binary-searched for
each lookup. Performance is O(log V) where V = total vnodes — negligible.

---

### 13.3 ID Generation — Snowflake IDs

**Problem:** `AUTO_INCREMENT` generates IDs on one MySQL instance. With multiple
shards, two instances would produce colliding IDs.

**Options considered:**

| Approach | Coordination? | Fits BIGINT? | B-tree locality | Survives resharding |
|---|---|---|---|---|
| Central auto-increment counter | Yes (single point of failure, bottleneck) | ✅ | ✅ (sequential) | ✅ |
| DB increment-by-step (`auto_increment_increment=N`) | Config-time only | ✅ | ✅ | ❌ (changing N breaks the step scheme) |
| Random UUID (128-bit) | None | ❌ (needs BINARY(16), 2× PK width) | ❌ (random scatter → page splits, poor buffer pool) | ✅ |
| ULID (128-bit, time-sorted) | None | ❌ (still 128 bits) | ✅ (time-sorted) | ✅ |
| **Snowflake (64-bit, time-sorted)** | Machine ID assignment only | ✅ | ✅ (time-sorted) | ✅ |

**Decision: application-generated snowflake IDs.** Each TCP server generates
64-bit IDs locally with no coordination and no DB round trip.

```
 64-bit ID layout (fits in BIGINT UNSIGNED):
 ┌─────────────────────────────┬──────────┬──────────────┐
 │  41 bits: millisecond       │ 10 bits: │ 12 bits:     │
 │  timestamp since custom     │ machine  │ sequence     │
 │  epoch (69 years range)     │ ID       │ (4096/ms)    │
 └─────────────────────────────┴──────────┴──────────────┘
```

| Component | Bits | Range | Purpose |
|---|---|---|---|
| Timestamp | 41 | ~69.7 years from custom epoch | Time-sorted for B-tree locality |
| Machine ID | 10 | 0–1023 TCP server instances | Prevents collisions across servers |
| Sequence | 12 | 0–4095 per ms per machine | Handles burst within one millisecond |

**Custom epoch:** not Unix epoch (1970) — pick the service launch date. This
avoids wasting bits on the 50+ years already passed. 2⁴¹ ms ≈ 69.7 years
from the chosen epoch.

**Machine ID assignment:** the only coordination needed, and it's one-time at
startup. Options: config file / env var per instance, or a coordination service
(ZooKeeper, etcd) that leases machine IDs. For a handful of TCP servers, a
config value is simplest.

**Properties:**
- **No central counter.** Each TCP server generates IDs independently.
- **No collisions.** Same millisecond + different machine bits = different IDs.
  Same machine + same millisecond + different sequence bits = different IDs.
- **Time-sorted.** High bits are the timestamp, so IDs are roughly chronological.
  New inserts cluster near the rightmost B-tree leaf — same locality benefit as
  `AUTO_INCREMENT`. This is the key advantage over random UUIDs, which scatter
  inserts across the entire index causing page splits and poor buffer pool hit
  rates.
- **Fits in `BIGINT UNSIGNED`.** No schema type change from the current `id`
  column — just remove `AUTO_INCREMENT` and supply the ID from Go.
- **Shard-independent.** The ID encodes no shard information. `ShardFor(id)` is
  a separate function (consistent hash) that can change without re-issuing IDs.
- **Density doesn't matter.** The IDs have gaps (machine bits, sequence bits
  create holes). But density was never useful — nobody queries
  `WHERE id BETWEEN 1 AND 1000` expecting exactly 1000 rows. In practice,
  auto-increment IDs also have gaps (rolled-back transactions, deletes).

**Go implementation** (no bitfield syntax in Go — explicit shifts, same as
C++ in practice since bitfield ordering is implementation-defined):
```go
id := (timestamp << 22) | (machineID << 12) | sequence

// Reading back:
timestamp := id >> 22
machineID := (id >> 12) & 0x3FF   // 10-bit mask
sequence  := id & 0xFFF            // 12-bit mask
```

This is the same as C/C++ bitfield packing (`struct { uint64_t seq:12; ... }`)
but explicit. C++ bitfields look cleaner in source but have implementation-
defined bit ordering across compilers/endianness, so most C++ snowflake
implementations also use explicit shifts for portability.

**Throughput:** 4,096 IDs/ms/machine = 4,096,000 IDs/sec/machine. If a machine
exhausts 4096 in one millisecond (extremely unlikely for user signups), it waits
for the next millisecond — a sub-millisecond stall. With 1024 machines, the
system can produce ~4 billion IDs/sec total.

---

### 13.4 Username Lookup — Global Uniqueness

**Problem:** with the `users` table sharded by `user_id`, the `UNIQUE KEY
uk_username` only enforces uniqueness within one shard. Two shards can each
accept an `INSERT` for the same username.

**Decision: MySQL table as source of truth, on a separate dedicated instance,
with Redis cache (`lookup:<username>`) on top.** Maps `username → user_id` and
is the single source of truth for "does this username exist globally."

**Why MySQL table (not purely Redis):**
- Redis is volatile. If Redis dies/flushes, a pure-Redis lookup loses all
  mappings and every login falls through to... nowhere (the per-shard `users`
  tables don't have a global username index). Rebuilding from shards requires
  scanning all shards and collecting `(username, id)` pairs — slow for 10M rows.
- A MySQL table is durable. Redis cache on top is a performance layer — same
  defense-in-depth as the existing `auth:<username>` cache over the `users` table.

**Why a separate instance (not co-located on a data shard):**
- Placing the lookup on one shard creates an asymmetry: that shard handles its
  normal 1/N share of user data queries PLUS 100% of all login lookups. The
  lookups are individually cheap (single PK read, table fits in memory), but at
  3k-6k login RPS the volume is real and that one shard always runs hotter than
  the others through no fault of its own data distribution.
- A separate instance keeps every data shard's load symmetric.
- Isolates lookup availability from data shard availability: if a data shard has
  a problem, the lookup is unaffected — logins can still resolve username→user_id,
  they just fail at the shard query step. If the lookup instance has a problem,
  only signup and login are affected — post-login operations (get_user,
  update_nickname, update_avatar) keep working because they route by `user_id`
  from the session and never touch the lookup.

```sql
CREATE TABLE username_lookup (
  username  VARCHAR(32) NOT NULL PRIMARY KEY,
  user_id   BIGINT UNSIGNED NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```
- One row ≈ 40 bytes. 10M users ≈ 400 MB — fits entirely in memory.
- Primary key lookup = O(log n), effectively O(1) in buffer pool.
- Durable — survives restarts without rebuilding.

**Redis cache on top:** cache-aside, same pattern as `auth:<username>`.

| Key | Value | TTL | Written | Read |
|---|---|---|---|---|
| `lookup:<username>` | JSON `{user_id}` | 10 min | SET on signup (new entry) | GET on login (before MySQL fallback) |

The data is immutable once written (a username maps to one user_id forever), so
there is no staleness concern and no need for versioning. A cache miss on login
falls through to the MySQL lookup table and populates the cache on the way back.

**Scaling the lookup itself** (if ever needed):

The lookup is **write-once-read-many** (written on signup, read on login) and
is on its own dedicated instance, so scaling it is independent of the data
shards.

- **Read scaling:** one primary + N read replicas trivially scales the read side.
- **Write scaling:** would need hundreds of thousands of signups per second to
  stress the write side — far beyond what bcrypt allows through per signup.
- **If write scaling is ever needed:** shard the lookup itself by
  `hash(username)` — matching its own access pattern (`WHERE username = ?`).
  This is independent of the data shards which shard by `hash(user_id)`. Each
  store is sharded by the key it's actually queried on — no cross-shard fan-out
  on either side. Username lookup shards still enforce global uniqueness because
  each username hashes to exactly one lookup shard, and that shard's PK catches
  duplicates.
- For this system, a single instance handles the load with enormous margin.

**The lookup is only touched on signup and login.** All post-login operations
(get_user, update_nickname, update_avatar) route by `user_id` from the session
and skip the lookup entirely. This is the critical property — the lookup is
not on any hot path.

---

### 13.5 Sharded Workflows

#### Signup (sharded)

```
TCP server (any, via round-robin):

  1. Generate snowflake ID locally
     (machine_id baked in, no DB round trip, no coordination)

  2. username_lookup.INSERT("alice", 12345)
     → duplicate key? → 409 username taken, stop
     → success? → name is now globally reserved

  3. shard = ShardFor(12345) → shard 1

  4. shard_1_pool.INSERT(12345, "alice", hash, nickname)
     → if this fails: DELETE from username_lookup
       (rollback the reservation, leave no dangling mapping)

  5. Return success
```

No `AUTO_INCREMENT`, no `LastInsertId()`. The ID is known before any INSERT runs.

**Failure handling:** step 2 is the global uniqueness check. Step 4's failure is
unlikely (the row doesn't exist yet on that shard), but if it does fail (disk,
connection), the lookup reservation from step 2 must be rolled back, or
"alice" is permanently reserved with no user behind it. This is a two-phase
commit across two stores — the simplest form (insert A, insert B, undo A on B's
failure). No distributed transaction needed because the lookup is a simple
key-value map, and the undo is a single DELETE.

#### Login (sharded)

```
TCP server (any, via round-robin):

  1. username_lookup.GET("alice") → user_id 12345
     → not found? → dummy bcrypt verify, return 401

  2. shard = ShardFor(12345) → shard 1

  3. shard_1_pool.GetUserByUsername("alice")
     → {id, password_hash}

  4. bcrypt.Compare, create session
     (session holds {user_id, created_at, last_seen, ip} — no row_version)
```

One extra lookup (step 1) compared to the current unsharded flow. Cost: one
primary-key read on a table that fits in memory — negligible next to bcrypt.

**Auth cache interaction:** the `auth:<username>` Redis cache still works
identically. On a cache hit, login skips step 3 (no MySQL query for the hash)
but still needs step 1 to get the user_id (unless the auth cache is expanded to
carry user_id — which it already does in `model.AuthCached.ID`). So the actual
hit-path is: lookup → auth cache GET → have id and hash → bcrypt → session. No
`GetRowVersion` call needed — the session no longer carries `row_version` (the
lock stack's version-in-value design eliminated that need).

#### Post-Login (get_user, update_nickname, update_avatar — sharded)

```
TCP server (any):

  1. resolveSession(token) → {user_id}
     (from Redis — no lookup needed, no row_version needed)

  2. shard = ShardFor(user_id)

  3. shard_N_pool.query(...)
     (identical to current single-instance queries)
```

The username lookup is not involved. The session carries `user_id`, and
`ShardFor(user_id)` is a pure function (consistent hash) — no network call.
This is the hot path, and it adds exactly one function call (a binary search
on a sorted slice of vnodes) per request.

---

### 13.6 Scaling Model — TCP Servers and DB Shards Are Independent Axes

Sharding the DB and scaling the TCP servers are two **decoupled** decisions.

```
                    TCP servers                    DB shards
                    (CPU-bound:                    (IO-bound:
                     bcrypt, JSON,                  disk, buffer pool,
                     protocol framing)              query execution)
                         │                              │
  Add TCP servers ───────┤          Add DB shards ──────┤
  for more bcrypt        │          for more storage     │
  throughput             │          or query IOPS        │
                         │                              │
                    No change to                   No change to
                    DB shards                      TCP servers
```

#### Option A — Every TCP server connects to every DB shard (chosen)

```
TCP-0 ──┬── DB shard 0
TCP-1 ──┼── DB shard 1
TCP-2 ──┴── DB shard 2
```

- Any TCP server can handle any request — HTTP LB stays dumb round-robin.
- Each TCP server holds one pool per shard. Total connections =
  `tcp_count × shard_count × pool_size`. Example: 3 × 3 × 10 = 90 total.
  MySQL handles thousands; this is negligible.
- TCP and DB scale independently: add TCP servers for CPU (bcrypt), add DB
  shards for storage/IOPS.
- **Failover is free:** a hot shard doesn't mean a hot TCP server. If one TCP
  server dies, the others keep serving all shards via the HTTP LB's existing
  health-check + round-robin.
- The TCP server is stateless (no local data, just connection pools), so any
  instance serves any user. Adding shards just means each TCP server's pool
  list grows by one entry.

#### Option B — TCP server paired 1:1 with its DB shard (rejected)

```
TCP-0 ── DB shard 0
TCP-1 ── DB shard 1
TCP-2 ── DB shard 2
```

- Fewer connections (each TCP server has one pool, not N pools).
- **But:** a hot shard = a hot TCP server. They're the same bottleneck. Cannot
  add TCP capacity without adding DB capacity.
- **But:** failover is hard. TCP-1 dies → shard 1 is unreachable. A "fallback"
  TCP server for shard 1 needs its own pool to shard 1's DB, which is just
  Option A with extra steps.
- **But:** requires the HTTP server (or a gateway in front) to become an
  application-aware router:
  - Parse the HTTP body to extract the username or session token
  - Query the username lookup or Redis to resolve to a user_id
  - Compute `ShardFor(user_id)` to pick the right TCP server
  - Handle routing for post-login requests (resolve session token → user_id →
    shard → TCP server) on every single request
  - This is no longer a load balancer — it's a full application-aware proxy with
    its own failure modes, harder to test, and more critical to uptime.

**Why Option A wins:** the connection count concern is the only real cost, and
it's trivially small compared to the operational complexity of building an
application-aware routing proxy. The decoupled scaling and free failover are
worth far more than saving a few dozen DB connections.

---

### 13.7 Schema Changes for Sharding

**Per-shard `users` table** — identical to the current schema except `id` is no
longer `AUTO_INCREMENT`:

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,       -- snowflake ID, app-generated
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,
  nickname       VARCHAR(128) NOT NULL,
  avatar_suffix  CHAR(8)      NULL,
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)              -- safety net within-shard; global uniqueness via lookup
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

**Global lookup table** (unsharded, separate instance):

```sql
CREATE TABLE username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

**What changes in Go code:**

- **ID generation:** `InsertUser` no longer reads `LastInsertId()`. The app
  generates the snowflake ID before insert and passes it in. A new
  `idgen.Generator` struct holds the machine ID and sequence counter.
- **Shard routing:** `db.Handle` becomes a shard-aware type: a slice of pools +
  a `ShardFor(userID) int` function (consistent hash, binary search on vnodes)
  that picks which pool to use per query.
- **Login path:** `lookupUserForLogin` hits the username lookup first to get
  `user_id`, then routes to the correct shard for the full row. The auth cache
  already carries `id` in `model.AuthCached`, so a cache hit still skips the
  full-row MySQL query — it just needs the lookup for routing.
- **Signup path:** generates snowflake ID, inserts into lookup (catches global
  duplicates), then inserts into the shard. On shard-insert failure, deletes
  from lookup (rollback).

**What stays unchanged:**

- `row_version`, `avatar_suffix`, the `LAST_INSERT_ID(row_version+1)` trick —
  all operate on a single row by ID on one shard.
- Cache keys (`auth:<username>`, `{user:<id>}`, `sess:<token>`) — keyed by
  username or user_id, not by shard. Redis doesn't care which MySQL instance the
  data came from.
- Lock stack write path (lock → DEL → DB UPDATE → write-through → unlock) —
  operates on the same `{user:<id>}` key regardless of which shard the write
  went to.
- Session model — carries `user_id` only, shard-agnostic.
- File-first avatar ordering — writes to local filesystem, then updates one row
  by ID on one shard. The filesystem sharding (`user_id & 0x1FFF`) is orthogonal
  to DB sharding.

---

### 13.8 Migration — Adding a Shard Without Downtime

Adding a new DB shard mid-flight is the operationally hardest part. With
consistent hashing, ~1/N of users need to move. The standard approach has
phases:

**Phase 0 — Before:** all traffic uses the current consistent hash ring (e.g.
3 shards). The new shard (shard 3) exists but has no data and no traffic.

**Phase 1 — Dual-write, read-old:**
```
Write path:  compute old_shard = ShardFor_v1(user_id)
             compute new_shard = ShardFor_v2(user_id)
             write to BOTH (old first, new second)
Read path:   still reads from old_shard only
```
New writes land in both places. Reads are still correct because old shard has
everything. Users that didn't move under the new hash are writing to the same
shard twice (old == new), which is idempotent for INSERT and fine for UPDATE.

**Phase 2 — Backfill:** a background job copies all historical rows that moved
under the new hash from old shard → new shard. This is an idempotent upsert by
PK. Can take hours for millions of rows but causes zero downtime — it's just
background writes. The dual-write from phase 1 ensures nothing written during
the backfill is missed.

**Phase 3 — Dual-write, read-new:**
```
Write path:  still writes to both
Read path:   switches to new_shard (using ShardFor_v2)
```
Grayscale this: 1% of reads from the new routing, then 10%, then 50%, then 100%.
Monitor error rates at each step. This is a canary rollout — feature-flagged in
the shard routing layer (the `ShardFor` function checks a migration-phase config
value), not in the handlers themselves.

**Phase 4 — Cutover:** stop writing to old shard locations for moved users.
Remove old shard routing code. Done.

**Where the complexity lives:** `ShardFor(userID)` becomes
`ShardFor(userID, migrationPhase)` during the transition. The handlers don't
change — they still call `db.GetUserByID` etc. — but the pool router underneath
knows to dual-write or to read from the new location. Once migration is
complete, the phase flag is removed and `ShardFor` returns to a simple hash.

**Alternative approach (read-old, write-new):** similar to above but the read
path stays on the old shard longer, while new writes go only to the new shard.
This requires the backfill to complete before reads can switch, whereas the
dual-write approach allows reads to switch as soon as the backfill covers the
rows that moved. The dual-write approach is more complex but has a shorter
total migration window.

**Zero downtime?** Yes, if the phases are implemented correctly. The user never
sees an error. During the backfill window, a user might read slightly stale data
if they're reading from an old shard while a new write went to the new shard,
but this is the dual-write phase where writes go to both, so it doesn't arise.

---

### 13.8.1 Rate Limiting in the Sharded Context

Rate limiting in the sharded context follows the same design and shelved status
as §4.8; no additional sharding-specific rate limiting concerns arise. The Redis
counters (`rl:login:ip:<ip>`, `rl:login:user:<username>`) are keyed by IP or
username, not by shard, so they work identically regardless of how many data
shards exist. When implemented, the enforcement point is still the TCP server
login handler, before bcrypt.

---

### 13.9 Availability vs. Sharding (Separate Concerns)

**Status: shelved — considered and documented but not implemented.** Same
rationale as rate limiting (§4.8): the current design accepts replicas cleanly
without restructuring (see below), and building replication infrastructure for a
localhost entry task with one MySQL instance is out of scope.

Sharding solves **load distribution**, not **availability**. A shard going down
takes its subset of users offline — sharding actually increases the blast radius
per-instance compared to a single DB (a fraction of users down vs degraded
performance for all), but reduces load per instance so each one is less likely to
fail under pressure. They are independent axes that compose:

```
TCP servers (any can reach any shard, round-robin)
    │
    ├── Shard 0:  Primary ←→ Replica  (failover via replication)
    ├── Shard 1:  Primary ←→ Replica
    └── Shard 2:  Primary ←→ Replica
```

| Concern | Who handles it | What the Go code does |
|---|---|---|
| Shard routing | App code (`ShardFor(userID)`) | Picks which pool to use |
| Primary down | MySQL replication + orchestrator (ProxySQL, Vitess, etc.) | DNS/VIP flips to replica; pool reconnects on next `Acquire` (existing `isBadConn` retry handles this) |
| Adding a shard | App code (update consistent hash ring) + migration (§13.8) | Add a new pool entry, run dual-write migration |
| Read scaling | Read replicas per shard | Optional read-only pool per shard; route SELECTs there, UPDATEs to primary |

#### Replication Modes and Staleness Guarantees

| Mode | Data loss on failover | Read-from-replica staleness | How it works |
|---|---|---|---|
| Async (MySQL default) | Possible (unreplicated writes lost) | Unbounded (seconds of lag possible) | Primary commits, ships binlog asynchronously. Replica applies whenever it gets around to it. |
| Semi-sync | None (≥1 replica ACKs before commit) | Still present (ACK = received, not applied) | Primary waits for ≥1 replica to ACK receipt of the binlog before returning OK to the client. But "receipt" ≠ "applied" — the replica may not have replayed it yet. |
| Group Replication (Paxos-based) | None (majority consensus) | Still present on secondaries (log replay lag) | MySQL's built-in consensus protocol. Leader proposes → majority ACK → commit. Guarantees log consistency across a majority, but applying the log to state still has a replay lag on followers. |

**Key insight:** replication guarantees consistency of the **log**, not
zero-staleness of **reads from followers.** Raft/Paxos ensure every node agrees
on the order of operations, but a follower that hasn't finished replaying the
latest committed entry still serves stale data if you read from it.

For zero-stale reads: **read from the primary.** This always works but defeats
the point of read replicas for that query.

**"Read your own writes" pattern** — the practical middle ground:
```
1. User updates nickname → write goes to primary, gets back GTID (transaction ID)
2. Next read from this user → pass GTID to the replica:
   "wait until you've replayed up to this transaction"
   (MySQL: WAIT_FOR_EXECUTED_GTID_SET(gtid, timeout))
3. Replica either catches up (typically ms) or times out → fall back to primary
```
The user who just edited always sees their own change; everyone else tolerates a
brief lag. This is how large-scale read replicas work without sacrificing the
"I just changed my nickname but it shows the old one" experience.

**For this system:** DB shards each have a single primary (no replicas yet), so
DB-level staleness isn't a concern — every read hits the primary. The lock stack's
`row_version` + version-in-value cache design already handles the **cache**
staleness problem (the same class of issue, just at the Redis layer instead of the
DB layer). If read replicas were added per shard, `GetUserByID` could route to a
replica and `UpdateNickname` to the primary — the lock stack cache design would
still work identically, since the cache is populated from whichever source
answered, and a stale replica would just produce a cache entry with a lower
version that the lock stack's Lua compare-and-SET would reject if a newer version
arrived.

---

### 13.10 Implementation Status

Implemented. The sharding design above is fully wired:

**Lookup shards:** 2 shards (`lookup_0`, `lookup_1`), routed by
`hash(username) % 2`. Global username uniqueness enforced per-shard via PK
constraint. Each holds ~50% of the username→user_id mappings. Configured via
`LOOKUP_SHARD_DSNS` in `.env`.

**Data shards:** 2 shards (`data_0`, `data_1`), routed by consistent hash ring
with 150 vnodes per shard (`SHARD_VNODES`). Scalable to 3+ via the migration
phases in §13.8. Configured via `DATA_SHARD_DSNS` in `.env`.

**ID generation:** snowflake IDs via `internal/idgen`, machine ID from
`MACHINE_ID` env var.

**Localhost setup:** all shards are separate MySQL schemas (databases) on the
same mysqld instance(s). The Go code doesn't know or care whether pools point at
different ports or different database names on the same port — the routing,
sharding, and migration logic is identical either way. The only thing this
doesn't demonstrate is physical load isolation between shards, which isn't
meaningful on localhost since they share the same CPU, disk, and buffer pool.

See §14 for the full configuration reference.

---

## 14. Configuration / Environment Variables

All configuration is loaded from environment variables, with `.env` file support
(see `internal/config/config.go`). Real env vars take precedence over `.env`.
See `.env.example` for a complete template.

### HTTP Server

| Variable | Example | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `HTTP_TCP_POOL_SIZE` | `8` | Number of persistent TCP connections to the backend |
| `WEB_ROOT` | `web` | Directory for static assets (HTML/JS) |
| `HTTP_READ_HEADER_TIMEOUT_SECONDS` | `5` | |
| `HTTP_READ_TIMEOUT_SECONDS` | `15` | |
| `HTTP_WRITE_TIMEOUT_SECONDS` | `15` | |
| `HTTP_IDLE_TIMEOUT_SECONDS` | `60` | |

### TCP Server

| Variable | Example | Purpose |
|---|---|---|
| `TCP_LISTEN` | `:9000` | TCP listen address |

### MySQL Sharding

| Variable | Example | Purpose |
|---|---|---|
| `DATA_SHARD_DSNS` | `app:app@tcp(.../data_0),app:app@tcp(.../data_1)` | Comma-separated DSNs for data shards |
| `LOOKUP_SHARD_DSNS` | `app:app@tcp(.../lookup_0),app:app@tcp(.../lookup_1)` | Comma-separated DSNs for lookup shards |
| `SHARD_VNODES` | `150` | Virtual nodes per shard in the consistent hash ring |
| `MYSQL_DATA_MAX_OPEN` | `10` | Max open connections per data shard pool |
| `MYSQL_DATA_MAX_WAIT_MS` | `200` | Max wait time for a connection from data pool |
| `MYSQL_DATA_CONN_MAX_LIFETIME_SECONDS` | `300` | |
| `MYSQL_DATA_MAX_IDLE_TIME_SECONDS` | `120` | |
| `MYSQL_DATA_SWEEP_INTERVAL_SECONDS` | `30` | Idle connection sweeper interval |
| `MYSQL_DATA_MAX_BAD_CONN_RETRIES` | `3` | |
| `MYSQL_LOOKUP_MAX_OPEN` | `5` | Max open connections per lookup shard pool |
| `MYSQL_LOOKUP_MAX_WAIT_MS` | `200` | |
| `MYSQL_LOOKUP_CONN_MAX_LIFETIME_SECONDS` | `300` | |
| `MYSQL_LOOKUP_MAX_IDLE_TIME_SECONDS` | `120` | |
| `MYSQL_LOOKUP_SWEEP_INTERVAL_SECONDS` | `30` | |
| `MYSQL_LOOKUP_MAX_BAD_CONN_RETRIES` | `3` | |

> **Note:** `.env.example` uses `SHARD_DATA_MAX_OPEN` and `SHARD_LOOKUP_MAX_OPEN`
> but `config.go` reads `MYSQL_DATA_MAX_OPEN` and `MYSQL_LOOKUP_MAX_OPEN`. The
> `.env.example` names are not loaded by the config loader — this is a mismatch
> that should be reconciled (either rename the env vars in `.env.example` or
> update `config.go`).

### ID Generation

| Variable | Example | Purpose |
|---|---|---|
| `MACHINE_ID` | `0` | Snowflake machine ID (0–1023). Must be unique per TCP server instance. |

### Redis

| Variable | Example | Purpose |
|---|---|---|
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis address |
| `REDIS_PASSWORD` | (empty) | Redis auth password |
| `REDIS_DB` | `0` | Redis database index |
| `REDIS_POOL_SIZE` | `50` | go-redis connection pool size |
| `REDIS_MIN_IDLE_CONNS` | `10` | Minimum idle connections kept warm |
| `REDIS_CONN_MAX_LIFETIME_SECONDS` | `1800` | |
| `REDIS_POOL_TIMEOUT_MS` | `200` | |
| `REDIS_IO_TIMEOUT_MS` | `500` | |
| `REDIS_PING_TIMEOUT_SECONDS` | `2` | |
| `CACHE_LAYER` | `on` | Set to `off` to disable user/profile cache (for perf baseline) |
| `CACHE_AUTH_TTL_SECONDS` | `600` | TTL for `auth:<username>` keys |
| `CACHE_USER_TTL_SECONDS` | `3600` | TTL for `{user:<id>}` keys (also used as lock stack cache TTL fallback) |
| `CACHE_LOOKUP_TTL_SECONDS` | `600` | TTL for `lookup:<username>` keys |

### Lock Stack

| Variable | Example | Purpose |
|---|---|---|
| `LOCKSTACK_LOCK_TTL_MS` | `2000` | Per-key write lock TTL (watchdog extends while writer is alive) |
| `LOCKSTACK_LEASE_TTL_MS` | `500` | Reader lease TTL (must be > DB query timeout) |
| `LOCKSTACK_CACHE_TTL_SECONDS` | `3600` | TTL for cache entries written by the lock stack |
| `LOCKSTACK_POLL_MIN_US` | `500` | Initial backoff for reader polling (microseconds) |
| `LOCKSTACK_POLL_MAX_US` | `4000` | Maximum backoff cap for reader polling (microseconds) |
| `LOCKSTACK_MAX_LOCK_RETRIES` | `200` | Max attempts to acquire the write lock before failing |

### Uploads

| Variable | Example | Purpose |
|---|---|---|
| `UPLOADS_ROOT` | `uploads` | Root directory for avatar file storage |
| `UPLOADS_MAX_BYTES` | `2097152` | Max upload size (2 MB) |

### Bcrypt

| Variable | Example | Purpose |
|---|---|---|
| `BCRYPT_COST` | `5` | Must match cost used by the seeder (§4.3, §7) |

### Sessions

| Variable | Example | Purpose |
|---|---|---|
| `SESSION_TTL_SECONDS` | `86400` | Session lifetime (24h), refreshed by sliding window |
