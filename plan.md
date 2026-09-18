# Project Plan — Entry Task (2 Weeks)

Rough daily plan for the user management system entry task. Adjust as reality diverges. Update the "Status" column and daily-update log as you go.

**Rule of thumb split:** ~20% design → ~60% build → ~20% perf/docs.

---

## Daily-Update Template

Keep updates to 3 lines:

> **Did:** what was completed today
> **Doing:** what's next
> **Blockers:** open questions or waiting-on-someone items

---

## Week 1 — Design + Foundation

| Day | Focus | Deliverable | Status |
|---|---|---|---|
| 1 | Understand & design | `system_design.md` draft; TBDs identified | ⬜ |
| 2 | Lock decisions + scaffold | Repo layout, dev env (MySQL+Redis), README skeleton | ⬜ |
| 3 | DB schema + seeder | Users table + 10M rows seeded and verified | ⬜ |
| 4 | Custom connection pool | Pool with prepared-stmt cache + unit tests | ⬜ |
| 5 | HTTP↔TCP protocol + skeleton | Frame format, persistent conns, end-to-end ping op | ⬜ |

### Day 1 — Understand & Design
- Re-read task carefully; list all requirements
- Draft `system_design.md` (options → considerations → decisions)
- Identify all TBDs, ask questions if any are blocking
- **Update:** *"Drafted system design doc covering architecture, data model, API, security, perf plan. X open questions raised."*

### Day 2 — Lock in Decisions + Repo Scaffold
- Resolve TBDs (language, framework, session TTL, etc.)
- Set up repo: Go modules, directory layout (`cmd/http-server`, `cmd/tcp-server`, `internal/...`, `scripts/seed`, `perf/`)
- Local dev env: MySQL + Redis running (docker-compose or install scripts)
- Basic `README.md` with setup instructions
- **Update:** *"Design decisions finalized. Repo scaffolded with X packages. Local dev environment running. Ready to code."*

### Day 3 — DB Schema + Seeder
- Write MySQL schema (users table, indexes, charset/collation)
- Write seeder: 10M rows (batched, `LOAD DATA INFILE` or bulk `INSERT`)
- Run it and verify — this takes longer than expected (disk, index build)
- Sanity-check lookup perf with a few sample queries
- **Update:** *"DB schema created. Seeded 10M user rows in ~X min. Verified index performance."*

### Day 4 — Custom Connection Pool
- Implement pool: acquire/release, size limits, wait queue with timeout, health checks
- Per-connection prepared statement cache
- Unit tests: concurrent acquire, wait timeout, shutdown drain, health eviction
- **Update:** *"Custom MySQL connection pool implemented with prepared statement caching. Unit tests passing."*

### Day 5 — HTTP↔TCP Protocol + Skeleton Servers
- Define frame format (length-prefixed, JSON payload; `req_id` for multiplexing)
- TCP server: accept connections, parse frames, dispatch by op
- HTTP server: pool of persistent conns to TCP server, multiplex, correlate responses
- One end-to-end "ping" op working
- **Update:** *"HTTP↔TCP protocol implemented. End-to-end ping working. Ready to implement business logic."*

---

## Week 2 — Features + Perf + Docs

| Day | Focus | Deliverable | Status |
|---|---|---|---|
| 6 | Signup + Login | Working signup/login with bcrypt + Redis session | ⬜ |
| 7 | Profile view + edit | Full functional requirements met (rate limit + CSRF included) | ⬜ |
| 8 | Cache layer + tests | Cache-aside with invalidation; integration tests | ⬜ |
| 9 | Perf baseline (no cache) | 3k RPS target hit; pprof + `perf` captured | ⬜ |
| 10 | Perf tuning + cache test | 6k RPS target hit; stretch attempt | ⬜ |
| 11 | Buffer / stretch / cleanup | Optional stretch (10k/20k); code cleanup | ⬜ |
| 12 | Technical Design Document | Confluence TDD complete | ⬜ |
| 13 | Test cases + polish | Test design doc; README polished | ⬜ |
| 14 | Buffer / review prep | Self-review; demo prep | ⬜ |

