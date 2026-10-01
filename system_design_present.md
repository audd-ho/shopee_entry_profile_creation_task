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
                                          │  lookup,  │ │ session │
                                          │  custom   │ │ + lock  │
                                          │ conn pool)│ │  stack  │
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
  - **Two separate caches, not one.** `auth:<username>` holds only `{id, password_hash}` — exactly what verifying a password and minting a session need. `{user:<user_id>}` holds `{username, nickname, avatar_suffix}` in a version-in-value format (`<row_version>\n<JSON>`) — everything a page needs to display, fetched only after a session exists. Neither cache holds a field the other one needs; there is no overlap.
  - The username→bcrypt-hash idea below was the starting point, and is still correct as far as it goes — it just turned out to be half the picture once profile data (nickname, avatar) needed caching too, and splitting by *when it's needed* (before vs. after a session exists) rather than caching one wide record per user turned out to matter for invalidation, not just for payload size.
- TTL?
  - `auth:<username>`: 10 min.
  - `{user:<user_id>}`: 1 hr (configured via `LOCKSTACK_CACHE_TTL_SECONDS`).
- Invalidation policy on profile edit: lock stack with delete-first + write-through (supersedes earlier delete-on-write — see below)
- Negative caching (cache "user not found" for a short TTL to protect against enumeration/lookup spam)
  - Not needed: enumeration is already mitigated by the dummy bcrypt verify on unknown usernames (§7), and lookup-spam is a perf concern only under targeted attack — out of scope for a localhost entry task. Easy to add later if needed.
- **DB versioning — implemented as version-in-value via the lock stack**
  - `row_version` column in MySQL bumps on every mutating write
  - Version is embedded in the cached value (`<row_version>\n<JSON>`), not in the key — the key is a single `{user:<id>}` per user
  - **Write path (lock stack):** lock `{user:<id>}:lock` → DEL `{user:<id>}` (delete-first) → DB UPDATE → Lua `versionSetScript` write-through (version compare-and-SET) → unlock. Writers are serialized by the lock; the Lua script rejects a write if the cache already holds a same-or-newer version
  - **Read path (lock stack):** GET `{user:<id>}` → hit with real data? serve immediately. Miss or lease value? → `readCheckScript` (atomically check lock + lease) → GRANTED? → DB SELECT → `leaseSetScript` (populate only if lease token still held). Readers blocked by a lock wait for the writer's write-through; readers blocked by another reader's lease wait for its populate
  - The earlier version-in-key reasoning below is historical — it was superseded by the lock stack design which uses version-in-value + leases to solve the slow-reader overwrite problem (see `lock_stack_implementation_guide.md`)

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

**Decision (historical): Delete-on-write.** The original reasoning below led to delete-on-write as a starting point. **Superseded by the lock stack** (implemented), which combines the best of both: delete-first ordering (safety net from delete-on-write) + lock-protected write-through (no extra DB read on next access) + reader leases (no slow-reader overwrite). See `lock_stack_implementation_guide.md` for the full design.

Original delete-on-write reasoning (still valid as background for why the lock stack was needed):
- **No sync problem:** delete removes the cache entry — there is nothing left to be out of sync with the DB. Write-through keeps data in both DB and cache, so both sides must adhere to the same state; any ordering bug, serialization error, or concurrent-writer race (see above) silently puts them out of sync.
- Race-safe under concurrent writers: DEL is idempotent and order-independent; write-through SET is order-dependent (see race analysis above)
- The slow-reader overwrite problem is solved by the lock stack's reader lease mechanism (M13) — a reader populates cache only if its lease token is still present; any writer's DEL wipes the lease. (Earlier version-in-key solved this differently — a slow reader writing to `user:<id>:v5` could not overwrite `user:<id>:v6` because they were different keys.)
- The lock stack eliminates the "one extra DB read on next access" cost: the writer's write-through populates the cache before unlocking, so readers after unlock get a cache hit immediately

**Also considered — CAS/lease (compare-and-swap):**

Two Redis keys per user: a version counter (`user:<id>:version`, value is an integer) and the cached data (`user:<id>`, value is `{data, version}`).

```
Write: INCR user:<id>:version → N+1.  DB UPDATE.  SET user:<id> {data, version: N+1}.
Read:  GET user:<id>:version → N+1.  GET user:<id> → {data, version: ?}.
       version matches? serve from cache.  mismatch? fall through to DB.
```

Because the counter is incremented before the DB write, any reader during or after the write sees a mismatch and goes to DB — eliminating stale reads entirely.

This is a valid approach, especially for workloads where even ms-level staleness is unacceptable. The implemented lock stack design incorporates elements of this pattern — the writer holds a lock (serializing concurrent writes), uses delete-first ordering as a safety net, then writes through with a Lua version-compare script. On the reader side, leases (M13) prevent slow-reader overwrites. See `lock_stack_implementation_guide.md` for the full design and the failure-mode analysis that led to this combination.

**Invalidation triggers (updated for the lock stack + sharding):**

| Action | Keys invalidated | Mechanism |
|---|---|---|
| Nickname update | `{user:<user_id>}` | Lock stack write path (lock → DEL → DB UPDATE → write-through → unlock) |
| Avatar update | `{user:<user_id>}` | Lock stack write path (same) |
| Signup | none (no negative caching) | — |
| Logout | `sess:<token>` | Plain DEL (no lock stack — session is not version-controlled) |
| Password change (future) | `auth:<username>` + all sessions for that user | — |

**Negative caching note:** if negative caching is added later, signup MUST invalidate that marker (`DEL user:<newuser>`), else login for a just-created user would incorrectly report not-found for the TTL window. This invariant is documented here so it cannot be missed when implementing.

**Versioning granularity — per-column vs per-row:**

- Per-column versioning
  - Pro: fields version independently — changing nickname does not bump avatar's version
  - Pro: scales if more independently-versioned fields are added later
  - Con: maintenance cost grows with each versioned field
  - Con: to truly decouple, tables must be split (FK by id) so field A's change doesn't lock field B's row — adds JOIN cost on every read
  - Con: not justified here — only 2 mutable fields (nickname, avatar), both cached together in `{user:<id>}` and both read together on the hot path. Per-column versioning would require either splitting the cache or double-versioning a single cache entry, for no benefit
  - Con: nickname and avatar changes are infrequent per-user — row-level lock contention between them is negligible
- Per-row versioning — the inverse trade-offs; preferred given both mutable fields share the same cache entry, the same read path, and the same low-frequency update pattern

**Implementation note (resolved in §4.4.4):** the per-row version above is a dedicated `row_version` column. It was originally kept separate from an `avatar_version` column (§4.4) for exactly the reason below — but `avatar_version` was later removed entirely (§4.4.4 point 3a), so today `row_version` is simply the only version counter in the schema. `row_version` bumps on *any* mutating write (nickname or avatar) and is embedded in the **value** of the `{user:<user_id>}` Redis key (as `<row_version>\n<JSON>`) — not in the key itself, and **not** in `auth:<username>`, which is deliberately left unversioned. The session record does not carry `row_version` either — the lock stack's single-key design means the reader does not need a version hint to form the cache key. The historical reasoning for keeping `row_version` and `avatar_version` as two counters, back when both existed: conflating them would have made a nickname-only edit bump `avatar_version` too, pointing avatar URLs at a version whose file was never written — see §4.4.4 point 4 for the full race analysis, which remains accurate as a record of why that design used two counters, even though only one exists now.

---

#### 4.1.1 Auth cache vs. user cache — the final split (post-implementation)

The table above already reflects the outcome; this subsection is the reasoning, since it took a few rounds to land on.

