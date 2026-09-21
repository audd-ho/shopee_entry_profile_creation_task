#!/usr/bin/env bash
# Drives a full with-cache / without-cache experiment end to end: optional
# re-seed, Redis flush, server restart in each mode, a measurement run per mode,
# and a comparison table.
#
#   ./loadtest/run-experiment.sh                          # both halves, current data
#   ./loadtest/run-experiment.sh --reseed --cost 6        # re-seed at cost 6 first
#   ./loadtest/run-experiment.sh --rate-on 6000 --rate-off 3000 --duration 30s
#
# The servers it starts are its own: it records their PIDs and kills only those
# on exit, so a server you started by hand is never touched. If its ports are
# already busy it refuses to run rather than guessing.
set -uo pipefail
cd "$(dirname "$0")/.."

# ---------------------------------------------------------------------------
# Configuration. Every value here is also an environment variable, so CI or a
# wrapper can override without editing the file.
# ---------------------------------------------------------------------------
COST="${BCRYPT_COST:-10}"          # bcrypt cost for BOTH the seed and the server
ROWS="${ROWS:-10000000}"           # rows to seed when --reseed is given
BATCH="${BATCH:-5000}"             # rows per INSERT
USERS="${USERS:-200}"              # distinct users the load test cycles through
DURATION="${DURATION:-30s}"        # per-run duration
RATE_ON="${RATE_ON:-6000}"         # target rate WITH the cache layer
RATE_OFF="${RATE_OFF:-3000}"       # target rate WITHOUT the cache layer
HTTP_PORT="${HTTP_PORT:-8080}"
TCP_PORT="${TCP_PORT:-9000}"
OUT="${OUT:-loadtest/results}"
RESEED=0
FLUSH=1
MODES="on off"

usage() {
  cat <<EOF
run-experiment.sh — one command runs the WHOLE with-cache/without-cache
experiment. You do not invoke this once per phase; combine the flags you want.

What a single run does, in order:
  1. re-seed the users table            (only with --reseed)
  2. flush Redis, start the server with the cache ON,  load at --rate-with-cache
  3. flush Redis, start the server with the cache OFF, load at --rate-without-cache
  4. print a comparison table of both halves

Two rates because the spec sets different targets for the two halves: 6000/s
with the cache layer, 3000/s without it.

Typical use:
  ./loadtest/run-experiment.sh --reseed --cost 6 \\
      --rate-with-cache 6000 --rate-without-cache 3000 --duration 30s

Flags:
  --reseed                   TRUNCATE and re-seed $ROWS rows first (DESTRUCTIVE)
  --cost N                   bcrypt cost for BOTH the seed and the server (default $COST)
  --rows N                   rows to seed with --reseed (default $ROWS)
  --batch N                  rows per INSERT when seeding (default $BATCH)
  --duration D               duration of EACH half, k6 syntax (default $DURATION)
  --rate-with-cache N        rate for the cache-ON half    (default $RATE_ON)
  --rate-without-cache N     rate for the cache-OFF half   (default $RATE_OFF)
  --users N                  distinct users to cycle through (default $USERS)
  --only with|without        run only one half instead of both
  --no-flush                 skip the Redis flush (NOT recommended; see README)
  --http-port N              (default $HTTP_PORT)
  --tcp-port N               (default $TCP_PORT)

Aliases kept for brevity: --rate-on = --rate-with-cache,
--rate-off = --rate-without-cache, --only on|off = --only with|without.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --reseed)      RESEED=1; shift ;;
    --cost)        COST="$2"; shift 2 ;;
    --rows)        ROWS="$2"; shift 2 ;;
    --batch)       BATCH="$2"; shift 2 ;;
    --duration)    DURATION="$2"; shift 2 ;;
    --rate-with-cache|--rate-on)       RATE_ON="$2"; shift 2 ;;
    --rate-without-cache|--rate-off)   RATE_OFF="$2"; shift 2 ;;
    --users)       USERS="$2"; shift 2 ;;
    # "with"/"without" refer to the cache layer. Normalised to the on/off that
    # CACHE_LAYER itself uses, so only one vocabulary reaches the server.
    --only)
      case "$2" in
        with|on)      MODES="on" ;;
        without|off)  MODES="off" ;;
        both)         MODES="on off" ;;
        *) echo "--only takes with|without|both, got '$2'" >&2; exit 2 ;;
      esac
      shift 2 ;;
    --no-flush)    FLUSH=0; shift ;;
    --http-port)   HTTP_PORT="$2"; shift 2 ;;
    --tcp-port)    TCP_PORT="$2"; shift 2 ;;
    -h|--help)     usage; exit 0 ;;
    *)             echo "unknown flag: $1" >&2; usage; exit 2 ;;
  esac
