# Workflow & Schema Design — DB + Cache

Sequence diagrams for the read and write (update) paths involving the TCP
server, MySQL, and Redis cache. Covers the cache-aside pattern with
delete-on-write invalidation and version-in-key.

---

## 1. DB Schema

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
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

| Column | Purpose | Notes |
|---|---|---|
| id | Primary key | AUTO_INCREMENT BIGINT |
| username | Login identifier | Immutable after signup. Unique index for O(log n) lookup. |
| password_hash | bcrypt output | VARBINARY(60), cost 5, salted |
| nickname | Display name | UTF-8, up to 32 runes |
| avatar_suffix | Random 8-char hex | NULL = no avatar (serve default.png). Non-NULL = file exists at derived path. |
| row_version | Cache key version | Bumps on ANY update (nickname or avatar). Used only for Redis cache key, never in filenames. |
| created_at | Signup timestamp | |
| updated_at | Last modification | AUTO-updated by MySQL |

---

## 2. Cache Schema (Redis)

| Key | Value | TTL | Purpose |
|---|---|---|---|
| `sess:<token>` | `{user_id, row_version, created_at, ip, last_seen}` | 24h sliding | Session. Token is 256-bit random. `row_version` seeds the user cache key. |
| `auth:<username>` | `{id, password_hash}` | 10 min | Login auth cache. NOT versioned. Holds only immutable fields (no invalidation needed). |
| `user:<user_id>:v<row_version>` | `{username, nickname, avatar_suffix}` | 1 hr | Profile display cache. Version-in-key = old version instantly unreachable on bump. |

**Key design decisions:**

- `auth:` and `user:` are split by lifecycle: `auth:` is needed before a
  session exists (login), `user:` is needed after (profile display). Neither
  holds a field the other needs.
- `auth:` is unversioned because login has no prior source for the version
  (no session exists yet). `row_version` is fetched fresh from MySQL on every
  login instead.
- `user:` uses version-in-key so old entries become unreachable by construction
  — no race between delete and concurrent read.
- `sess:` carries `row_version` as a hint for the next `user:` cache lookup,
  at near-zero marginal cost (session is read on every authenticated request
  anyway).

---

## 3. Login Path (`POST /api/login`)

Cache-aside read on `auth:<username>`, bcrypt verify, fresh `row_version`
fetch from MySQL, session creation in Redis.

```
Browser              HTTP Server           TCP Server             Redis                MySQL
  │                      │                     │                    │                    │
  ├─ POST /api/login ───►│                     │                    │                    │
  │  body: {username,    │                     │                    │                    │
  │         password}    │                     │                    │                    │
  │                      ├─ {op: login} ──────►│                    │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET auth:<user> ─►│                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  AUTH CACHE HIT        │    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │◄─ {id: 123,  ──────┤                    │
  │                      │                     │   password_hash}   │                    │
  │                      │                     │                    │                    │
  │                      │                     │  bcrypt.Compare(password, hash)         │
  │                      │                     │  → match? continue. no match? → 401.    │
  │                      │                     │                    │                    │
  │                      │                     │  (need row_version for session,         │
  │                      │                     │   not in auth: cache — fetch fresh)     │
  │                      │                     │                    │                    │
  │                      │                     ├─ SELECT row_version FROM users ────────►│
  │                      │                     │    WHERE id = 123                       │
  │                      │                     │◄── row_version: 5 ──────────────────────┤
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  AUTH CACHE MISS       │    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │◄─ nil ─────────────┤                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ SELECT id, password_hash, ────────────►│
  │                      │                     │    row_version                          │
  │                      │                     │    FROM users WHERE username = ?        │
  │                      │                     │◄── {id: 123, password_hash, ────────────┤
  │                      │                     │     row_version: 5}                     │
  │                      │                     │                    │                    │
  │                      │                     │  bcrypt.Compare(password, hash)         │
  │                      │                     │  → match? continue. no match? → 401.    │
  │                      │                     │                    │                    │
  │                      │                     ├─[goroutine] ───────┤                    │
  │                      │                     │  SET auth:<user> ─►│                    │
  │                      │                     │ {id, password_hash}│                    │
  │                      │                     │  EX 600 (10 min)   │                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  BOTH PATHS CONVERGE   │    │                    │                    │
  │              │  (have id, row_version)│    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │                    │                    │
  │                      │                     │ Generate session token (256-bit random) │
  │                      │                     │                    │                    │
  │                      │                     ├ SETEX sess:<token>►│                    │
  │                      │                     │  {user_id: 123,    │                    │
  │                      │                     │   row_version: 5,  │                    │
  │                      │                     │   created_at, ip}  │                    │
  │                      │                     │  EX 86400 (24h)    │                    │
  │                      │                     │                    │                    │
  │                      │◄─ {token} ──────────┤                    │                    │
  │◄─ 200 OK ────────────┤                     │                    │                    │
  │  Set-Cookie: session │                     │                    │                    │
  │  =<token>; HttpOnly; │                     │                    │                    │
  │  SameSite=Lax        │                     │                    │                    │
```