**The starting design cached one record per user under `user:<username>`,** containing everything a login might eventually need to display: `id`, `password_hash`, `nickname`, `avatar_suffix`, and (once §4.1's versioning was added) `row_version`. That is a natural first cut — one user, one cache entry — but it conflates two things with genuinely different lifecycles: *authenticating* (needs only the password hash) and *displaying a profile* (needs nickname/avatar, never the hash). Splitting them into `auth:<username>` and `{user:<user_id>}` is what the current design does, and each field question below was resolved by asking which of those two jobs actually needs it.

**Why `row_version` does not live in `auth:<username>` or in the session** (historical context — this question is now moot, but the reasoning was important in reaching the current design):

An intermediate version of this design put `row_version` in `auth:` to seed a session's cache-version hint, and a later version fetched it fresh from MySQL on every login. Both were needed because the cache key was `user:<id>:v<N>` — a reader needed the version *before* forming the key. **The lock stack design eliminated this entire problem:** the cache key is now a single `{user:<id>}` with version-in-value, so the reader does not need to know any version to look up the cache. The session no longer carries `row_version` at all, and login no longer fetches it for session seeding — it is only read/written inside the lock stack's own paths. This simplified login (one fewer MySQL SELECT on the auth-cache-hit path) and the session model (just `user_id, created_at, last_seen, ip`).

**What this means for invalidation:** `auth:<username>` now holds only `id` and `password_hash`, both immutable in this service's current feature set (no change-password endpoint exists). So it needs **no invalidation path at all** today, beyond its own TTL — nickname and avatar edits touch only `{user:<user_id>}` via the lock stack write path. `InvalidateOnPasswordChange` is kept in the codebase even though it has no caller, against the day a change-password feature exists — deleting working code for an out-of-scope feature just means writing it again later.

**What this means for the session record:** with nothing left that needs `username` for invalidation (see the previous paragraph), and `get_user`'s response able to read `username` from the `{user:<user_id>}` cache just as well, `Session.Username` was removed. With the lock stack's single-key design removing the need for a version hint, `Session.RowVersion` was also removed. The session record's only remaining job is "who is this, and are they still logged in": `{user_id, created_at, last_seen, ip}`.

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
| Fit for this task | Reuses Redis already required for caching — no new infrastructure | Stateless benefit (no session store) is irrelevant with one backend already connected to Redis |
| Rotation on profile change | Trivial (update Redis value) | Requires re-issuing and re-signing the token |
| Complexity | Low — opaque token, Redis GET/SET/DEL | Higher — signing key management, verification, claims schema |
| ***explicit single use?*** | supposed to be long lasting and easy ti remove/log out and revoke | not easy to revoke and meant more for single one time use like auth |

**Decision: Session cookie backed by Redis.** 

**Sub-decisions:**
- Cookie attributes: `HttpOnly`, `SameSite=Lax` (or `Strict`?), `Secure` (production only). 
- Session TTL: 24h, sliding window (refresh when remaining TTL drops below half).
  - Sliding chosen over fixed because a user actively editing their profile should not be logged out mid-session — activity extends the window.
  - Refresh fires only in the second half of the TTL to avoid turning every authenticated request into a Redis write.
- Session key format: `sess:<random-256-bit>` → JSON `{user_id, created_at, ip, last_seen}` in Redis.
  - `ip` stored for future suspicious-activity detection (different IP mid-session); not enforced currently.
  - `last_seen` used to implement the sliding window refresh.
  - No `row_version` — the lock stack's single-key design (`{user:<id>}` with version-in-value) means the reader does not need a version hint from the session to form the cache key.

**Status:** session cookie + sliding TTL decided.

---

### 4.3 Password Hashing

**Options:**
- (A) **bcrypt** (`golang.org/x/crypto/bcrypt`) — battle-tested, salt built-in.
- (B) **Argon2id** (`golang.org/x/crypto/argon2`) — OWASP-recommended, memory-hard.
- (C) SHA-256 + salt (weaker, faster).

**Considerations:**
- Never plaintext, never plain SHA.
- bcrypt cost 10 ≈ ~50-100ms/hash on modern CPU. Cost 12 ≈ ~250-400ms.
- Argon2id is stronger but requires tuning three parameters (memory, iterations, parallelism) vs bcrypt's one (cost). Memory-hard by design — at high concurrency on this dev machine, concurrent hashes can exhaust available memory and OOM the process. bcrypt is CPU-bound only, bounded and predictable.
- **Perf impact:** login RPS test does 3k-20k login/sec. If every login re-hashes, at cost 10 we'd need ~150-1000 CPU cores just for bcrypt.
  - **Mitigation:** the `auth:<username>` cache means repeat logins for the same user verify the cached hash via `bcrypt.Compare` without a MySQL round trip. With ≥200 unique users and a 10-minute auth cache TTL, most logins within a test window are cache hits.
  - **Assumption:** the performance harness counts each HTTP POST to `/api/login` as one "login request", including cache-hit logins.

**Decision: bcrypt, cost 5.** (Initially cost 10, scaled down after performance testing.)

**Why bcrypt over Argon2id:**
- One tunable parameter (cost) vs three (memory, iterations, parallelism) — less to misconfigure
- Salt built-in to the 60-byte output — no separate salt column, no manual generation
- CPU-bound only — no OOM risk under concurrency, unlike Argon2id's memory-hard design
- **Retroactively upgradeable:** bcrypt embeds the cost in the hash itself, so existing hashes at cost 5 still verify correctly if the server cost is later raised to 10 or 12. New signups hash at the new cost immediately; existing users can be rehashed on their next successful login (verify at old cost, re-hash at new cost, update row). No migration, no downtime, no breaking existing passwords.

**Why cost 5, not cost 10:**
- Cost 10 ≈ 50-100ms/hash; at 3k RPS with 200 unique users, the auth cache absorbs repeats but each user's first login (cache miss) pays the full bcrypt cost. 200 cold logins at cost 10 ≈ 10-20 seconds of CPU — a bottleneck on a single machine. Cost 5 ≈ 4.5ms/hash, reduces this to ~0.9s.
- **Assumption:** cost 5 is acceptable for a localhost entry task where the threat model does not include offline brute-force of a leaked database. Still salted (resistant to rainbow tables), still stronger than unsalted SHA-256. Can be raised retroactively (see above) if deployed to production.

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
| Fit for entry task | ✅ No external deps — a directory is sufficient for single-machine scope | ⚠️ Row size increase reduces buffer pool efficiency for 10M rows, on a hot path that never touches images | ❌ Adds infrastructure (creds, SDK, billing) with no hot-path benefit |
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
     - **Uniqueness under concurrency without a counter:** two concurrent uploads by the same user still cannot collide, because each independently draws a fresh 32-bit random suffix — the same property a monotonic counter provided, now from randomness instead of DB atomicity. This is the piece that makes dropping the counter *safe*, not just a reduction in moving parts: without it, "no shared filename" would have had no guarantee at all under concurrent uploads.
   - **What this fixes:** the row can now only ever point at a suffix if the file for that suffix was *already* written successfully — there is no ordering under which MySQL commits before the file exists. A failed `UPDATE` (rare — id vanished, connection dropped) leaves an orphaned file and an **unchanged, still-valid** row; the client is correctly told the upload failed, and the persisted state agrees with that. Point 3's "correct DB, missing file" state is no longer reachable at all, not just less likely.
   - **What this costs:** the orphan sweeper (see "Orphan cleanup" below) loses the ability to tell which file is current by reading filenames alone — a random suffix carries no ordering information, unlike a version number. It now needs one MySQL read per swept user to learn the authoritative `avatar_suffix`, where the old scheme needed none. This is a real, ongoing cost (a DB read per cleanup pass instead of zero), traded for eliminating a data-corruption bug outright — worth it, but not free.
   - **The new race this ordering opens up, and its fix:** a file can now legitimately exist on disk *before* the database has been told about it — the normal state during the gap between the rename below landing and the `UPDATE` committing. A sweeper that decides what to keep by asking "does the DB currently point at this file?" would, during exactly that gap, see a brand-new file the DB doesn't yet reference and correctly-but-wrongly conclude it is an orphan. Deleting it there is worse than the bug this reordering fixed, because it needs no crash at all — an ordinary sweep tick landing at an unlucky moment is enough, and at 10M-user scale, anything that *can* happen on a given upload *will* happen periodically. **Fix: any sweeper must skip every file younger than a fixed grace period (`AvatarSweepGracePeriod` in code, 10 minutes), checked via filesystem mtime, before ever consulting the database — unconditionally, regardless of what the DB says.** Ten minutes is two to three orders of magnitude longer than an upload plus its `UPDATE` could plausibly take, so it costs nothing in the common case while closing the race structurally rather than probabilistically. No sweeper exists in the codebase yet; this requirement is recorded here and as a named constant in code specifically so whichever one is written next cannot miss it.

4. **`row_version` / `avatar_version` conflation race (historical — kept as a record of why the two counters were designed separately; `avatar_version` no longer exists, see point 3a):** §4.1 decided **per-row** cache versioning: a single version number that bumps on *either* nickname update or avatar upload, used to build the `user:<user_id>:v<N>` Redis cache key (`auth:<username>` is unversioned and, since §4.1.1, does not carry `row_version` at all — §5). `avatar_version` (this section, when it still existed) bumped **only** on avatar upload and was embedded in the served file's **name**, not just a cache key.
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
7. Lock stack write path wraps steps 5–6: lock `{user:<user_id>}:lock` →
   DEL `{user:<user_id>}` → DB UPDATE → Lua versionSetScript write-through →
   unlock. `auth:<username>` is NOT touched: it holds nothing an avatar edit
   could make stale.
```
If step 5 fails, the file written in steps 2-4 is simply an orphan — swept up later, same as any other orphan — and the row is exactly as it was before the request. There is no ordering under which this flow can leave MySQL pointing at a suffix whose file does not exist.

**Nickname update flow, for symmetry (no file involved, so only `row_version` moves):**
```
1. MySQL: UPDATE users SET nickname = ?, row_version = LAST_INSERT_ID(row_version + 1) WHERE id = ?
   → then SELECT LAST_INSERT_ID() to get new_row_version
2. Lock stack write path: lock → DEL `{user:<user_id>}` → (DB UPDATE is step 1) →
   Lua versionSetScript write-through → unlock. `auth:<username>` not touched,
   same reasoning as the avatar flow above
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
- Fewer failure modes in the upload path in exchange; orphans are harmless (just disk), cleaned up on the sweeper's next pass.

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

**Decision:** `net/http`.
- The 3k-6k RPS target is achievable with any Go HTTP framework — framework choice is not the bottleneck (bcrypt and DB are).
- stdlib has no external dependencies to manage.
- Demonstrates understanding of Go's HTTP primitives (handlers, middleware, mux) without framework abstraction — appropriate for an entry task.
- Boilerplate cost is acceptable given the small number of endpoints (7 routes).

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
  - Matches the task requirement: "Connect by MySQL Go client. Connection Pool should be built on your own" — the driver IS the "Go client"; the pool is what we build.

- **(D) Full DIY** — implement the MySQL wire protocol ourselves over `net.Conn`.
  - The task says "MySQL Go client" — implementing the wire protocol is building a client, not building a pool. Out of scope.

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
| Build custom Redis pool | Learning consistency, deeper practice, one abstraction across both stores. Redis pool has fewer concerns than MySQL (no prepared stmts, no transactions). | ~1 extra day. Risk of subtle bugs (RESP edge cases, reconnection storms). Duplicates existing well-tuned library. |
| Middle: thin wrapper around `go-redis` | ~2 hours. Get consistent API + custom metrics/timeouts across both stores. | Not truly custom; still relying on library for the hard parts. |

**Not choosing "build custom" is deliberate choice:**
- MySQL pool is the interesting one (prepared statements, driver-level integration, protocol nuance).
- Redis pool is mechanically similar but has fewer edge cases (no prepared statements, no transaction state) → less learning value per hour spent.
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
- gRPC is battle-tested but its HTTP/2 substrate is arguably "not TCP" — risky interpretation of the task constraint.
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
- Number of persistent TCP connections HTTP→TCP: 8. Confirmed during performance testing — the TCP link is not the bottleneck (multiplexing distributes load across connections, and the bottleneck is bcrypt/DB, not framing).
- Per-request timeout: 2s.
- Reconnect-on-failure is implemented. Proactive health-check pings for idle connections are not — deferred because connections are rarely idle under load, and reconnect-on-failure covers the crash case.

---

### 4.8 Rate Limiting [rough design but not implemented]

**Options:**
- (A) Redis counter per IP + per username, sliding window / fixed window.
- (B) Token bucket in-memory on TCP server.
- (C) None (rely on network layer).

**Considerations:**
- Login is the abuse target → rate limit at TCP server (auth authority).
- Redis lets us share state if we ever scale to multiple TCP servers.
- Fixed window requires fewer operations (`INCR` + `EXPIRE`); sliding window is more accurate but needs more Redis ops per check.

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

### 4.9 Input Validation

**Rules:**
| Field | Rule |
|---|---|
| username | `^[a-zA-Z0-9_]{3,32}$`, ASCII, lowercase-normalized |
| password | min 8 chars, max 72 (bcrypt limit), reject all-same-char |
| nickname | valid UTF-8, 1-32 **runes** (not bytes), strip control chars |
| image | ≤ 2 MB, MIME sniff, allowlist: jpeg/png/webp |

**Enforcement:** both HTTP server (early reject) and TCP server (source of truth).

**Decision: as above.** 

- Auth middleware (session cookie presence check) is a separate concern — it validates identity, not user-supplied data. Both are enforced but categorized separately.

---

### 4.10 SQL Injection Prevention

**Approach:** parameterized queries only. Never string-concat SQL.

```go
// GOOD
db.Query("SELECT id, nickname FROM users WHERE username = ?", username)
```

**Enforcement:** code review + optional `grep` for `fmt.Sprintf.*(Query|Exec)`.

**Decision: enforced by discipline + review.** ✅

---

### 4.11 Prepared Statements

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

## 5. Data Model

### `users` table

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,              -- snowflake ID, app-generated (see §13.3)
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,      -- bcrypt output is 60 bytes
  nickname       VARCHAR(128) NOT NULL,       -- 32 runes × up to 4 bytes
  avatar_suffix  CHAR(8)      NULL,           -- 8 hex chars (32 bits random); NULL when no avatar
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,    -- bumped on any mutating write; version-in-value for lock stack (§4.1)
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

**Per-lookup-shard: username → user_id mapping (global uniqueness):**

```sql
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
- `id` is a snowflake ID generated by the application (see §13.3), not `AUTO_INCREMENT`. Each TCP server generates IDs locally with no coordination.
- `utf8mb4` for full unicode (emojis in nicknames etc.).
- `utf8mb4_bin` collation → case-sensitive username; use lower-cased username for lookups if we want case-insensitive.
- Unique index on `username` is a safety net within each data shard; global uniqueness is enforced by the `username_lookup` table on the lookup shards (see §13.4).
- `VARBINARY(60)` for bcrypt (fixed size).
- Avatar path is **derived**, not stored: `/uploads/<id & 0x1FFF>/<id>/<id>_<avatar_suffix>.jpg` (see §4.4). `avatar_suffix` being NULL is the sole signal for "no avatar" — there is no version to check alongside it.
- Row footprint is smaller than the version-numbered scheme (one fewer `INT` column) as well as than storing a full path — meaningful at 10M rows for buffer pool efficiency.
- `row_version` is unrelated to the avatar scheme above, and unaffected by removing `avatar_version` — the two were already deliberately separate counters (§4.4.4 point 4). `row_version` is embedded in the **value** of the `{user:<id>}` Redis cache key (as `<row_version>\n<JSON>`) for the lock stack's version-compare write-through, and never appears in an avatar filename.

### Redis keys

| Key | Value | TTL | Purpose |
|---|---|---|---|
| `sess:<token>` | JSON `{user_id, created_at, ip?, last_seen}` | 24h sliding | Session identity. Token is 256-bit random. |
| `auth:<username>` | JSON `{id, password_hash}` | 10 min | Login auth cache. NOT versioned. |
| `{user:<user_id>}` | `<row_version>\n<JSON>` (version-in-value) | 1 h | Profile display cache. Managed by lock stack. |
| `{user:<user_id>}:lock` | `<writer_id>` | LockTTL (2s default) | Lock stack write lock. |
| `lookup:<username>` | JSON `{user_id}` | 10 min | Username → user_id mapping cache (sharding). |
| `rl:login:ip:<ip>` | counter | 5 min | Rate limit (shelved). |
| `rl:login:user:<uname>` | counter | 15 min | Rate limit (shelved). |

**Why `auth:<username>` is unversioned:** `auth:` holds only `id` and `password_hash`, both immutable in the current feature set (no change-password endpoint). It needs no versioning and no invalidation beyond its own TTL. Nickname and avatar edits touch only `{user:<user_id>}` via the lock stack.

**Why `{user:<id>}` uses version-in-value (not version-in-key):** an earlier design used `user:<id>:v<N>` (version-in-key) so the old key became unreachable when `row_version` bumped, without needing a delete to race against a concurrent read. The lock stack design superseded this: the version is now in the **value** (`<row_version>\n<JSON>`), and consistency is maintained by the lock stack's write path (lock → DEL → DB UPDATE → Lua version-compare write-through → unlock) and reader leases (M13). This eliminates the need for the session to carry a `row_version` hint — the reader simply GETs `{user:<id>}` without needing to know the version.

**Cache-key design rationale:**

- **Not per-field caching.** Every hot-path read (login, `/api/me`) needs most fields together — per-field GETs would multiply Redis ops for the same data. Single JSON blob per logical access pattern is faster.
- **Two keys per user, not one — split by *when* the data is needed, not just by size.** `auth:` holds exactly what authenticating requires and nothing a profile edit could ever touch, `{user:}` holds exactly what a page display requires and nothing that must never leave the server.
  - `auth:<username>` — indexed by username, for login only. Contains the password hash; contains nothing else.
  - `{user:<user_id>}` — indexed by user_id, for everything after login. Contains username, nickname, avatar; never the hash. Version embedded in value for the lock stack's compare-and-SET.
- **Split benefits:**
  - Different TTLs (auth: 10 min, user: 1 hr).
  - Clean invalidation: a nickname/avatar edit touches `{user:<user_id>}` only (via lock stack). `auth:<username>` needs no invalidation today.
  - Different index keys naturally — username lookup for login, id lookup for everything after.
- **`sess:<token>`, `lookup:`, and rate-limit keys** are separate concerns (session state, shard routing, counters); no user data leaks in them.

**Why the session record stores only `user_id`:** the session no longer carries `username` (no invalidation path needs it — see §4.1.1) or `row_version` (the lock stack's single-key design means the reader does not need a version hint to form the cache key). The session's job is purely "who is this, and are they still logged in": `{user_id, created_at, last_seen, ip}`.

See §4.4.4 for why `row_version` is a separate counter from the (now-removed) `avatar_version`.

### User table: no vertical split

**Single `users` table, all columns together.** Considered vertical partitioning (splitting hot/cold columns into separate tables) but rejected:
- Row width is ~134 bytes (~130 before `row_version` was added, minus the ~4-byte `avatar_version` `INT` removed in §4.4.4 point 3a) → 10M rows still fits comfortably in typical buffer pool (~1.3-1.4 GB). Neither change moves the needle on this decision.
- The two hot-path queries pull different subsets now (§4.1.1): login (`GetUserByUsername`) reads `id, password_hash` only; `get_user` (`GetUserByID`) reads `id, username, nickname, avatar_suffix, row_version`. Both are still single-row, single-table reads by an indexed column on a specific data shard — the split changed which columns are selected, not how many tables are touched, so it is not an argument for splitting the table itself. `row_version` rides along on the `get_user` query because the lock stack embeds it in the cached value for its version-compare write-through.
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

---

## 9. Workflows

Sequence diagrams for every user-facing operation end-to-end, including the
HTTP + TCP protocol interfaces and failure handling for each.

### How the Lock Stack Manages Cache Consistency

Cache consistency for the `{user:<id>}` key is managed by the lock stack
(`internal/lockstack`), not by version-in-key or manual goroutine DELs. The
session does not carry `row_version` — it only holds `user_id`. See
`lock_stack_implementation_guide.md` for the full design and failure-mode
analysis.

**Write path (nickname update, avatar upload):**
```
1. Acquire lock:  SET {user:<id>}:lock <writer_id> EX <LockTTL> NX
2. Start watchdog (extends lock TTL while writer is alive)
3. Save current cache value (for restoration on DB failure)
4. DEL {user:<id>}  — delete-first ordering (safety net + lease invalidation)
5. DB UPDATE (row_version bumped atomically)
6. Lua versionSetScript: compare-and-SET {user:<id>} with new data (write-through)
7. Release lock (Lua ownership-check DEL)
```

**Read path (get_user):**
```
1. GET {user:<id>} → real data (not L: prefix)? serve immediately (M8)
2. Miss or lease → readCheckScript (atomic: check cache, lock, lease)
   - HIT:     data appeared while waiting → serve
   - LOCKED:  writer in progress → poll with backoff, retry
   - LEASED:  another reader filling → poll with backoff, retry
   - GRANTED: we hold the lease → DB SELECT → leaseSetScript (populate if lease survived)
3. Max wait exceeded → DB SELECT, return without caching
```

---

### 9.1 Signup (`POST /api/signup`)

Validate input, hash password, generate snowflake ID, reserve username on
lookup shard, insert into data shard. No session created — user must login
separately after signup.

#### Diagram

```
Browser              HTTP Server           TCP Server             Redis        Lookup Shard     Data Shard
  │                      │                     │                    │               │               │
  ├─ POST /api/signup ──►│                     │                    │               │               │
  │  body: {username,    │                     │                    │               │               │
  │   password, nickname}│                     │                    │               │               │
  │                      ├─ {op: signup} ─────►│                    │               │               │
  │                      │                     │                    │               │               │
  │                      │                     │  Validate input    │               │               │
  │                      │                     │  bcrypt.Hash       │               │               │
  │                      │                     │  id = snowflake()  │               │               │
  │                      │                     │                    │               │               │
  │                      │                     ├─ INSERT username_lookup ──────────►│               │
  │                      │                     │  (username, id)    │               │               │
  │                      │                     │                    │               │               │
  │              ┌───────┴────────────────┐    │                    │               │               │
  │              │  DUPLICATE USERNAME    │    │                    │               │               │
  │              └───────┬────────────────┘    │◄── Error 1062 ────────────────────┤               │
  │◄─ 409 Conflict ──────┤                     │                    │               │               │
  │                      │                     │                    │               │               │
  │              ┌───────┴────────────────┐    │                    │               │               │
  │              │  USERNAME RESERVED     │    │                    │               │               │
  │              └───────┬────────────────┘    │                    │               │               │
  │                      │                     │  shard = ShardFor(id)              │               │
  │                      │                     ├─ INSERT INTO users (id, username, hash, nick) ───►│
  │                      │                     │                    │               │               │
  │                      │                     │  (on fail: DELETE from lookup shard — rollback)    │
  │                      │                     │                    │               │               │
  │                      │                     ├─[bg] SET lookup:── ►│               │               │
  │                      │                     │  <username> {id}   │               │               │
  │                      │                     │                    │               │               │
  │                      │◄─ {status: ok} ─────┤                    │               │               │
  │◄─ 201 Created ───────┤                     │                    │               │               │
```

#### API Interface

**HTTP request:**
```
POST /api/signup
Content-Type: application/json

{
  "username": "alice",
  "password": "s3cureP@ss",
  "nickname": "Alice"
}
```

**TCP protocol (HTTP server → TCP server):**
```json
{"req_id": 1, "op": "signup", "data": {"username": "alice", "password": "s3cureP@ss", "nickname": "Alice"}}
```
```json
{"req_id": 1, "status": "ok", "data": {"id": 456, "username": "alice", "nickname": "Alice"}}
```

**HTTP responses:**

| HTTP Status | Condition | Body |
|---|---|---|
| 201 | Created successfully | `{"status": "ok", "data": {"id": 456, "username": "alice", "nickname": "Alice"}}` |
| 400 | Validation failed | `{"status": "err", "error": "username must be 3-32 alphanumeric/underscore chars"}` |
| 409 | Username already taken | `{"status": "err", "error": "username already exists"}` |
| 500 | Internal error | `{"status": "err", "error": "internal server error"}` |

**Validation rules:**
- `username`: `^[a-zA-Z0-9_]{3,32}$`, lowercase-normalized
- `password`: 8–72 chars (bcrypt limit), reject all-same-char
- `nickname`: valid UTF-8, 1–32 runes, strip control chars

#### Failure Handling

| Failure | State after | Stale? |
|---|---|---|
| Validation fails (HTTP or TCP) | Nothing touched | No |
| bcrypt hash error (extremely rare) | Nothing touched → 500 | No |
| MySQL INSERT fails — duplicate username (error 1062) | Nothing changed → 409 | No |
| MySQL INSERT fails — other (conn, disk) | Nothing changed → 500 | No |

No cache or file involvement — every failure is a clean no-op.

**Key points:**
- Snowflake ID generated locally before any INSERT (no `AUTO_INCREMENT`).
- Username reserved on lookup shard FIRST — the PK constraint catches global
  duplicates. If the data shard INSERT fails, the lookup reservation is rolled back.
- Lookup cache populated in background goroutine after success.
- bcrypt cost 5, salt built-in. Hashing is the slowest step (~4.5ms).
- `row_version` starts at 0 (DEFAULT), `avatar_suffix` starts NULL.
- No session created: user must POST /api/login separately.

---

### 9.2 Login (`POST /api/login`)

Resolve username via lookup cache / lookup shard, cache-aside read on
`auth:<username>`, bcrypt verify, session creation in Redis.

#### Diagram

```
Browser              HTTP Server           TCP Server             Redis           Lookup Shard     Data Shard
  │                      │                     │                    │               │               │
  ├─ POST /api/login ───►│                     │                    │               │               │
  │  body: {username,    │                     │                    │               │               │
  │         password}    │                     │                    │               │               │
  │                      ├─ {op: login} ──────►│                    │               │               │
  │                      │                     │                    │               │               │
  │                      │                     │  LOOKUP: username → user_id        │               │
  │                      │                     ├─ GET lookup:<user>─►│               │               │
  │                      │                     │  HIT → have user_id│               │               │
  │                      │                     │  MISS → SELECT from├──────────────►│               │
  │                      │                     │    username_lookup  │               │               │
  │                      │                     │    (populate cache) │               │               │
  │                      │                     │                    │               │               │
  │                      │                     │  AUTH: verify password              │               │
  │                      │                     ├─ GET auth:<user> ─►│               │               │
  │                      │                     │                    │               │               │
  │              ┌───────┴────────────────┐    │                    │               │               │
  │              │  AUTH CACHE HIT        │    │                    │               │               │
  │              └───────┬────────────────┘    │                    │               │               │
  │                      │                     │◄─ {id, hash} ──────┤               │               │
  │                      │                     │  bcrypt.Compare    │               │               │
  │                      │                     │                    │               │               │
  │              ┌───────┴────────────────┐    │                    │               │               │
  │              │  AUTH CACHE MISS       │    │  shard = ShardFor(user_id)         │               │
  │              └───────┬────────────────┘    │                    │               │               │
  │                      │                     ├─ SELECT id, password_hash ─────────────────────────►│
  │                      │                     │    FROM users WHERE username = ?    │               │
  │                      │                     │◄── {id, hash} ─────────────────────────────────────┤
  │                      │                     │  bcrypt.Compare    │               │               │
  │                      │                     ├─[bg] SET auth:── ─►│               │               │
  │                      │                     │  <user> {id, hash} │               │               │
  │                      │                     │                    │               │               │
  │              ┌───────┴────────────────┐    │                    │               │               │
  │              │  BOTH PATHS CONVERGE   │    │                    │               │               │
  │              │  (have id, pw verified)│    │                    │               │               │
  │              └───────┬────────────────┘    │                    │               │               │
  │                      │                     │                    │               │               │
  │                      │                     │ Generate session token (256-bit random)             │
  │                      │                     │                    │               │               │
  │                      │                     ├ SETEX sess:<token>►│               │               │
  │                      │                     │  {user_id: 123,    │               │               │
  │                      │                     │   created_at, ip}  │               │               │
  │                      │                     │  EX 86400 (24h)    │               │               │
  │                      │                     │                    │               │               │
  │                      │◄─ {token} ──────────┤                    │               │               │
  │◄─ 200 OK ────────────┤                     │                    │               │               │
  │  Set-Cookie: session │                     │                    │               │               │
  │  =<token>; HttpOnly; │                     │                    │               │               │
  │  SameSite=Lax        │                     │                    │               │               │
```

#### API Interface

**HTTP request:**
```
POST /api/login
Content-Type: application/json

{
  "username": "alice",
  "password": "s3cureP@ss"
}
```

**TCP protocol:**
```json
{"req_id": 2, "op": "login", "data": {"username": "alice", "password": "s3cureP@ss"}}
```
```json
{"req_id": 2, "status": "ok", "data": {"token": "<256-bit-hex>"}}
```

**HTTP responses:**

| HTTP Status | Condition | Headers | Body |
|---|---|---|---|
| 200 | Login successful | `Set-Cookie: session=<token>; HttpOnly; SameSite=Lax; Path=/` | `{"status": "ok"}` |
| 401 | Invalid credentials (wrong password OR unknown username) | — | `{"status": "err", "error": "invalid username or password"}` |
| 400 | Validation failed | — | `{"status": "err", "error": "..."}` |
| 500 | Internal error | — | `{"status": "err", "error": "internal server error"}` |

**Security note:** unknown username returns the same 401 + same response time as
wrong password (dummy bcrypt verify at matching cost) to prevent timing-based
enumeration.

**Logout (`POST /api/logout`):**

```
Browser              HTTP Server           TCP Server             Redis
  │                      │                     │                    │
  ├─ POST /api/logout ──►│                     │                    │
  │  (cookie: session)   │                     │                    │
  │                      ├─ {op: logout} ─────►│                    │
  │                      │                     ├─ DEL sess:<token> ►│
  │                      │                     │◄─ OK ──────────────┤
  │                      │◄─ success ──────────┤                    │
  │◄─ 200 OK ────────────┤                     │                    │
  │  Set-Cookie: session │                     │                    │
  │  =; Max-Age=0        │                     │                    │
```

TCP protocol:
```json
{"req_id": 3, "op": "logout", "data": {"session_token": "<token>"}}
```
```json
{"req_id": 3, "status": "ok"}
```

#### Failure Handling

**Login failures:**

| Failure | What happens | Stale? |
|---|---|---|
| Lookup cache miss + lookup shard SELECT fails | 500 error (can't resolve username → user_id) | No |
| Auth cache GET fails (Redis down) | Fall through to MySQL → slower but correct | No |
| MySQL SELECT fails | 500 error | No |
| bcrypt verify fails (wrong password) | 401, nothing changes | No |
| Session SETEX fails (Redis down) | 500 — can't establish session | No |
| Auth cache SET fails (population goroutine) | Next login will miss again and retry | No |

**Logout failures:**

| Failure | What happens | Stale? |
|---|---|---|
| Session DEL fails (Redis down) | Session lingers until TTL (24h). Client cookie cleared regardless. | Stale session until TTL |

**Key points:**
- Lookup step: 1 Redis GET (`lookup:<username>`) + fallback to lookup shard on miss
- Auth cache hit path: 1 Redis GET (`auth:`) + bcrypt + 1 Redis SETEX (`sess:`)
- Auth cache miss path: 1 Redis GET (miss) + 1 MySQL SELECT (id, hash) on data shard + bcrypt + 1 Redis SET (`auth:`) + 1 Redis SETEX (`sess:`)
- Session stores only `{user_id, created_at, ip, last_seen}` — no `row_version` needed (lock stack uses single key `{user:<id>}`)

---

### 9.3 Profile Update

Covers all profile-related operations: reading profile data, updating nickname,
and uploading avatar. All require an authenticated session.

#### 9.3.1 Profile Display (`GET /api/me`)

Lock stack read path. The session provides `user_id`; the cache key is
`{user:<id>}` (no version hint needed).

**Diagram:**

```
Browser              HTTP Server           TCP Server             Redis                Data Shard
  │                      │                     │                    │                    │
  ├─ GET /api/me ───────►│                     │                    │                    │
  │  (cookie: session)   │                     │                    │                    │
  │                      ├─ {op: get_user} ───►│                    │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET sess:<token> ►│                    │
  │                      │                     │◄─ {user_id: 123}  ─┤                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET {user:123} ──►│                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  CACHE HIT (real data) │    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │◄─ "7\n{username,  ─┤                    │
  │                      │                     │    nickname, ...}" │                    │
  │                      │                     │  decode → serve    │                    │
  │                      │◄─ response ─────────┤                    │                    │
  │◄─ JSON response ─────┤                     │                    │                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  CACHE MISS / LEASE    │    │  lock stack miss path:               │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     ├─ readCheckScript ─►│                    │
  │                      │                     │  (check cache,     │                    │
  │                      │                     │   lock, lease)     │                    │
  │                      │                     │◄─ GRANTED ─────────┤                    │
  │                      │                     │                    │                    │
  │                      │                     │  shard = ShardFor(123)                  │
  │                      │                     ├─ SELECT FROM users WHERE id = 123 ─────►│
  │                      │                     │◄─ {row_version: 7, nickname: "bob",...}─┤
  │                      │                     │                    │                    │
  │                      │                     ├─ leaseSetScript ──►│                    │
  │                      │                     │  (populate only if │                    │
  │                      │                     │   lease survived)  │                    │
  │                      │                     │                    │                    │
  │                      │◄─ response ─────────┤                    │                    │
  │◄─ JSON response ─────┤                     │                    │                    │
