#!/usr/bin/env bash
# TODO: Load-test script (deferred to Days 9-11).
#
# Will use wrk / vegeta / hey to hit /api/login at target RPS.
# Captures pprof CPU + heap + `perf stat` output for analysis.
#
# Usage (planned):
#   ./scripts/perf.sh --rps 3000 --duration 30s --unique-users 200

set -euo pipefail

echo "TODO: implement load test invocation"
exit 1
