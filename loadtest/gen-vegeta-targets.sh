#!/usr/bin/env bash
# Generates the body files and targets file vegeta needs. Run it ONCE; you do
# not write these by hand.
#
#   ./loadtest/gen-vegeta-targets.sh 200
#   vegeta attack -targets=loadtest/vegeta/targets.txt -rate=3000 -duration=30s \
#     -timeout=15s -max-workers=1000 > result.bin
#   vegeta report result.bin
#
# k6 (loadtest/login.js) needs none of this and is the better default. vegeta
# earns its place as a second opinion: it is the stricter open-loop generator,
# so if the two disagree about achieved throughput, believe vegeta.
set -euo pipefail

USERS="${1:-200}"
BASE="${BASE:-http://127.0.0.1:8080}"
DIR="$(cd "$(dirname "$0")" && pwd)/vegeta"
STRIDE=$(( 10000000 / USERS ))

rm -rf "$DIR"; mkdir -p "$DIR/bodies"
: > "$DIR/targets.txt"

for i in $(seq 1 "$USERS"); do
  n=$(( (i - 1) * STRIDE + 1 ))
  printf '{"username":"user%08d","password":"password123"}' "$n" > "$DIR/bodies/u$i.json"
  printf 'POST %s/api/login\nContent-Type: application/json\n@%s/bodies/u%d.json\n\n' \
    "$BASE" "$DIR" "$i" >> "$DIR/targets.txt"
done

echo "generated $USERS targets in $DIR/targets.txt"