```

**API Interface:**

HTTP request:
```
GET /api/me
Cookie: session=<token>
```

TCP protocol:
```json
{"req_id": 4, "op": "get_user", "data": {"session_token": "<token>"}}
```
```json
{"req_id": 4, "status": "ok", "data": {"id": 123, "username": "alice", "nickname": "Alice", "avatar_url": "/uploads/0/123/123_a3f8b91c.jpg"}}
```

HTTP responses:

| HTTP Status | Condition | Body |
|---|---|---|
| 200 | Success | `{"status": "ok", "data": {"id": 123, "username": "alice", "nickname": "Alice", "avatar_url": "..."}}` |
| 401 | No session / expired session | `{"status": "err", "error": "unauthorized"}` |
| 500 | Internal error | `{"status": "err", "error": "internal server error"}` |

**Failure handling:**

| Failure | What happens | Stale? |
|---|---|---|
| Session GET fails (Redis down) | Can't identify user → 401 | No |
| Cache GET fails (Redis down) | Fall through to MySQL → slower but correct | No |
| MySQL SELECT fails | 500 error, cache not populated. Lease released so waiters unblock. | No |
| leaseSetScript fails (Redis error) | Data returned to caller, just not cached. Next read re-leases. | No |

**Key points:**
- Cache hit: 2 Redis ops (session + cache GET), 0 MySQL ops
- Cache miss: session GET + readCheckScript + 1 MySQL SELECT on correct data shard + leaseSetScript
- No session update needed on miss — the lock stack uses a single key `{user:<id>}`, no version hint in the session
- A writer's DEL wipes any outstanding reader lease, preventing slow-reader overwrites (M13)

---

#### 9.3.2 Nickname Update (`POST /api/profile/nickname`)

Lock stack write path: lock → save → DEL → DB UPDATE → write-through → unlock.

**Diagram:**

```
Browser              HTTP Server           TCP Server             Redis                Data Shard
  │                      │                     │                    │                    │
  ├─ POST /api/profile/ ►│                     │                    │                    │
  │  nickname            │                     │                    │                    │
  │ (cookie: session)    │                     │                    │                    │
  │  body: {nickname}    │                     │                    │                    │
  │                      ├─ {op: update_       │                    │                    │
  │                      │   nickname} ───────►│                    │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET sess:<token> ►│                    │
  │                      │                     │◄─ {user_id: 123}  ─┤                    │
  │                      │                     │                    │                    │
  │                      │                     │  LOCK STACK WRITE: │                    │
  │                      │                     ├─ SET {user:123}:lock <wid> NX EX 2s ──►│
  │                      │                     │  (start watchdog)  │                    │
  │                      │                     ├─ GET {user:123} ──►│  (save for restore)│
  │                      │                     ├─ DEL {user:123} ──►│  (delete-first)    │
  │                      │                     │                    │                    │
  │                      │                     │  shard = ShardFor(123)                  │
  │                      │                     ├─ UPDATE users SET nickname = ?, ───────►│
  │                      │                     │    row_version = LAST_INSERT_ID(        │
  │                      │                     │      row_version + 1) WHERE id = 123   │
  │                      │                     │◄── OK + new_row_version ────────────────┤
  │                      │                     │                    │                    │
  │                      │                     ├─ Lua versionSet ──►│  (write-through:   │
  │                      │                     │  SET {user:123}    │   version compare  │
  │                      │                     │  "6\n{...}" EX 1h │   + SET)            │
  │                      │                     │                    │                    │
  │                      │                     ├─ Lua lock release ►│  (ownership check) │
  │                      │                     │  (cancel watchdog) │                    │
  │                      │                     │                    │                    │
  │                      │◄─ success ──────────┤                    │                    │
  │◄─ 200 OK ────────────┤                     │                    │                    │
