#!/usr/bin/env bash
# One measurement run: snapshot the instruments, drive load, print the deltas.
#
#   ./loadtest/run-login.sh <rate> <duration>
#   ./loadtest/run-login.sh 3000 30s
#
# Env: BASE (default http://127.0.0.1:8080), USERS (200), OUT, K6, LABEL
#
# This script does NOT start, stop or configure the servers. Start them yourself
# with the CACHE_LAYER and BCRYPT_COST you mean to measure, or use
# run-experiment.sh which drives both halves for you.
set -uo pipefail

RATE="${1:-3000}"
DURATION="${2:-30s}"
BASE="${BASE:-http://127.0.0.1:8080}"
USERS="${USERS:-200}"
OUT="${OUT:-loadtest/results}"
K6="${K6:-$HOME/go/bin/k6}"
LABEL="${LABEL:-run}"

command -v "$K6" >/dev/null 2>&1 || { echo "k6 not found at $K6 (go install go.k6.io/k6@latest)" >&2; exit 127; }

mkdir -p "$OUT"
TAG="$OUT/${LABEL}-${RATE}ps-$(date +%Y%m%d-%H%M%S)"

if ! curl -sf -o /dev/null "$BASE/api/ping"; then
  echo "server not reachable at $BASE — start it first" >&2
  exit 1
fi

redis_stat() { redis-cli info stats 2>/dev/null | tr -d '\r' | awk -F: "/^$1:/{print \$2}"; }
mysql_stat() {
  mysql -h 127.0.0.1 -u app -papp userdb -N -B \
    -e "SHOW GLOBAL STATUS LIKE '$1'" 2>/dev/null | awk '{print $2}'
}

H0=$(redis_stat keyspace_hits);  M0=$(redis_stat keyspace_misses)
Q0=$(mysql_stat Queries);        BR0=$(mysql_stat Innodb_buffer_pool_reads)
S0=$(mysql_stat Slow_queries);   CE0=$(mysql_stat Connection_errors_max_connections)

# 'us' pinned near 100% is the difference between "the server is busy" and
# "the server is waiting on something".
vmstat 1 > "$TAG.vmstat" 2>/dev/null &
VMPID=$!

echo "=== $LABEL: login @ ${RATE}/s for ${DURATION}, ${USERS} unique users ==="
# p(99) is NOT in k6's default trend stats, so it has to be requested
# explicitly or the threshold above it has nothing to read.
"$K6" run \
  --summary-export="$TAG.summary.json" \
  --summary-trend-stats='avg,min,med,max,p(90),p(95),p(99)' \
  -e RATE="$RATE" -e DURATION="$DURATION" -e BASE="$BASE" -e USERS="$USERS" \
  loadtest/login.js 2>&1 | tee "$TAG.k6.txt"
K6_EXIT=${PIPESTATUS[0]}

kill $VMPID 2>/dev/null

H1=$(redis_stat keyspace_hits);  M1=$(redis_stat keyspace_misses)
Q1=$(mysql_stat Queries);        BR1=$(mysql_stat Innodb_buffer_pool_reads)
S1=$(mysql_stat Slow_queries);   CE1=$(mysql_stat Connection_errors_max_connections)

HITS=$((H1-H0)); MISSES=$((M1-M0)); TOTAL=$((HITS+MISSES))
QUERIES=$((Q1-Q0))
PEAK_CPU=$(awk 'NR>2 && $13 ~ /^[0-9]+$/ {if ($13>m) m=$13} END {print m+0}' "$TAG.vmstat")

echo
echo "=== cache (a login test only READS user:<username>; sessions are writes) ==="
if [ "$TOTAL" -gt 0 ]; then
  RATIO=$(awk "BEGIN{printf \"%.1f\", $HITS*100/$TOTAL}")
  echo "redis hits=$HITS misses=$MISSES hit_ratio=${RATIO}%"
else
  RATIO="n/a"
  echo "redis hits=0 misses=0  (cache layer OFF — no reads reached Redis)"
fi
echo
echo "=== mysql ==="
echo "queries=$QUERIES slow=$((S1-S0)) buffer_pool_disk_reads=$((BR1-BR0)) max_conn_errors=$((CE1-CE0))"
echo
echo "=== cpu ==="
echo "peak user cpu = ${PEAK_CPU}%"

# One machine-readable line so run-experiment.sh can build a comparison table
# without re-parsing k6's human summary.
python3 - "$TAG.summary.json" "$LABEL" "$RATE" "$RATIO" "$HITS" "$MISSES" "$QUERIES" "$PEAK_CPU" <<'PY' | tee "$TAG.result"
import json, sys
path, label, rate, ratio, hits, misses, queries, cpu = sys.argv[1:9]
m = json.load(open(path)).get('metrics', {})
d = m.get('http_req_duration', {})
def g(k, default=0.0):
    v = d.get(k, default)
    return v if isinstance(v, (int, float)) else default
print("RESULT "
      f"label={label} rate={rate} "
      f"achieved={m.get('http_reqs',{}).get('rate',0):.1f} "
      f"reqs={m.get('http_reqs',{}).get('count',0)} "
      f"failed_pct={m.get('http_req_failed',{}).get('value',0)*100:.2f} "
      f"socket_errors={m.get('socket_errors',{}).get('count',0)} "
      f"p95={g('p(95)'):.1f} p99={g('p(99)'):.1f} "
      f"hit_ratio={ratio} hits={hits} misses={misses} "
      f"mysql_queries={queries} peak_cpu={cpu}")
PY

echo "artifacts: $TAG.*"
exit $K6_EXIT