### Day 6 — Signup + Login
- Signup: input validation, bcrypt hash, insert
- Login: verify hash, create Redis session, set HttpOnly + SameSite cookie
- Redis session store
- Basic HTML pages: signup form, login form, minimal styling
- **Update:** *"Signup and login working end-to-end. Sessions persisted in Redis."*

### Day 7 — Profile View + Edit
- `/api/user` returns current user
- Update nickname (UTF-8 rune-count validation)
- Upload avatar (size cap, MIME sniff, filesystem write, path in DB)
- Serve avatar
- Rate limiting on login (Redis counters, per-IP + per-user)
- CSRF on profile edits
- **Update:** *"Profile view and edit complete. Rate limiting and CSRF in place. All functional requirements met."*

### Day 8 — Cache Layer + Tests
- Cache-aside on user lookup for login
- Invalidation on profile edit (delete-on-write)
- Consider negative caching for enumeration protection
- Integration tests: signup, login, profile edit, cache invalidation
- **Update:** *"Cache-aside layer added. Integration tests cover full flows."*

### Day 9 — Perf Baseline (No Cache)
- Set up load testing (`wrk` / `vegeta` / `hey`) with ≥200 unique users
- Run 3k RPS target without cache
- Capture: pprof CPU + heap, `perf stat` (IPC, cache misses, branch mispredicts), flame graph
- Tune pool size, timeouts, GC settings
- **Update:** *"No-cache baseline: hit X RPS with p99=Yms. Bottleneck identified as [bcrypt / SQL / etc]."*

### Day 10 — Perf Tuning + Cache Test
- Address top bottleneck from Day 9
- Run 6k RPS target with cache warm
- Profile again, iterate
- Attempt stretch (10k / 20k) if time permits
- **Update:** *"With cache: X RPS with p99=Yms. Profiling shows [next bottleneck]. Attempted stretch: got to Z RPS."*

### Day 11 — Buffer / Cleanup / Stretch
- Fix anything broken
- Attempt any remaining stretch goals
- Code cleanup, add missing tests, remove dead code
- **Update:** *"Cleanup and stretch work done. Final RPS: [without cache: X, with cache: Y]."*

### Day 12 — Technical Design Document (Confluence)
- Port `system_design.md` to Confluence
- Sections required by task:
  - Background
  - Requirements
  - Workflows / System Design (with diagrams)
  - Interface Design (APIs)
  - Critical Test Design
  - Project Repository and Installation
  - Performance Test Result
- Add final numbers, sequence diagrams, screenshots
- **Update:** *"TDD drafted on Confluence covering all required sections."*

### Day 13 — Test Cases + Polish
- Formalize "Critical Test Design" section: functional, edge, error, perf tests
- Polish README with clear install/run instructions
- Verify a fresh clone works end-to-end
- **Update:** *"Test design documented. README finalized. Project ready for review."*

### Day 14 — Buffer / Review Prep
- Buffer day (something always overruns)
- Self-review: read the doc + code as if you were the reviewer
- Prepare walkthrough / demo
- **Update:** *"Final review pass complete. Ready to walk through."*

---

## Daily-Update Log

Fill in each day. Keep it terse.

### Day 1 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 2 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 3 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 4 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 5 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 6 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 7 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 8 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 9 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 10 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 11 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 12 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 13 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

### Day 14 — _(date)_
- **Did:**
- **Doing:**
- **Blockers:**

---

## Rules of Thumb

- ~20% design / ~60% build / ~20% perf+docs
- Days 1-2 should be **almost entirely design + planning + setup** — this is what's being evaluated
- By Day 3 you should have written code (the seeder)
- Never skip the design phase to code faster — reviewers will notice
- Mirror `system_design.md` to the TDD throughout, don't wait until Day 12
- Perf work always takes longer than expected — reserve real days
- Leave Day 14 as buffer — it will save you