done

BASE="http://127.0.0.1:$HTTP_PORT"
mkdir -p "$OUT"
STAMP=$(date +%Y%m%d-%H%M%S)
LOGDIR="$OUT/experiment-$STAMP"
mkdir -p "$LOGDIR"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
die() { echo "error: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preflight. Every one of these has cost someone an invalid run at least once.
# ---------------------------------------------------------------------------
say "preflight"

command -v redis-cli >/dev/null || die "redis-cli not found"
command -v mysql     >/dev/null || die "mysql client not found"
[ -x "${K6:-$HOME/go/bin/k6}" ] || die "k6 not found (go install go.k6.io/k6@latest)"
redis-cli ping >/dev/null 2>&1  || die "redis not reachable"
mysql -h 127.0.0.1 -u app -papp userdb -N -B -e "SELECT 1" >/dev/null 2>&1 \
  || die "mysql not reachable as app@userdb"

# Refuse to fight over a port rather than killing a server we do not own.
for p in "$HTTP_PORT" "$TCP_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then
    die "port $p is already in use. Stop your server, or pass --http-port/--tcp-port."
  fi
done

go build -o bin/tcp-server ./cmd/tcp-server   || die "build tcp-server"
go build -o bin/http-server ./cmd/http-server || die "build http-server"
go build -o bin/seed ./cmd/seed               || die "build seed"
echo "binaries built; redis and mysql reachable; ports $HTTP_PORT/$TCP_PORT free"

# ---------------------------------------------------------------------------
# Server lifecycle. PIDs are recorded so the trap kills only our own children.
# ---------------------------------------------------------------------------
TCP_PID=""; HTTP_PID=""
stop_servers() {
  [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
  [ -n "$TCP_PID" ]  && kill "$TCP_PID"  2>/dev/null
  wait "$HTTP_PID" "$TCP_PID" 2>/dev/null
  HTTP_PID=""; TCP_PID=""
}
trap stop_servers EXIT INT TERM

start_servers() {
  local mode="$1" log="$2"
  CACHE_LAYER="$mode" BCRYPT_COST="$COST" \
    TCP_LISTEN=":$TCP_PORT" HTTP_ADDR=":$HTTP_PORT" \
    ./bin/tcp-server > "$log/tcp.log" 2>&1 &
  TCP_PID=$!
  TCP_LISTEN=":$TCP_PORT" HTTP_ADDR=":$HTTP_PORT" \
    ./bin/http-server > "$log/http.log" 2>&1 &
  HTTP_PID=$!

  # Readiness is a successful LOGIN, three times running -- not a ping. The
  # http server's TCP pool fills its slots asynchronously, so /api/ping can
  # answer off the first live slot while the other seven are still dialling.
  # Starting the load there produces a burst of spurious socket errors.
  local ok=0
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null -X POST "$BASE/api/login" \
         -H 'Content-Type: application/json' \
         -d '{"username":"user00000001","password":"password123"}'; then
      ok=$((ok+1))
      [ "$ok" -ge 3 ] && break
    else
      ok=0
    fi
    sleep 0.5
  done
  [ "$ok" -ge 3 ] || { cat "$log/tcp.log" "$log/http.log" >&2; die "servers did not become ready"; }

  # Confirm from the log which half we are actually measuring, rather than
  # trusting the variable we just set.
  if grep -q 'cache: user/profile cache DISABLED' "$log/tcp.log"; then
    echo "server reports: cache layer OFF"
    [ "$mode" = "off" ] || die "asked for cache=$mode but server says OFF"
  else
    echo "server reports: cache layer ON"
    [ "$mode" = "on" ] || die "asked for cache=$mode but server says ON"
  fi
}

# ---------------------------------------------------------------------------
# Optional re-seed. Destructive, hence opt-in.
# ---------------------------------------------------------------------------
if [ "$RESEED" -eq 1 ]; then
  say "re-seeding $ROWS rows at bcrypt cost $COST (TRUNCATE first)"
  echo "this DESTROYS every existing row, including hand-made accounts."
  ./bin/seed --truncate --rows "$ROWS" --batch "$BATCH" --bcrypt-cost "$COST" \
    2>&1 | tee "$LOGDIR/seed.log" | tail -3
  ACTUAL=$(mysql -h 127.0.0.1 -u app -papp userdb -N -B -e "SELECT COUNT(*) FROM users" 2>/dev/null)
  echo "rows now in users: $ACTUAL"
  # The seeder counts ATTEMPTED rows, so a silent no-op is possible with
  # --ignore. Check what actually landed.
  [ "$ACTUAL" -ge "$ROWS" ] || die "expected >= $ROWS rows, found $ACTUAL"
fi

# ---------------------------------------------------------------------------
# The runs.
# ---------------------------------------------------------------------------
for mode in $MODES; do
  case "$mode" in
    on)  rate="$RATE_ON" ;;
    off) rate="$RATE_OFF" ;;
    *)   die "--only takes 'on' or 'off', got '$mode'" ;;
  esac

  RUNDIR="$LOGDIR/cache-$mode"; mkdir -p "$RUNDIR"

  say "cache=$mode, bcrypt=$COST, target ${rate}/s"

  # Flush BEFORE each half, not once at the start. The with-cache run leaves a
  # warm cache behind; carrying that into the next run would mean the two halves
  # started from different states.
  if [ "$FLUSH" -eq 1 ]; then
    redis-cli flushdb >/dev/null && echo "redis flushed"
  else
    echo "redis NOT flushed (--no-flush): results may reflect stale entries"
  fi

  start_servers "$mode" "$RUNDIR"

  BASE="$BASE" USERS="$USERS" OUT="$RUNDIR" LABEL="cache-$mode" \
    ./loadtest/run-login.sh "$rate" "$DURATION" 2>&1 | tee "$RUNDIR/run.log"

  stop_servers
done

# ---------------------------------------------------------------------------
# Comparison table, built from the RESULT lines each run emitted.
# ---------------------------------------------------------------------------
say "summary (bcrypt cost $COST, ${DURATION} per run, ${USERS} unique users)"
grep -h '^RESULT ' "$LOGDIR"/cache-*/*.result 2>/dev/null | python3 -c "
import sys
rows=[dict(kv.split('=',1) for kv in l.split()[1:]) for l in sys.stdin]
if not rows:
    print('no results parsed'); raise SystemExit
cols=[('label','cache'),('rate','target/s'),('achieved','actual/s'),
      ('failed_pct','failed%'),('socket_errors','sockerr'),
      ('p95','p95ms'),('p99','p99ms'),('hit_ratio','hit%'),
      ('mysql_queries','sql'),('peak_cpu','cpu%')]
w=[max(len(h),*(len(str(r.get(k,'-'))) for r in rows)) for k,h in cols]
print('  '.join(h.ljust(x) for (k,h),x in zip(cols,w)))
for r in rows:
    print('  '.join(str(r.get(k,'-')).ljust(x) for (k,h),x in zip(cols,w)))
"
echo
echo "artifacts: $LOGDIR/"
