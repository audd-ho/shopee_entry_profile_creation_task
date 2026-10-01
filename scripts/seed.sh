#!/usr/bin/env bash
# TODO: Run the seeder to populate 10M user rows into MySQL.
#
# Usage: ./scripts/seed.sh [rows] [extra seed flags...]
#   default rows: 10000000
#   e.g. ./scripts/seed.sh 10000 --truncate
#
# Seeds both tables on every shard: users on the data shards (split exactly
# evenly) and username_lookup on the lookup shards. Shard DSNs and SHARD_VNODES
# come from the environment or .env, the same values the server reads; they
# must match or the server will look for rows on the wrong shard.

set -euo pipefail

cd "$(dirname "$0")/.."

ROWS="${1:-10000000}"
shift || true

echo "==> Seeding $ROWS users across data + lookup shards"
go run ./cmd/seed --rows "$ROWS" "$@"