**Key points:**
- `auth:<username>` holds only `{id, password_hash}` — no `row_version`
- Cache hit path: 1 Redis GET (auth cache) + 1 MySQL SELECT (row_version only)
  + bcrypt verify + 1 Redis SETEX (session). The MySQL read is the deliberate
  cost of not caching `row_version` in `auth:` — see §4.1.1 in system_design.md.
- Cache miss path: 1 Redis GET (miss) + 1 MySQL SELECT (full: id, hash,
  row_version in one query) + bcrypt verify + 1 Redis SET (populate auth cache)
  + 1 Redis SETEX (session). The miss query returns `row_version` in the same
  round trip — no extra MySQL read needed.
- `row_version` is stored in the session so every subsequent authenticated
  request can form the correct `user:<id>:v<N>` cache key without another
  MySQL lookup.
- Unknown username: dummy bcrypt verify at same cost to prevent timing-based
  enumeration.

---

## 4. Read Path — Profile Display (`GET /api/me`)

Cache-aside read with version-in-key. The session provides the version hint.

```
Browser              HTTP Server           TCP Server             Redis                MySQL
  │                      │                     │                    │                    │
  ├─ GET /api/me ───────►│                     │                    │                    │
  │  (cookie: sess token)│                     │                    │                    │
  │                      ├─ {op: get_user} ───►│                    │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET sess:<token> ►│                    │
  │                      │                     │◄─ {user_id: 123,  ─┤                    │
  │                      │                     │    row_version: 5} │                    │
  │                      │                     │                    │                    │
  │                      │                     │  (form key: user:123:v5)                │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET user:123:v5 ─►│                    │
  │                      │                     │                    │                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  CACHE HIT             │    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │◄─ {username, ──────┤                    │
  │                      │                     │    nickname,       │                    │
  │                      │                     │    avatar_suffix}  │                    │
  │                      │                     │                    │                    │
  │                      │◄─ response ─────────┤                    │                    │
  │◄─ JSON response ─────┤                     │                    │                    │
  │                      │                     │                    │                    │
  │              ┌───────┴────────────────┐    │                    │                    │
  │              │  CACHE MISS            │    │                    │                    │
  │              └───────┬────────────────┘    │                    │                    │
  │                      │                     │◄─ nil ─────────────┤                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ SELECT id, username, nickname, ───────►│
  │                      │                     │    avatar_suffix, row_version           │
  │                      │                     │    FROM users WHERE id = 123            │
  │                      │                     │◄─{row_version: 7, nickname: "bob", ...}─┤
  │                      │                     │                    │                    │
  │                      │                     │  (version changed: 5 → 7)               │
  │                      │                     │                    │                    │
  │                      │                     ├─[goroutine]────────┤                    │
  │                      │                     │  SET user:123:v7 ─►│                    │
  │                      │                     │  {username,        │                    │
  │                      │                     │   nickname,        │                    │
  │                      │                     │   avatar_suffix}   │                    │
  │                      │                     │  EX 3600           │                    │
  │                      │                     │                    │                    │
  │                      │                     │  UPDATE sess:<token>                    │
  │                      │                     │  row_version = 7 ─►│                    │
  │                      │                     │                    │                    │
  │                      │◄─ response ─────────┤                    │                    │
  │◄─ JSON response ─────┤                     │                    │                    │
```