```

**API Interface:**

HTTP request:
```
POST /api/profile/nickname
Cookie: session=<token>
Content-Type: application/json

{
  "nickname": "NewNick"
}
```

TCP protocol:
```json
{"req_id": 5, "op": "update_nickname", "data": {"session_token": "<token>", "nickname": "NewNick"}}
```
```json
{"req_id": 5, "status": "ok", "data": {"nickname": "NewNick"}}
```

HTTP responses:

| HTTP Status | Condition | Body |
|---|---|---|
| 200 | Updated | `{"status": "ok", "data": {"nickname": "NewNick"}}` |
| 400 | Validation failed (empty, >32 runes, control chars) | `{"status": "err", "error": "nickname must be 1-32 valid unicode characters"}` |
| 401 | No session / expired | `{"status": "err", "error": "unauthorized"}` |
| 500 | Internal error | `{"status": "err", "error": "internal server error"}` |

**Failure handling:**

| Failure | State after | Stale? |
|---|---|---|
| Lock acquisition fails (Redis down) | Degrade to Simple: DB UPDATE → best-effort DEL `{user:123}`. Correct, brief stale window. | ~ms window, self-heals |
| MySQL UPDATE fails (inside lock) | Cache restored from saved value if available. Lock released. | No |
| MySQL succeeds, write-through fails | Cache empty (DEL already ran). Readers fall through to DB — correct. Lock released. | No (readers hit DB) |
| Writer crash after DB commit | Lock expires after LockTTL. Readers wait, then lease and cache fresh data. | Blocked up to LockTTL |

**Key points:**
- DB update is atomic: `row_version = LAST_INSERT_ID(row_version + 1)` bumps
  version and returns it in one round trip via `SELECT LAST_INSERT_ID()`
- Cache invalidated inside the lock (DEL before DB UPDATE — delete-first ordering)
- Write-through populates cache before unlock — readers after unlock get a cache hit, no DB stampede
- No session update needed — session does not carry `row_version`
- `auth:<username>` is NOT touched — it holds nothing a nickname edit could make stale
- Watchdog extends lock TTL while the writer is alive; if the writer crashes, the lock expires and readers unblock

---

#### 9.3.3 Avatar Upload (`POST /api/profile/avatar`)

File-first, then DB, then invalidate. The file must exist before the DB
knows about it.

**Diagram:**

```
Browser              HTTP Server           TCP Server             Redis           MySQL         Filesystem
  │                      │                     │                    │                │               │
  ├─ POST /api/profile/ ►│                     │                    │                │               │
  │  avatar              │                     │                    │                │               │
  │  (multipart file)    │                     │                    │                │               │
  │                      ├─ {op: update_       │                    │                │               │
  │                      │   avatar,           │                    │                │               │
  │                      │   file_data} ──────►│                    │                │               │
  │                      │                     │                    │                │               │
  │                      │                     ├─ GET sess:<token> ►│                │               │
  │                      │                     │◄─ {user_id: 123,  ─┤                │               │
  │                      │                     │    row_version: 5} │                │               │
  │                      │                     │                    │                │               │
  │                      │                     │  1. Generate suffix (crypto/rand)   │               │
  │                      │                     │     suffix = "a3f8b91c"             │               │
  │                      │                     │                    │                │               │
  │                      │                     │  2. MkdirAll /uploads/0/123/ ──────────────────────►│
  │                      │                     │                    │                │               │
  │                      │                     │  3. Write file to  │                │               │
  │                      │                     │     123_a3f8b91c.jpg.tmp ──────────────────────────►│
  │                      │                     │                    │                │               │
  │                      │                     │  4. os.Rename(tmp, final) ─────────────────────────►│
  │                      │                     │     (atomic on POSIX)               │               │
  │                      │                     │                    │                │               │
  │                      │                     │  [file exists on disk. DB doesn't know yet.]        │
  │                      │                     │                    │                │               │
  │                      │                     │  5. UPDATE users SET                │               │
  │                      │                     │       avatar_suffix = 'a3f8b91c',   │               │
  │                      │                     │       row_version = LAST_INSERT_ID( │               │
  │                      │                     ├──────  row_version + 1)  ──────────────────────────►│
  │                      │                     │     WHERE id = 123                  │               │
  │                      │                     │◄── OK ──────────────────────────────────────────────┤
  │                      │                     │                    │                │               │
  │                      │                     ├─ SELECT LAST_INSERT_ID() ──────────►│               │
  │                      │                     │◄── 6 ───────────────────────────────┤               │
  │                      │                     │                    │                │               │
  │                      │                     │  LOCK STACK WRITE (same as nickname update):           │
  │                      │                     ├─ lock → save → DEL {user:123} → DB UPDATE ─────────►│
  │                      │                     │◄── OK + new_row_version ────────────────────────────┤
  │                      │                     ├─ Lua versionSet write-through → unlock              │
  │                      │                     │                    │                │               │
  │                      │◄─ success ──────────┤                    │                │               │
  │◄─ 200 OK ────────────┤                     │                    │                │               │
```

**API Interface:**

HTTP request:
```
POST /api/profile/avatar
Cookie: session=<token>
Content-Type: multipart/form-data

(file field: "avatar", max 2 MB, jpeg/png/webp only)
```

TCP protocol:
```json
{"req_id": 6, "op": "update_avatar", "data": {"session_token": "<token>", "file_data": "<base64>", "content_type": "image/jpeg"}}
```
```json
{"req_id": 6, "status": "ok", "data": {"avatar_url": "/uploads/0/123/123_a3f8b91c.jpg"}}
```

HTTP responses:

| HTTP Status | Condition | Body |
|---|---|---|
| 200 | Upload successful | `{"status": "ok", "data": {"avatar_url": "/uploads/0/123/123_a3f8b91c.jpg"}}` |
| 400 | File too large (>2 MB) | `{"status": "err", "error": "file exceeds 2 MB limit"}` |
| 400 | Invalid file type (MIME sniff fails allowlist) | `{"status": "err", "error": "only jpeg, png, and webp images are allowed"}` |
| 401 | No session / expired | `{"status": "err", "error": "unauthorized"}` |
| 500 | Internal error | `{"status": "err", "error": "internal server error"}` |

**Upload validation (in order):**
1. Size check — reject > 2 MB before reading full body
2. Sniff first 512 bytes with `http.DetectContentType`
3. MIME allowlist: `image/jpeg`, `image/png`, `image/webp`
4. Fresh filename — never trust user's filename

**Failure handling:**

| Failure | State after | Stale? |
|---|---|---|
| File write fails (disk full, permissions) | Nothing in MySQL changed. .tmp file cleaned up. | No |
| Rename fails | Same as above. | No |
| MySQL UPDATE fails after file written (inside lock) | Orphaned file on disk. Cache restored from saved value. Lock released. | No |
| MySQL succeeds, write-through fails | Cache empty (DEL already ran). Readers hit DB. Lock released. | No (readers hit DB) |

**Key points:**
- Steps 1–4: no database involved. If anything fails, nothing in MySQL changed.
- Step 5: DB only updated AFTER file exists. Row can never point at a file
  that doesn't exist.
- Step 6: cache invalidation uses the lock stack write path (same as nickname update).
  The DEL inside the lock also wipes any outstanding reader lease.
- Each upload writes a different file (new random suffix) — no FS race on
  concurrent uploads by the same user.
- Old avatar files become orphans. Async sweeper (not yet implemented) deletes
  files not matching current `avatar_suffix`, with 10-minute mtime grace period.
- `auth:<username>` is NOT touched — same reasoning as nickname update.

**The ordering is designed so that every failure leaves the system in a
consistent or self-healing state.** File before DB means the row never points
at a missing file. The lock stack's delete-first + write-through means the worst
case after a DB success is an empty cache (readers hit DB), not stale data.

---

## 10. DB Schema Design

**Per-data-shard: users table** (applied to `data_0`, `data_1`, etc.):

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,              -- snowflake ID, app-generated
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

**Per-lookup-shard: username_lookup table** (applied to `lookup_0`, `lookup_1`):

```sql
CREATE TABLE username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

| Column | Type | Purpose | Notes |
|---|---|---|---|
| id | BIGINT UNSIGNED | Primary key | Snowflake ID (app-generated, see §13.3). |
| username | VARCHAR(32) | Login identifier | Immutable after signup. Global uniqueness enforced by `username_lookup` on lookup shards. |
| password_hash | VARBINARY(60) | bcrypt output | Cost 5, salt built-in. Fixed 60-byte output. |
| nickname | VARCHAR(128) | Display name | UTF-8, up to 32 runes (× 4 bytes max per rune). |
| avatar_suffix | CHAR(8) | Random hex suffix | NULL = no avatar (serve default.png). Non-NULL = file at derived path. |
| row_version | BIGINT UNSIGNED | Cache version | Bumps on ANY update (nickname or avatar). Embedded in `{user:<id>}` cache **value** for the lock stack's version-compare write-through, never in filenames. |
| created_at | TIMESTAMP | Signup time | |
| updated_at | TIMESTAMP | Last modification | AUTO-updated by MySQL `ON UPDATE CURRENT_TIMESTAMP`. |

**Design rationale:**

- **Snowflake IDs** generated by the application (see §13.3) — no `AUTO_INCREMENT`, no coordination between shards.
- **`utf8mb4` charset** for full unicode support (emojis in nicknames).
- **`utf8mb4_bin` collation** → case-sensitive username matching.
- **Avatar path is derived, not stored:** `/uploads/<id & 0x1FFF>/<id>/<id>_<avatar_suffix>.jpg`.
  Only the 8-byte suffix is in the row — path is constructed server-side.
  At 10M rows, this saves ~400 MB–1 GB vs storing full paths.
- **No `avatar_version` column.** An earlier iteration had one; it was removed
  because an atomic MySQL-generated version forced the DB write to happen before
  the file existed, which could leave a row pointing at a missing file. The
  random suffix needs no MySQL round trip to generate, enabling file-first
  ordering (see §9.3.3).
- **`row_version` is separate from the avatar scheme.** It exists for the lock
  stack's version-compare write-through (§4.1). Bumped on every mutating write
  via `row_version = LAST_INSERT_ID(row_version + 1)` — returns new version in
  one round trip.
- **Single table, no vertical split.** Row width ~134 bytes → 10M rows ≈
  1.3 GB, fits comfortably in InnoDB buffer pool. Query-level column selection
  handles per-request data minimization.

---

## 11. Cache Schema Design

### Redis Keys

| Key | Value | TTL | Purpose |
|---|---|---|---|
| `sess:<token>` | `{user_id, created_at, ip, last_seen}` | 24h sliding | Session identity. Token is 256-bit random. |
| `auth:<username>` | `{id, password_hash}` | 10 min | Login auth cache. NOT versioned. |
| `{user:<user_id>}` | `<row_version>\n<JSON>` | 1 hr | Profile display cache. Version-in-value, managed by lock stack. May also hold a lease (`L:<token>`) during the miss path. |
| `{user:<user_id>}:lock` | `<writer_id>` | LockTTL (2s) | Lock stack write lock. Same hash tag as the cache key for Cluster compat. |
| `lookup:<username>` | `{user_id}` | 10 min | Username → user_id mapping cache (sharding). Immutable once written. |

### Design Rationale

**Two caches split by lifecycle, not one wide record per user:**

- `auth:<username>` — needed *before* a session exists (login). Contains only
  what password verification requires (`id`, `password_hash`). Indexed by
  username because that is the login credential.
- `{user:<user_id>}` — needed *after* a session exists (profile display).
  Contains only what a page needs to render (`username`, `nickname`,
  `avatar_suffix`), with `row_version` embedded in the value for the lock
  stack's version-compare write-through. Indexed by user_id because
  authenticated requests carry user_id in the session.
- Neither holds a field the other needs. No overlap.

**Why `auth:` is unversioned:** `auth:` holds only `id` and `password_hash`,
both immutable in the current feature set. It needs no versioning and no
invalidation beyond its own TTL.

**Why `{user:}` uses version-in-value (not version-in-key):**

An earlier design used `user:<id>:v<N>` (version-in-key) so the old key became
unreachable on bump without needing a delete to race against concurrent reads.
The lock stack design superseded this: the version is now in the **value**
(`<row_version>\n<JSON>`), and consistency is maintained by the lock stack's
write path (lock → DEL → write-through) and reader leases (M13). This
eliminates the need for the session to carry a `row_version` hint — the reader
simply GETs `{user:<id>}` without knowing the version. The `{…}` hash tag
ensures the cache key and its lock key land on the same Redis Cluster slot.

**Invalidation strategy — lock stack (supersedes delete-on-write):**

| Action | Keys invalidated | Mechanism |
|---|---|---|
| Nickname update | `{user:<user_id>}` | Lock stack write path (lock → DEL → DB UPDATE → write-through → unlock) |
| Avatar update | `{user:<user_id>}` | Lock stack write path (same) |
| Signup | None | — |
| Logout | `sess:<token>` | Plain DEL |
| Password change (future) | `auth:<username>` + all sessions | — |

`auth:<username>` needs **no invalidation** today — it holds only `id` and
`password_hash`, both immutable in the current feature set. Nickname and avatar
edits touch only the `{user:}` cache via the lock stack.

**Why the session does NOT carry `row_version`:** the lock stack uses a single
key `{user:<id>}` — the reader does not need to know any version to form the
cache key. The version is inside the value, checked only by the lock stack's Lua
scripts during write-through and decode. This simplified both the session model
(just `{user_id, created_at, last_seen, ip}`) and the login path (no extra
MySQL SELECT for `row_version` on an auth-cache hit).

---

## 12. Critical Test Cases

Important functional tests for each workflow. Each case lists the scenario,
the action, and the expected outcome.

---

### 12.1 Signup

| # | Scenario | Action | Expected |
|---|---|---|---|
| S1 | New user, valid input | POST `/api/signup` `{"username": "alice", "password": "secure99", "nickname": "Alice"}` | 201 Created. Snowflake ID generated. `username_lookup` row on lookup shard. `users` row on data shard with `row_version=0`, `avatar_suffix=NULL`. `lookup:<username>` populated in Redis (goroutine). No session created. |
| S2 | Duplicate username | POST `/api/signup` with a username that already exists | 409 Conflict. Lookup shard catches via PK on `username_lookup` (error 1062). No data shard insert attempted. |
| S3 | Username too short | `{"username": "ab", ...}` (2 chars, min is 3) | 400. No DB write. |
| S4 | Username with invalid chars | `{"username": "alice!@#", ...}` | 400. Regex `^[a-zA-Z0-9_]{3,32}$` rejects. |
| S5 | Password too short | `{"password": "1234567"}` (7 chars, min is 8) | 400. No DB write. |
| S6 | Password too long | 73+ char password (bcrypt limit is 72) | 400. No DB write. |
| S7 | Password all same char | `{"password": "aaaaaaaa"}` | 400. Rejected by all-same-char rule. |
| S8 | Nickname empty | `{"nickname": ""}` | 400. Min 1 rune. |
| S9 | Nickname too long | 33+ unicode runes | 400. Max 32 runes. |
| S10 | Nickname with emoji | `{"nickname": "Alice 🎉"}` | 201. `utf8mb4` charset supports emoji. Rune count includes emoji (1 rune each). |
| S11 | Signup then verify login required | Signup, then immediately GET `/api/me` without logging in | GET `/api/me` returns 401. Signup does not create a session. |

---

### 12.2 Login

| # | Scenario | Action | Expected |
|---|---|---|---|
| L1 | Valid credentials, auth cache miss | Login with correct username + password, `auth:<username>` not in Redis | 200. `Set-Cookie: session=<token>`. Username resolved via lookup cache/shard. MySQL queried for `id, password_hash` on data shard. `auth:<username>` populated in Redis (goroutine). `sess:<token>` created with `{user_id, created_at, ip, last_seen}`. |
| L2 | Valid credentials, auth cache hit | Login again (or with warm cache) | 200. Username resolved via lookup cache. `auth:<username>` served from Redis (no MySQL query for hash). Session created — no `row_version` fetch needed. |
| L3 | Wrong password, user exists | Login with correct username, wrong password | 401 `"invalid username or password"`. No session created. `auth:` cache unchanged. |
| L4 | Unknown username | Login with username that doesn't exist | 401 `"invalid username or password"` — same error message as L3. Dummy bcrypt verify executed at same cost so response time ≈ L3 (no timing-based enumeration). |
| L5 | Timing equivalence (L3 vs L4) | Measure response times for L3 and L4 across multiple requests | Response times should be statistically indistinguishable (both dominated by bcrypt cost 5 ≈ 4.5ms). |
| L6 | Session contains only user_id | Login, then inspect `sess:<token>` in Redis | Contains `user_id`, `created_at`, `last_seen`, `ip`. No `row_version` (lock stack uses single key, no version hint needed). |
| L7 | Multiple logins, separate sessions | Login twice from different contexts | Two distinct `sess:` keys in Redis, each valid. Both carry the same `user_id`. |
| L8 | Login after profile edit | Edit nickname (bumps `row_version` to N+1), then login again | Session created normally — `row_version` is not in the session. The lock stack manages cache consistency independently. |

---

### 12.3 Logout

| # | Scenario | Action | Expected |
|---|---|---|---|
| O1 | Valid session | POST `/api/logout` with valid session cookie | 200. `sess:<token>` deleted from Redis. Response includes `Set-Cookie: session=; Max-Age=0` (clears browser cookie). |
| O2 | No session cookie | POST `/api/logout` without cookie | 401. |
| O3 | Expired / invalid session | POST `/api/logout` with a token not in Redis | 401. Nothing deleted. |
| O4 | Logout then access protected endpoint | Logout, then GET `/api/me` with same cookie | 401. Session no longer exists in Redis. |
| O5 | Logout one session, other survives | Login twice (sessions A, B). Logout session A. | Session B still valid. GET `/api/me` with B returns 200. |

---

### 12.4 Profile Display (`GET /api/me`)

| # | Scenario | Action | Expected |
|---|---|---|---|
| P1 | Cache hit | GET `/api/me` with session, `{user:<id>}` has real data in Redis | 200 with `{id, username, nickname, avatar_url}`. 0 MySQL queries. 2 Redis ops (session + cache GET). |
| P2 | Cache miss, first read | GET `/api/me` with session, `{user:<id>}` not populated | 200 with data from MySQL via lock stack miss path (readCheck → GRANTED → DB SELECT → leaseSet). |
| P3 | Stale data prevented by lock stack | A writer DELs the cache mid-read; a slow reader's lease is wiped | Slow reader's leaseSetScript sees its lease is gone → populate rejected. Data still returned to caller from DB. Next reader re-leases and caches fresh data. |
| P4 | No session | GET `/api/me` without cookie | 401. |
| P5 | Expired session | GET `/api/me` with token not in Redis (TTL expired) | 401. |
| P6 | Default avatar | GET `/api/me` for user with `avatar_suffix = NULL` | `avatar_url` points to `/default.png` (shared static asset), not a per-user path. |
| P7 | Custom avatar | GET `/api/me` for user with `avatar_suffix = "a3f8b91c"` | `avatar_url` = `/uploads/<id & 0x1FFF>/<id>/<id>_a3f8b91c.jpg`. URL is publicly accessible. |

---

### 12.5 Nickname Update

| # | Scenario | Action | Expected |
|---|---|---|---|
| N1 | Valid update | POST `/api/profile/nickname` `{"nickname": "NewNick"}` with valid session | 200. Lock stack write path: lock → DEL `{user:<id>}` → MySQL UPDATE (row_version N→N+1) → write-through `{user:<id>}` with new data → unlock. |
| N2 | Verify cache populated after update | After N1, check Redis for `{user:<id>}` | Key exists with new data (write-through populated it before unlock). |
| N3 | Verify next read after update | After N1, GET `/api/me` | Cache hit on `{user:<id>}` — write-through already populated it. No MySQL query needed. |
| N4 | Empty nickname | `{"nickname": ""}` | 400. Min 1 rune. No DB change. |
| N5 | Nickname with unicode | `{"nickname": "日本語テスト"}` (5 runes) | 200. UTF-8 stored correctly. Rune count validated (not byte count). |
| N6 | Nickname with control chars | `{"nickname": "test\x00\x01"}` | 400. Control characters stripped/rejected. |
| N7 | No session | POST without cookie | 401. No DB change. |
| N8 | `auth:` cache untouched | After N1, check `auth:<username>` in Redis | `auth:` key unchanged (if it existed) — nickname edit uses lock stack on `{user:<id>}` only, does not touch `auth:`. |

---

### 12.6 Avatar Upload

| # | Scenario | Action | Expected |
|---|---|---|---|
| A1 | Valid JPEG upload | POST multipart with valid JPEG ≤2 MB | 200. File written to `/uploads/<bucket>/<id>/<id>_<suffix>.jpg`. MySQL `avatar_suffix` updated, `row_version` bumped. Old `user:` cache deleted. `avatar_url` in response points to new file. |
| A2 | Valid PNG upload | POST with valid PNG ≤2 MB | 200. Same flow as A1. MIME sniff detects `image/png`. |
| A3 | Valid WebP upload | POST with valid WebP ≤2 MB | 200. Same flow. |
| A4 | File too large | POST with 3 MB image | 400 `"file exceeds 2 MB limit"`. Rejected before full body read. No file on disk, no DB change. |
| A5 | Invalid file type — wrong extension | POST with `.jpg` extension but actual content is a PDF | 400 `"only jpeg, png, and webp images are allowed"`. MIME sniff detects actual content type, not extension. |
| A6 | Invalid file type — executable disguised | POST with binary executable renamed to `.png` | 400. Magic bytes don't match PNG/JPEG/WebP. |
| A7 | No session | POST without cookie | 401. No file written, no DB change. |
| A8 | File on disk before DB | Verify ordering during upload | File exists at `<path>.jpg` BEFORE MySQL UPDATE runs. If UPDATE fails, file is an orphan (harmless), row is unchanged. |
| A9 | Verify old file becomes orphan | Upload new avatar for user who already has one | Old file still on disk (not deleted inline). New file at different path (new suffix). DB points to new suffix. Old file cleaned by sweeper (when implemented). |
| A10 | Avatar URL publicly accessible | After A1, access the `avatar_url` without any session cookie | 200 — file served. `Content-Type: image/jpeg`. `X-Content-Type-Options: nosniff` header present. No auth required. |
| A11 | Avatar URL not enumerable | Try to guess avatar URL for another user by iterating suffixes | 8-char hex suffix = 32 bits of randomness ≈ 4 billion possibilities. Iteration infeasible. |
| A12 | Directory sharding correct | After upload for user_id=12345, check filesystem | File at `/uploads/<12345 & 0x1FFF>/<12345>/<12345>_<suffix>.jpg` = `/uploads/4153/12345/12345_<suffix>.jpg`. |

---

### 12.7 Cache Behavior

| # | Scenario | Action | Expected |
|---|---|---|---|
| C1 | Auth cache TTL | Populate `auth:<username>`, wait >10 minutes | Key expired. Next login falls through to MySQL. |
| C2 | User cache TTL | Populate `{user:<id>}`, wait >1 hour | Key expired. Next GET `/api/me` triggers lock stack miss path (readCheck → lease → DB → leaseSet). |
| C3 | Session sliding window | Active session — make an authenticated request past the halfway mark | Session TTL refreshed to 24h. Requests in the first half do NOT refresh (reduces Redis writes). |
| C4 | Session TTL expiry | Session idle for >24h | `sess:<token>` expired. Next request returns 401. |
| C5 | Lock stack version-in-value consistency | Writer updates `{user:123}` from v5→v6 via lock stack | Writer's DEL wipes any outstanding reader lease. Writer's versionSetScript write-through replaces with `"6\n{...}"`. A slow reader holding a lease from before the DEL has its leaseSetScript rejected (lease token gone). No stale data cached. |
| C6 | Redis down — login still works | Simulate Redis unavailable | Lookup and auth cache misses → fall through to MySQL. Session creation fails → 500 (can't establish session). Login degrades gracefully for cache but requires Redis for session. |
| C7 | Redis down — profile write | Simulate Redis unavailable during nickname update | Lock stack degrades to Simple: DB UPDATE → best-effort DEL `{user:<id>}`. Correct, brief stale window until TTL or next successful write-through. |
| C8 | Redis down — profile read | Simulate Redis unavailable during GET `/api/me` | Session GET fails → 401 (can't identify user). Cannot degrade — session is identity. |

---

### 12.8 Security

| # | Scenario | Action | Expected |
|---|---|---|---|
| X1 | Timing-safe login rejection | Compare response times: wrong password (user exists) vs unknown username | Both ≈ bcrypt cost 5 time (~4.5ms). No statistically significant difference (dummy verify on unknown username). |
| X2 | Uniform error messages | Wrong password vs unknown username | Both return identical 401 body: `"invalid username or password"`. No "user not found" vs "wrong password" distinction. |
| X3 | Session cookie attributes | Inspect `Set-Cookie` header after login | `HttpOnly` (JS cannot read), `SameSite=Lax` (cross-site POST won't include it), `Path=/`. |
| X4 | SQL injection attempt | Signup/login with `username = "'; DROP TABLE users;--"` | 400 (fails regex validation) or harmless (parameterized query). No SQL executed. |
| X5 | Avatar serves with nosniff | GET an uploaded avatar | Response includes `X-Content-Type-Options: nosniff`. Browser will NOT sniff content type — prevents XSS via disguised uploads. |
| X6 | Password not in any cache | After login, inspect `{user:<id>}` and `sess:<token>` | Neither contains `password_hash`. Only `auth:<username>` holds it, and that cache is keyed by username, TTL'd at 10 min, and holds nothing else. |

---

## 13. Horizontal Sharding Design (Future Scale)

Implemented for this entry task — 2 lookup shards and 2 data shards (scalable to
3+) on localhost, all as separate MySQL schemas on the same mysqld instance. This
section documents the full sharding design, both what is implemented and the
production-scale extensions it is built to support.

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

**Consistent hashing for shard assignment:**

Nodes (shards) and keys (user_ids) live on a circular hash ring (0 to 2³²-1). A
user_id hashes to a position and walks clockwise to the first shard it hits.

```
        Shard 0 (pos 100)
       /
  0 ──●────────────●── Shard 1 (pos 400)
  │      Ring           │
  │                     │
  └────●────────────────┘
     Shard 2 (pos 700)

user_id 42 hashes to 250 → walks clockwise → Shard 1
user_id 99 hashes to 500 → walks clockwise → Shard 2
```

**Adding a shard:** say Shard 3 joins at position 550. Only keys between Shard 3
and its predecessor (Shard 1) move — they come from one neighbor (Shard 2), not
from all shards. ~1/N of keys move, not all of them.

**Virtual nodes (vnodes):** each physical shard gets 100-200 positions on the ring
to even out arc lengths. Without them, 3 physical shards could have wildly uneven
key distributions.

**Why consistent hashing over simple modulo:**
- `user_id % N` reshuffles nearly all keys when N changes (adding one shard
  moves ~(N-1)/N of all users). Consistent hashing moves ~1/N.
- Consistent hashing allows adding/removing shards with minimal data migration.

---

### 13.3 ID Generation — Snowflake IDs

**Problem:** `AUTO_INCREMENT` generates IDs on one MySQL instance. With multiple
shards, two instances would produce colliding IDs.

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

**Properties:**
- **No central counter.** Each TCP server has a unique `machine_id` (assigned at
  startup via config). It generates IDs independently.
- **No collisions.** Two servers at the same millisecond produce different IDs
  because their machine bits differ. Same server, same millisecond, different
  sequence bits.
- **Time-sorted.** High bits are the timestamp, so IDs are roughly chronological.
  New inserts cluster near the rightmost B-tree leaf — same locality benefit as
  `AUTO_INCREMENT`. This is the key advantage over random UUIDs, which scatter
  inserts across the entire index causing page splits and poor buffer pool hit
  rates.
- **Fits in `BIGINT UNSIGNED`.** No schema type change from the current `id`
  column — just remove `AUTO_INCREMENT` and supply the ID from Go.
- **Shard-independent.** The ID encodes no shard information. `ShardFor(id)` is
  a separate function (consistent hash), so resharding does not require
  re-issuing IDs.

**Go implementation** (no bitfield syntax — explicit shifts):
```go
id := (timestamp << 22) | (machineID << 12) | sequence

// Reading back:
timestamp := id >> 22
machineID := (id >> 12) & 0x3FF   // 10-bit mask
sequence  := id & 0xFFF            // 12-bit mask
```

**Throughput:** 4,096 IDs/ms/machine = 4,096,000 IDs/sec/machine. If a machine
exhausts 4096 in one millisecond (extremely unlikely for user signups), it waits
for the next millisecond — a sub-millisecond stall.

---

### 13.4 Username Lookup — Global Uniqueness

**Problem:** with the `users` table sharded by `user_id`, the `UNIQUE KEY
uk_username` only enforces uniqueness within one shard. Two shards can each
accept an `INSERT` for the same username.

**Decision: MySQL table as source of truth on a separate, dedicated instance,
with a Redis cache (`lookup:<username>`) on top.** Maps `username → user_id`
and is the single source of truth for "does this username exist globally."

**Why a MySQL table (not purely Redis):** Redis is volatile — if it dies or
flushes, a pure-Redis lookup loses all mappings and every login falls through
to nowhere (the per-shard `users` tables have no global username index).
Rebuilding from shards requires scanning all of them — slow for 10M rows. A
MySQL table is durable; the Redis cache on top is a performance layer, same
defense-in-depth as the existing `auth:<username>` cache over the `users` table.

**Why a separate instance (not co-located on a data shard):** placing the lookup
on shard 0 would mean shard 0 handles its normal 1/N share of user data
queries PLUS 100% of all login lookups. At 3k–6k login RPS the volume is
real, and that shard always runs hotter than the others through no fault of
its own data distribution. A separate instance keeps every data shard's load
symmetric, and isolates lookup availability from data shard availability —
if the lookup instance has a problem, only signup and login are affected;
post-login operations keep working because they route by `user_id` from the
session and never touch the lookup.

```sql
CREATE TABLE username_lookup (
  username  VARCHAR(32) NOT NULL PRIMARY KEY,
  user_id   BIGINT UNSIGNED NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```
- One row ≈ 40 bytes. 10M users ≈ 400 MB — fits entirely in memory.
- Primary key lookup = O(log n), effectively O(1) in buffer pool.

**Redis cache on top** — cache-aside, same pattern as `auth:<username>`:

| Key | Value | TTL |
|---|---|---|
| `lookup:<username>` | JSON `{user_id}` | 10 min |

- Populated on login cache miss (after the MySQL lookup query).
- SET on signup (the entry is being created, not changed).
- Data is immutable once written (a username maps to one user_id forever), so
  there is no staleness concern and no need for versioning or invalidation.

**Scaling the lookup itself** (if ever needed):

The lookup is write-once-read-many (written on signup, read on login). A single
instance handles the load with enormous margin at current scale. When it does
need to scale, it shards by `hash(username)` — matching its own access pattern
(`WHERE username = ?`). This is independent of the data shards which use
`hash(user_id)`. Each store is sharded by the key it is actually queried on.
No cross-shard fan-out on either side: a login lookup hits exactly one lookup
shard, then exactly one data shard. Lookup shards still enforce global
uniqueness because each username hashes to exactly one lookup shard, and that
shard's PK constraint catches duplicates — two concurrent signups for "alice"
both hash to the same lookup shard and the same PK, so one wins and the other
gets a duplicate key error.

**The lookup is only touched on signup and login.** All post-login operations
(get_user, update_nickname, update_avatar) route by `user_id` from the session
and skip the lookup entirely.

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

#### Login (sharded)

```
TCP server (any, via round-robin):

  1. username_lookup.GET("alice") → user_id 12345
     → not found? → dummy bcrypt verify, return 401

  2. shard = ShardFor(12345) → shard 1

  3. shard_1_pool.GetUserByUsername("alice")
     → {id, password_hash}

  4. bcrypt.Compare, create session {user_id, created_at, ip, last_seen}
```

One extra lookup (step 1) compared to the current unsharded flow. Cost: one
primary-key read on a table that fits in memory — negligible next to bcrypt.

#### Post-Login (get_user, update_nickname, update_avatar — sharded)

```
TCP server (any):

  1. resolveSession(token) → {user_id}
     (from Redis — no lookup needed, no row_version in session)

  2. shard = ShardFor(user_id)

  3. shard_N_pool.query(...)
     (identical to current single-instance queries)
```

The username lookup is not involved. The session carries `user_id`, and
`ShardFor(user_id)` is a pure function (consistent hash) — no network call.

---

### 13.6 Scaling Model — TCP Servers and DB Shards Are Independent

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

**Why they decouple (Option A — every TCP server connects to every shard):**
- A hot shard does NOT mean a hot TCP server. bcrypt load distributes across all
  TCP servers regardless of which shard the user lives on.
- Adding a TCP server (for CPU) requires zero changes to the DB layer.
- Adding a DB shard (for storage/IOPS) requires updating the consistent hash ring
  and migrating ~1/N of users — no TCP server changes beyond adding one more pool
  to each server's pool list.

**The rejected alternative (Option B — TCP server paired 1:1 with its DB shard):**
- Fewer connections, but a hot shard = a hot TCP server (coupled bottleneck).
- Cannot add TCP capacity without adding DB capacity.
- Failover is harder: if TCP-1 (owning shard 1) dies, shard 1 is unreachable.
  A "fallback" TCP server for shard 1 needs its own pool to shard 1's DB — which
  is just Option A with extra steps.
- Requires the HTTP server or a gateway to become an application-aware router
  (parse request bodies, resolve sessions, compute shard) instead of a dumb
  round-robin LB.

**Connection count concern (Option A's cost):**
- 3 TCP servers × (3 data shards × 10 conns + 1 lookup × 5 conns) = 105 total
  connections across all MySQL instances. Each data shard sees 30 connections
  (3 × 10); the lookup instance sees 15 (3 × 5) — all well within MySQL's
  default `max_connections` of 151 per instance. The extra connections from
  Option A are negligible compared to the operational complexity of building an
  application-aware routing proxy.

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
- `InsertUser`: no longer reads `LastInsertId()` — the app generates the
  snowflake ID before insert and passes it in.
- `db.Handle` becomes a shard-aware type: a slice of pools + a `ShardFor(userID)`
  router that picks which pool to use per query.
- `lookupUserForLogin`: hits the username lookup first to get `(user_id)`, then
  routes to the correct shard.
- `handleSignup`: generates snowflake ID, inserts into lookup (catches global
  duplicates), then inserts into the shard.

**What stays unchanged:**
- `row_version`, `avatar_suffix`, the `LAST_INSERT_ID(row_version+1)` trick —
  all operate on a single row by ID on one shard.
- Cache keys (`auth:<username>`, `{user:<id>}`, `sess:<token>`, `lookup:<username>`) — keyed by
  username or user_id, not by shard. Redis doesn't care which MySQL instance the
  data came from.
- Lock stack write path (lock → DEL → write-through → unlock), session model
  (`{user_id, created_at, last_seen, ip}`), file-first avatar ordering — all unchanged.
- Avatar filesystem sharding (`user_id & 0x1FFF`) — orthogonal to DB sharding.

---

### 13.8 Availability vs. Sharding (Separate Concerns)

**Status: shelved — considered and documented but not implemented.** Same
rationale as rate limiting (§4.8): the current design accepts replication
cleanly (pool retry handles failover, the lock stack's version-in-value handles
cache staleness, GTID "read your own writes" is a routing-layer addition),
and building replication infrastructure for a localhost entry task with one
MySQL instance is out of scope. Nothing below requires an overhaul to implement
later.

Sharding solves **load distribution**, not **availability**. They are independent
axes that compose:

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
| Primary down | MySQL replication + orchestrator | DNS/VIP flips to replica; pool reconnects on next `Acquire` (existing `isBadConn` retry handles this) |
| Adding a shard | App code (update consistent hash ring) + migration | Add a new pool entry, migrate ~1/N of users |
| Read scaling | Read replicas per shard | Optional read-only pool per shard; route SELECTs there, UPDATEs to primary |

**Replication modes and staleness:**

| Mode | Data loss on failover | Read-from-replica staleness |
|---|---|---|
| Async (MySQL default) | Possible (unreplicated writes lost) | Unbounded (seconds of lag possible) |
| Semi-sync | None (≥1 replica ACKs before commit) | Still present (ACK = received, not applied) |
| Group Replication (Paxos-based) | None (majority consensus) | Still present on secondaries (log replay lag) |

**Replication guarantees consistency of the _log_, not zero-staleness of reads
from followers.** For zero-stale reads: read from the primary. For "read your own
writes" without always hitting the primary: track the GTID (transaction ID) from
the write and pass it to the replica read with
`WAIT_FOR_EXECUTED_GTID_SET(gtid, timeout)` — the replica either catches up
(typically ms) or times out and the read falls back to the primary.

For this system, the lock stack (version-in-value + leases) already handles
the staleness problem at the Redis layer, which is where the hot-path reads
actually go. DB-level read staleness only matters on cache misses, where the
lock stack's lease mechanism ensures only fresh data is cached.

---

### 13.9 Rate Limiting in the Sharded Context

**Status: shelved** — follows the same design and rationale as §4.8. Sharding
introduces no additional rate-limiting concerns: the plumbing points (Redis
counters checked before bcrypt in the login handler) are the same whether there
is one data shard or many, since rate-limit keys (`rl:login:ip:<ip>`,
`rl:login:user:<username>`) are in the shared Redis instance and are not
shard-specific.

---

### 13.10 Implementation Scope for This Entry Task

The sharding design above is implemented for this entry task:

**Lookup shards:** 2 shards (`lookup_0`, `lookup_1`), routed by
`hash(username) % 2`. Global uniqueness enforced per-shard via PK on
`username_lookup`. Redis `lookup:<username>` cache on top.

**Data shards:** 2 shards (`data_0`, `data_1`), routed by consistent hashing
(150 vnodes per shard). Scalable to 3+ via the dual-write migration phases
described in §13.8 of `sharding_implementation.md`.

**Localhost setup:** all shards are separate MySQL schemas (databases) on
separate mysqld instances (ports 3211, 3212 — see `sharding_implementation.md`
§IMPLEMENTATION/SETUP). The Go code connects to each via its own DSN in
`DATA_SHARD_DSNS` / `LOOKUP_SHARD_DSNS` — the routing, sharding, and migration
logic is identical whether pools point at different ports or different database
names on the same port.

**Lock stack:** implemented (`internal/lockstack`), managing the `{user:<id>}`
cache with version-in-value, reader leases, writer locks with watchdog, and
Lua scripts for atomic operations. See `lock_stack_implementation_guide.md`.

**ID generation:** snowflake IDs via `internal/idgen`, configured by `MACHINE_ID`.

---

## 14. Configuration & Environment Variables

All configuration is loaded from `.env` (with real env vars taking precedence)
by `internal/config/config.go`. See `.env.example` for defaults.

### HTTP Server

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `HTTP_TCP_POOL_SIZE` | `8` | Persistent TCP connections to the TCP server |
| `WEB_ROOT` | `web` | Static file root for HTML/JS |
| `HTTP_READ_HEADER_TIMEOUT_SECONDS` | `5` | |
| `HTTP_READ_TIMEOUT_SECONDS` | `15` | |
| `HTTP_WRITE_TIMEOUT_SECONDS` | `15` | |
| `HTTP_IDLE_TIMEOUT_SECONDS` | `60` | |

### TCP Server

| Variable | Default | Purpose |
|---|---|---|
| `TCP_LISTEN` | `:9000` | TCP listen address |

### MySQL Sharding

| Variable | Default | Purpose |
|---|---|---|
| `DATA_SHARD_DSNS` | (see `.env.example`) | Comma-separated DSNs for data shards (`data_0`, `data_1`, …) |
| `LOOKUP_SHARD_DSNS` | (see `.env.example`) | Comma-separated DSNs for lookup shards (`lookup_0`, `lookup_1`) |
| `SHARD_VNODES` | `150` | Virtual nodes per shard on the consistent hash ring |
| `MYSQL_DATA_MAX_OPEN` | `10` | Max open connections per data shard pool |
| `MYSQL_DATA_MAX_WAIT_MS` | `200` | Max wait for a connection from a data shard pool |
| `MYSQL_DATA_CONN_MAX_LIFETIME_SECONDS` | `300` | Max lifetime of a connection in a data shard pool |
| `MYSQL_DATA_MAX_IDLE_TIME_SECONDS` | `120` | Max idle time before a connection is swept |
| `MYSQL_LOOKUP_MAX_OPEN` | `5` | Max open connections per lookup shard pool |
| `MYSQL_LOOKUP_MAX_WAIT_MS` | `200` | Max wait for a connection from a lookup shard pool |

**Note:** `.env.example` uses `SHARD_DATA_MAX_OPEN` / `SHARD_LOOKUP_MAX_OPEN`,
but `config.go` reads `MYSQL_DATA_MAX_OPEN` / `MYSQL_LOOKUP_MAX_OPEN`. The
actual `.env` must use the names that `config.go` reads.

### ID Generation

| Variable | Default | Purpose |
|---|---|---|
| `MACHINE_ID` | `0` | Snowflake ID machine bits (0–1023). Must be unique per TCP server instance. |

### Redis

| Variable | Default | Purpose |
|---|---|---|
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis address |
| `REDIS_PASSWORD` | (empty) | Redis password |
| `REDIS_DB` | `0` | Redis database number |
| `REDIS_POOL_SIZE` | `50` | `go-redis` pool size |
| `REDIS_MIN_IDLE_CONNS` | `10` | Minimum idle connections kept warm |
| `REDIS_CONN_MAX_LIFETIME_SECONDS` | `1800` | Max connection age |
| `REDIS_POOL_TIMEOUT_MS` | `200` | Max wait for a pooled connection |
| `REDIS_IO_TIMEOUT_MS` | `500` | Read/write timeout |
| `REDIS_PING_TIMEOUT_SECONDS` | `2` | Health-check ping timeout |
| `CACHE_LAYER` | `on` | Set to `off` to disable user/profile cache (sessions still cached) |
| `CACHE_AUTH_TTL_SECONDS` | `600` | `auth:<username>` TTL (10 min) |
| `CACHE_USER_TTL_SECONDS` | `3600` | `{user:<id>}` TTL (1 hr) — also used as the lock stack's CacheTTL default |
| `CACHE_LOOKUP_TTL_SECONDS` | `600` | `lookup:<username>` TTL (10 min) |

### Lock Stack

| Variable | Default | Purpose |
|---|---|---|
| `LOCKSTACK_LOCK_TTL_MS` | `2000` | Writer lock TTL. Watchdog extends while alive. |
| `LOCKSTACK_LEASE_TTL_MS` | `500` | Reader lease TTL. Must be > DB query timeout. |
| `LOCKSTACK_CACHE_TTL_SECONDS` | `3600` | Cache entry TTL (set on write-through and lease-populate). |
| `LOCKSTACK_POLL_MIN_US` | `500` | Initial backoff for LOCKED/LEASED polling (0.5ms). |
| `LOCKSTACK_POLL_MAX_US` | `4000` | Max backoff cap (4ms). |
| `LOCKSTACK_MAX_LOCK_RETRIES` | `200` | Max retries to acquire the write lock before giving up. |

### Uploads

| Variable | Default | Purpose |
|---|---|---|
| `UPLOADS_ROOT` | `uploads` | Filesystem root for avatar uploads |
| `UPLOADS_MAX_BYTES` | `2097152` | Max upload size (2 MB) |

### Bcrypt

| Variable | Default | Purpose |
|---|---|---|
| `BCRYPT_COST` | `5` | bcrypt cost factor. Must match whatever cost the seeder wrote (see §4.3). |

### Sessions

| Variable | Default | Purpose |
|---|---|---|
| `SESSION_TTL_SECONDS` | `86400` | Session TTL (24h). Sliding window refreshes when remaining < half. |