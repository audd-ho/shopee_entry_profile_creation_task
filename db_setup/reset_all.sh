#!/usr/bin/env bash
# Variant 2: reset the databases on ALL instances in one go.
#
# Drops and re-creates one database per instance, and makes sure the app user
# exists and has access to it. Creates no tables; run apply_schema.sh next.
#
# Usage: ./db_setup/reset_all.sh
#   MYSQL_HOST / MYSQL_ROOT_USER override the defaults below.
#   Passwords: put them in ~/.my.cnf ([client] user=... password=...), or
#   leave root passwordless (instances set up with --initialize-insecure).

set -euo pipefail

HOST="${MYSQL_HOST:-127.0.0.1}"
ROOT_USER="${MYSQL_ROOT_USER:-root}"

# port:dbname — keep the same order as DATA_SHARD_DSNS / LOOKUP_SHARD_DSNS
SHARDS="3211:data_0 3212:data_1 3221:lookup_0 3222:lookup_1"

for pair in $SHARDS; do
  port=${pair%%:*}
  db=${pair##*:}
  echo "==> $db on port $port"
  mysql -h "$HOST" -P "$port" -u "$ROOT_USER" <<SQL
DROP DATABASE IF EXISTS $db;
CREATE DATABASE $db DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED BY 'app';
GRANT ALL PRIVILEGES ON $db.* TO 'app'@'%';
SQL
done

echo "==> Done. Next: ./db_setup/apply_schema.sh"
