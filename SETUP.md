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

The frontend is plain HTML/CSS/JS in `frontend/`. No build step. In production it will be served by the Go HTTP server; during early development you can preview it with any static file server.

### Files

```
frontend/
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
cd frontend
python3 -m http.server 8080
```
Then open http://localhost:8080/login.html

**Option 2 — Node (if you prefer):**
```
npx serve frontend -p 8080
# or
npx http-server frontend -p 8080
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

_TBD — fill in once implemented._

Placeholder:
```
cd cmd/http-server
go run .
# listens on :8080 (or configured port)
```

---

## Backend — TCP Server

_TBD — fill in once implemented._

Placeholder:
```
cd cmd/tcp-server
go run .
# listens on :9000 (or configured port)
```

---

## Database (MySQL)

_TBD — fill in once schema + seeder are ready._

Placeholder topics to cover here:
- How to start MySQL locally (Docker or native install)
- Apply schema (`schema.sql`)
- Run seeder for 10M rows (`cmd/seed`)
- Verify row count and index

---

## Cache (Redis)

_TBD — fill in once cache layer is wired._

Placeholder topics to cover here:
- How to start Redis locally (Docker or native install)
- Verify connection (`redis-cli ping`)
- Any keys/prefixes worth knowing

---

## Full local stack

_TBD — filled in once all components exist._

Will describe the recommended sequence:
1. Start MySQL + Redis (docker-compose)
2. Apply schema + seed
3. Start TCP server
4. Start HTTP server
5. Open browser to http://localhost:8080

---

## Testing

_TBD — filled in once tests exist._

Placeholder:
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
