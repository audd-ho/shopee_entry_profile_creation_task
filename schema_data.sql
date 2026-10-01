-- TODO: MySQL schema for the users table (see §5 of system_design.md).
--
-- Notes:
--   - utf8mb4 for full unicode nicknames (emojis etc.)
--   - VARBINARY(60) for bcrypt hash (fixed size)
--   - Unique index on username for O(log n) login lookup at 10M rows
--   - avatar_suffix is NULL when no avatar uploaded (server serves default.png); 8 hex chars once uploaded
--   - Avatar file path is DERIVED from (id, avatar_suffix), not stored. There is no
--     avatar_version column: an earlier version of this schema had one, generated
--     atomically in MySQL and embedded in the filename, which forced the database
--     write to happen before the file existed (see internal/model/user.go's comment
--     on AvatarRelPath for the failure mode that caused, and why a random suffix
--     generated with no DB round trip replaced it).
--   - row_version is unrelated to avatar_suffix: it bumps on ANY mutating write and
--     exists purely to build/tear down Redis cache keys (§4.1/§4.4.4), never a filename

-- ---------------------------------------------------------------------------
-- Per-data-shard: users table (id is snowflake, no AUTO_INCREMENT).
-- Applied to every data shard (data_0, data_1); username_lookup lives on the
-- lookup shards instead, see schema_lookup.sql.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id             BIGINT UNSIGNED NOT NULL,              -- snowflake ID, app-generated
    username       VARCHAR(32)     NOT NULL,
    password_hash  VARBINARY(60)   NOT NULL,
    nickname       VARCHAR(128)    NOT NULL,
    avatar_suffix  CHAR(8)         NULL,
    row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,    -- bumped on any mutating write; cache-key versioning only (§4.1/§4.4.4 of system_design.md), never used in a filename
    created_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
