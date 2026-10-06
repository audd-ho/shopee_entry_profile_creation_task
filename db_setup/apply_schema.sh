#!/usr/bin/env bash
# Variant 3: create the tables, one schema file per kind of shard.
#
# Data shards get users (schema_data.sql), lookup shards get username_lookup
# (schema_lookup.sql). The files have no USE line; the database name passed
# on the command line picks the target, so one file serves every shard of
# its kind. Uses the repo-root schema files so there is only one copy of
# each table definition.
#
# Usage: ./db_setup/apply_schema.sh   (after reset_all.sh)
#   MYSQL_HOST / MYSQL_ROOT_USER override the defaults below.

set -euo pipefail

cd "$(dirname "$0")/.."

HOST="${MYSQL_HOST:-127.0.0.1}"
ROOT_USER="${MYSQL_ROOT_USER:-root}"

apply() { # port db schema_file
  echo "==> $3 -> $2 on port $1"
  mysql -h "$HOST" -P "$1" -u "$ROOT_USER" "$2" < "$3"
}

apply 3211 data_0   schema_data.sql
apply 3212 data_1   schema_data.sql
apply 3221 lookup_0 schema_lookup.sql
apply 3222 lookup_1 schema_lookup.sql

echo "==> Done. Seed with: ./scripts/seed.sh 10000 --truncate  (then redis-cli FLUSHDB)"
