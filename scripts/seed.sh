#!/usr/bin/env bash
# TODO: Run the seeder to populate 10M user rows into MySQL.
#
# Usage: ./scripts/seed.sh [rows]
#   default rows: 10000000

set -euo pipefail

cd "$(dirname "$0")/.."

ROWS="${1:-10000000}"
DSN="${DSN:-app:app@tcp(localhost:3306)/userdb}"

echo "==> Seeding $ROWS users into $DSN"
go run ./cmd/seed --rows "$ROWS" --dsn "$DSN"
