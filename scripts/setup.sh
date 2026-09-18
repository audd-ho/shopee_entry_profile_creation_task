#!/usr/bin/env bash
# TODO: One-time setup — start local MySQL + Redis, apply schema.
#
# Usage: ./scripts/setup.sh

set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Starting MySQL + Redis via docker compose..."
docker compose -f deploy/docker-compose.yml up -d

echo "==> Waiting for MySQL to be ready..."
# TODO: proper readiness check (mysqladmin ping)
sleep 10

echo "==> Schema is applied automatically on first container start via /docker-entrypoint-initdb.d."
echo "==> Ready. MySQL @ localhost:3306, Redis @ localhost:6379."
