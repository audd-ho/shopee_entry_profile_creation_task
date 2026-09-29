-- Setup script for shard databases and tables.
-- Run with: mysql -u root < scripts/setup_shards.sql
--
-- Creates 2 lookup shards + 2 data shards (add data_2 later for the scaling demo).

-- Lookup shards
CREATE DATABASE IF NOT EXISTS lookup_0
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE DATABASE IF NOT EXISTS lookup_1
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Data shards (start with 2)
CREATE DATABASE IF NOT EXISTS data_0
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
CREATE DATABASE IF NOT EXISTS data_1
  DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Username lookup table in each lookup shard
CREATE TABLE IF NOT EXISTS lookup_0.username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS lookup_1.username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Users table in each data shard (snowflake ID, no AUTO_INCREMENT)
CREATE TABLE IF NOT EXISTS data_0.users (
  id             BIGINT UNSIGNED NOT NULL,
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,
  nickname       VARCHAR(128) NOT NULL,
  avatar_suffix  CHAR(8)      NULL,
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS data_1.users (
  id             BIGINT UNSIGNED NOT NULL,
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,
  nickname       VARCHAR(128) NOT NULL,
  avatar_suffix  CHAR(8)      NULL,
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Grant access to the app user
GRANT ALL PRIVILEGES ON lookup_0.* TO 'app'@'%';
GRANT ALL PRIVILEGES ON lookup_1.* TO 'app'@'%';
GRANT ALL PRIVILEGES ON data_0.* TO 'app'@'%';
GRANT ALL PRIVILEGES ON data_1.* TO 'app'@'%';
FLUSH PRIVILEGES;
