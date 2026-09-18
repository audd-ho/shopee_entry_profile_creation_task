# Setup & Run

Setup and run instructions for the entry-task project. Fill in each section as the corresponding component is built.

---

## Prerequisites

Assumed installed on the dev machine:
- macOS (Darwin) — instructions target zsh
- A modern browser (Chrome, Safari, Firefox)
- Python 3 (pre-installed on macOS) — used only for local static preview of the frontend

To be added as their sections come online:
- Go 1.22+
- MySQL 8+
- Redis 7+
- Docker + docker-compose (optional, for local infra)

---

## Frontend (static preview)

The web is plain HTML/CSS/JS in `web/`. No build step. In production it will be served by the Go HTTP server; during early development you can preview it with any static file server.

### Files

```
web/
├── login.html      # POST /api/login
├── signup.html     # POST /api/signup
├── profile.html    # GET /api/user, POST /api/profile/{nickname,avatar}, POST /api/logout
├── style.css
├── app.js          # placeholder for shared JS
└── README.md
```

### Preview locally

Any of the following works. Pick one.

**Option 1 — Python (recommended, zero install):**
```
cd web
python3 -m http.server 8080
```
Then open http://localhost:8080/login.html

**Option 2 — Node (if you prefer):**
```
npx serve web -p 8080
# or
npx http-server web -p 8080
```

**Option 3 — VS Code Live Server extension:**
Right-click any `.html` file → "Open with Live Server". Auto-reloads on save.

**Option 4 — open the file directly:**
Double-click `login.html`. Works for viewing the layout only — API calls (`fetch('/api/...')`) will fail under the `file://` origin. Fine for a quick eyeball, not for testing interactions.

### Expected behavior during preview

- Pages render, forms accept input, client-side validation works
- Submitting a form will fail (network error / 404) until the backend HTTP server is running
- This is expected; the frontend is ready for the backend to be wired in

### Stop

`Ctrl-C` in the terminal running the static server.

---

## Backend — HTTP Server

```
go run ./cmd/http-server
# Listens on :8080 (configurable via HTTP_ADDR env var)
# Requires the TCP server to be running (configurable via TCP_ADDR)
```

Environment overrides:
- `HTTP_ADDR` — listen address (default `:8080`)
- `TCP_ADDR` — TCP backend address (default `127.0.0.1:9000`)
- `HTTP_TCP_POOL_SIZE` — persistent conns to TCP backend (default `8`)
- `WEB_ROOT` — static asset root (default `web`)
- `UPLOADS_ROOT` — avatar file root (default `uploads`)
- `SESSION_TTL_SECONDS` — cookie / session TTL (default `86400`)

---

## Backend — TCP Server

```
go run ./cmd/tcp-server
# Listens on :9000 (configurable via TCP_LISTEN)
# Requires MySQL + Redis running
```

Environment overrides:
- `TCP_LISTEN` — listen address (default `:9000`)
- `MYSQL_DSN` — MySQL connection string (default `app:app@tcp(127.0.0.1:3306)/userdb?parseTime=true&charset=utf8mb4`)
- `MYSQL_MAX_OPEN` — pool size (default `50`)
- `MYSQL_MAX_WAIT_MS` — pool acquire timeout (default `200`)
- `REDIS_ADDR` — Redis address (default `127.0.0.1:6379`)
- `REDIS_POOL_SIZE` — Redis pool size (default `50`)
- `BCRYPT_COST` — bcrypt cost factor (default `10`)

---

## Database (MySQL)

Start MySQL via Docker Compose:
```
docker compose -f deploy/docker-compose.yml up -d mysql
```

Schema is auto-applied on first container start (from `schema.sql`).
Manual apply (if using local MySQL):
```
mysql -u app -p userdb < schema.sql
```

Seed data (10M rows):
```
./scripts/seed.sh 10000000
# Or a smaller number for dev:
./scripts/seed.sh 10000
```

---

## Cache (Redis)

Start Redis via Docker Compose:
```
docker compose -f deploy/docker-compose.yml up -d redis
```

Verify:
```
redis-cli -h 127.0.0.1 -p 6379 ping    # PONG
```

Key patterns used (see §5 of `system_design.md`):
- `sess:<token>` — user sessions (TTL 24h)
- `creds:<username>` — cached credentials for login (TTL 10min)
- `user:<user_id>` — cached user display data for /api/user (TTL 1h)

---

## Full local stack

```bash
# 1. Start MySQL + Redis
./scripts/setup.sh
# (equivalent: docker compose -f deploy/docker-compose.yml up -d)

# 2. Seed users
./scripts/seed.sh 10000       # small dev seed
# ./scripts/seed.sh 10000000  # full perf-test seed (long)

# 3. Start TCP backend (in one terminal)
go run ./cmd/tcp-server

# 4. Start HTTP frontend (in another terminal)
go run ./cmd/http-server

# 5. Open browser to http://localhost:8080/login.html
```

---

## Testing

```
go test ./...
```

---

## Performance testing

_Deferred — will be added when the perf phase begins (see `plan.md` Day 9-11)._

Will cover:
- Load generator setup (wrk / vegeta / k6)
- Test scripts and parameters
- How to capture pprof / `perf stat` / flame graphs
- Where results land (`perf/` directory)

---

## Troubleshooting

_Add entries as issues are hit and resolved._

- **Port already in use:** `lsof -i :8080` to find the offender, `kill <pid>` to free it.
- **Frontend fetch fails with `file://` origin:** use a static server (Option 1-3 above), not double-click.
