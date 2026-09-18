# Entry Profile Creation — User Management System

Entry-task project: user signup, login, and profile edit with a two-process architecture (HTTP frontend + TCP backend), MySQL, Redis, and custom connection pooling.

## Documentation

- **`system_design.md`** — architecture, design decisions, trade-offs (living doc)
- **`plan.md`** — 2-week daily plan
- **`SETUP.md`** — setup & run instructions
- **`schema.sql`** — MySQL DDL for the users table
- **`web/README.md`** — frontend info

## Layout

```
cmd/         entry-point binaries (http-server, tcp-server, seed)
internal/    private packages (protocol, httpserver, tcpserver, db, cache, model, validate, crypto, config)
web/         static frontend (HTML/CSS/JS)
scripts/     setup/seed/perf shell scripts
deploy/      docker-compose for local MySQL + Redis
uploads/     runtime avatar storage (gitignored)
perf/        perf test artifacts (gitignored raw profiles)
```

See `system_design.md` §4 and the file layout in the same doc for full details.

## Quick start

```bash
# 1. Start MySQL + Redis
./scripts/setup.sh

# 2. Seed 10M users (long-running)
./scripts/seed.sh

# 3. Start backend (in one terminal)
go run ./cmd/tcp-server

# 4. Start frontend (in another terminal)
go run ./cmd/http-server

# 5. Open browser to http://localhost:8080/login.html
```

## Status

Design-phase scaffolding. All Go files are stubs with TODO headers describing intent. See `plan.md` for implementation schedule.