**Key points:**
- Session provides `row_version` → reader knows which cache key to GET
- Cache hit: 2 Redis ops (session + cache), 0 MySQL ops
- Cache miss: 2 Redis ops + 1 MySQL SELECT, then populates cache + updates
  session hint in background goroutine
- If DB returns a different `row_version` than session expected (edits happened
  mid-session), reader uses the DB's version and updates the session hint

---

## 5. Write Path — Nickname Update (`POST /api/profile/nickname`)

DB-first, then delete-on-write invalidation. Version-in-key means the old
cache entry becomes unreachable once `row_version` bumps.

```
Browser              HTTP Server           TCP Server             Redis                MySQL
  │                      │                     │                    │                    │
  ├─ POST /api/profile/ ►│                     │                    │                    │
  │  nickname            │                     │                    │                    │
  │ (cookie: sess token) │                     │                    │                    │
  │  body: {nickname}    │                     │                    │                    │
  │                      ├─ {op: update_       │                    │                    │
  │                      │   nickname} ───────►│                    │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ GET sess:<token> ►│                    │
  │                      │                     │◄─ {user_id: 123,  ─┤                    │
  │                      │                     │   row_version: 5}  │                    │
  │                      │                     │                    │                    │
  │                      │                     │ (old_version = 5)  │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─ UPDATE users SET nickname = ?, ───────►│
  │                      │                     │    row_version = LAST_INSERT_ID(        │
  │                      │                     │      row_version + 1)                   │
  │                      │                     │    WHERE id = 123                       │
  │                      │                     │◄── OK (affected: 1) ────────────────────┤
  │                      │                     │                    │                    │
  │                      │                     ├─ SELECT LAST_INSERT_ID() ──────────────►│
  │                      │                     │◄── 6 (new_row_version) ─────────────────┤
  │                      │                     │                    │                    │
  │                      │                     │ (new_version = 6)  │                    │
  │                      │                     │                    │                    │
  │                      │                     ├─[goroutine] ───────┤                    │
  │                      │                     │  DEL user:123:v5 ─►│                    │
  │                      │                     │◄─ OK ──────────────┤                    │
  │                      │                     │                    │                    │
  │                      │                     │ UPDATE sess:<token>│                    │
  │                      │                     │  row_version = 6 ─►│                    │
  │                      │                     │                    │                    │
  │                      │◄─ success ──────────┤                    │                    │
  │◄─ 200 OK ────────────┤                     │                    │                    │
```

**Key points:**
- DB update is atomic: `row_version = LAST_INSERT_ID(row_version + 1)` bumps
  version and returns it in one round trip
- Old cache key (`user:123:v5`) is deleted in a background goroutine
  (delete-on-write, M10)
- Session hint updated to new version (6) so the next read forms the correct
  cache key
- If DEL fails (Redis network error): old key lingers until TTL, but any
  reader who learns v6 (from session) will ask for `user:123:v6`, miss, go
  to DB. Self-healing.
- `auth:<username>` is NOT touched — it holds nothing a nickname edit could
  make stale

**Small staleness window (P1):** between the DB commit and the DEL, a reader
whose session still says v5 could GET `user:123:v5` and get old data. This
window is ~0.1-0.5ms (one Redis round trip). Self-heals once DEL completes.

---

## 6. Write Path — Avatar Upload (`POST /api/profile/avatar`)

File-first, then DB, then invalidate. The file must exist before the DB
knows about it.

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
  │                      │                     ├─[goroutine] ───────┤                │               │
  │                      │                     │  DEL user:123:v5 ─►│                │               │
  │                      │                     │                    │                │               │
  │                      │                     │ UPDATE sess:<token>│                │               │
  │                      │                     │  row_version = 6 ─►│                │               │
  │                      │                     │                    │                │               │
  │                      │◄─ success ──────────┤                    │                │               │
  │◄─ 200 OK ────────────┤                     │                    │                │               │
