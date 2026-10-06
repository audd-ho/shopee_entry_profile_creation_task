-- Variant 1: reset ONE instance by hand from a .sql file.
--
-- Run against the data_0 instance:
--   mysql -h 127.0.0.1 -P 3211 -u root < db_setup/reset_shard.sql
-- or inside a mysql session:
--   source db_setup/reset_shard.sql;
--
-- Safe to re-run: IF EXISTS / IF NOT EXISTS skip what's already done.
-- Copy and edit the database name (and table) for each other instance.

DROP DATABASE IF EXISTS data_0;
CREATE DATABASE IF NOT EXISTS data_0 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED BY 'app';
GRANT ALL PRIVILEGES ON data_0.* TO 'app'@'%';

USE data_0;
CREATE TABLE IF NOT EXISTS users (
    id             BIGINT UNSIGNED NOT NULL,
    username       VARCHAR(32)     NOT NULL,
    password_hash  VARBINARY(60)   NOT NULL,
    nickname       VARCHAR(128)    NOT NULL,
    avatar_suffix  CHAR(8)         NULL,
    row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
