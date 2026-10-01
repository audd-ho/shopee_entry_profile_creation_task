# Session Handover

## What was done this session

### 1. Updated `system_design_present.md` with new sections

Added the following to match the reviewer's required outline:

- **§9 Workflows** — sequence diagrams, TCP protocol messages, HTTP request/response
  formats, and failure handling tables for:
  - 9.1 Signup (new diagram, no cache involved)
  - 9.2 Login + Logout (auth cache hit/miss paths)
  - 9.3 Profile Update:
    - 9.3.1 Profile Display (`GET /api/me`)
    - 9.3.2 Nickname Update
    - 9.3.3 Avatar Upload
  - Includes `row_version` flow overview at the top
- **§10 DB Schema Design** — DDL, column table, design rationale
- **§11 Cache Schema Design** — Redis key table, split rationale, invalidation table
- **§12 Critical Test Cases** — 50+ test cases across:
  - Signup (S1–S11), Login (L1–L8), Logout (O1–O5)
  - Profile Display (P1–P7), Nickname Update (N1–N8), Avatar Upload (A1–A12)
  - Cache Behavior (C1–C7), Security (X1–X6)

### 2. Tightened reasoning/wording per reviewer feedback

Reviewer feedback addressed:
> "Need to have clear reasoning on why such approach being chosen. Any assumptions
> applied should be reasonable and clearly explained as well. Can be more careful
> during explanation in both docs and verbal wordings. e.g. Simplicity may
> interpreted differently in Implementation and Design context."

Changes made throughout `system_design_present.md`:

- **Replaced all vague "simpler/simplicity"** with specific design qualities:
  - "No sync problem" (delete removes the entry — nothing left to disagree with DB)
  - "Fewer failure modes" (DEL has no data payload to get wrong)
  - "Fewer concerns" (no prepared stmts, no transactions)
  - "Fewer operations" (INCR + EXPIRE)
- **Added missing assumptions** where decisions were stated without justification:
  - Delete-on-write: ms stale acceptable because profile updates are infrequent
    per-user — same user must update twice within ~0.1-0.5ms to observe stale
  - bcrypt cost 5: acceptable for localhost threat model, can be raised
    retroactively (bcrypt embeds cost in hash, old hashes still verify at new cost)
  - Negative caching deferred: enumeration already mitigated by dummy bcrypt verify
  - Per-row versioning: only 2 mutable fields, both cached together, both
    infrequent — per-column versioning overhead not justified
- **Added CAS/lease comparison** to §4.1: explained what it is (version counter
  key + cached data key, INCR → DB → SET, compare on read), why it eliminates
  stale entirely, and why delete-on-write is the better fit for this workload
- **Cleaned up informal notes:** removed `[i think]`, `[on review now...]`,
  `[believed most appropriate?]` — replaced with confident, specific reasoning
- **Fixed inconsistencies:** bcrypt decision now says "cost 5" (not "cost 10"
  contradicted by status), split into "Why bcrypt" and "Why cost 5" sub-sections
- **Added retroactive upgradeability** to bcrypt reasoning: raise cost later, old
  hashes still verify, rehash on next login — no migration, no downtime
- **Tightened §4.2** session sub-decisions: sliding window justified (active user
  shouldn't be logged out mid-edit), ip/last_seen purposes stated
- **Tightened §4.5** framework: removed hedging, stated concrete reasons (not
  bottleneck, no external deps, 7 routes makes boilerplate acceptable)
- **Tightened §4.6** pool: task interpretation stated with confidence
- **Tightened §4.7** protocol: confirmed perf findings, deferred health-check
  with reason

### 3. Reference files used (not edited)

- `notes/workflow_schema_design.md` — source for sequence diagrams
- `notes/db_cache_design_present.md` — source for CAS/lease comparison and
  the "why delete-on-write is enough" reasoning
- `system_design.md` — the full working design doc (present version is the
  reviewer-facing subset)

---

## What remains — next session

### Code Organization and Patterns

The third reviewer feedback point:
> "Put attention to Code Organization and Patterns."

This has not been addressed yet. The current code structure is:

```
cmd/
  http-server/main.go
  tcp-server/main.go
  seed/main.go
internal/
  cache/          — Redis cache (invalidate, session, user)
  config/         — config loading
  crypto/         — bcrypt hashing, random generation
  db/             — custom MySQL pool (conn, pool, users)
  httpserver/     — HTTP handlers, middleware, routes, session, TCP client
  model/          — User, Session structs
  protocol/       — TCP frame encoding, op definitions
  tcpserver/      — TCP server, dispatch, auth/profile handlers
```

Things to review/discuss for code organization:
- Whether the package boundaries are clean (e.g. `httpserver/tcpclient.go` —
  should the TCP client pool be its own package?)
- Handler patterns (are auth/profile handlers structured consistently?)
- Error handling patterns across packages
- Dependency injection / how services are wired together in main
- Test organization and coverage
- Any patterns the reviewer expects to see (middleware chain, request context,
  structured logging, etc.)