```

**Key points:**
- Steps 1-4: no database involved. If anything fails, nothing in MySQL changed.
  Orphaned .tmp file cleaned up by OS or sweeper.
- Step 5: DB only updated AFTER file exists. Row can never point at a file
  that doesn't exist.
- If step 5 fails: file is an orphan (harmless, sweeper cleans up), row is
  unchanged, client told upload failed. Correct.
- Old avatar files become orphans. Async sweeper (not yet implemented) deletes
  files not matching current `avatar_suffix`, with 10-minute mtime grace period
  to avoid deleting mid-upload files.

---

## 7. Failure Handling Summary

### Read path failures

| Failure | What happens | Stale? |
|---|---|---|
| Session GET fails (Redis down) | Can't identify user → 401 | No |
| Cache GET fails (Redis down) | Fall through to MySQL → slower but correct | No |
| MySQL SELECT fails | 500 error, cache not populated | No |
| Cache SET fails (population) | Next read will miss again and retry | No |

### Write path failures (nickname)

| Failure | State after | Stale? |
|---|---|---|
| MySQL UPDATE fails | Nothing changed. Cache still correct. | No |
| MySQL succeeds, DEL fails | DB=v6, cache has v5 entry. Stale until TTL. Readers with v6 session miss old key → DB → correct. | ms window (P1) |
| MySQL succeeds, session update fails | Session says v5, DB says v6. Next read: GET v5 → miss (deleted) → DB → learn v6 → update session. | Self-heals on next read |

### Write path failures (avatar)

| Failure | State after | Stale? |
|---|---|---|
| File write fails (disk full) | Nothing in MySQL changed. .tmp file cleaned up. | No |
| Rename fails | Same as above. | No |
| MySQL UPDATE fails after file written | Orphaned file on disk. Row unchanged. Client told failure. Sweeper cleans file. | No |
| MySQL succeeds, DEL fails | Same as nickname: stale until TTL, self-healing. | ms window (P1) |

**The ordering is designed so that every failure leaves the system in a
consistent or self-healing state.** File before DB means the row never points
at a missing file. DB before cache-delete means the worst case is a briefly
stale cache entry, not missing data.

---

## 8. Version Flow — How `row_version` Travels

```
                    ┌──────────┐
                    │  MySQL   │  Source of truth for row_version
                    │  users   │  Bumped atomically on every UPDATE
                    └────┬─────┘
                         │
           ┌─────────────┼─────────────┐
           │             │             │
           ▼             ▼             ▼
    ┌──────────┐  ┌────────────┐  ┌──────────────────────┐
    │  Login   │  │  Session   │  │ Cache key            │
    │  handler │  │  sess:<t>  │  │ user:<id>:v<version> │
    │          │  │  {row_ver} │  │                      │
    │ reads    │  │            │  │  populated on miss,  │
    │ fresh    │  │  carries   │  │  deleted on write    │
    │ from DB  │  │  the hint  │  │                      │
    └──────────┘  └─────┬──────┘  └──────────────────────┘
                        │
                        │ session provides row_version
                        │ to every authenticated request
                        ▼
                  ┌────────────┐
                  │  Reader    │
                  │  forms key │
                  │  user:<id> │
                  │  :v<N>     │
                  └────────────┘
```

1. **Login:** `row_version` fetched fresh from MySQL (not cached, not guessed).
   Stored in session.
2. **Every authenticated request:** session provides `row_version` → reader
   forms cache key `user:<id>:v<N>`.
3. **On write:** MySQL bumps `row_version` atomically. Old cache key deleted.
   Session updated with new version.
4. **On cache miss (version mismatch):** reader goes to DB, learns current
   `row_version`, updates session hint, populates cache at correct version.

The version never needs to be compared or validated on reads — the key itself
IS the version check. If the key exists, it was populated for that version.
If it doesn't exist, it's a miss, and the reader goes to DB.
