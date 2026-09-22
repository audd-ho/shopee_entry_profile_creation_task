-- TODO: MySQL schema for the users table (see §5 of system_design.md).
--
-- Notes:
--   - utf8mb4 for full unicode nicknames (emojis etc.)
--   - VARBINARY(60) for bcrypt hash (fixed size)
--   - Unique index on username for O(log n) login lookup at 10M rows
--   - avatar_version starts at 0 (means no avatar uploaded; server serves default.png)
--   - avatar_suffix is NULL when no avatar; 8 hex chars once uploaded
--   - Avatar file path is DERIVED from (id, avatar_version, avatar_suffix), not stored

CREATE TABLE IF NOT EXISTS users (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    username       VARCHAR(32)     NOT NULL,
    password_hash  VARBINARY(60)   NOT NULL,
    nickname       VARCHAR(128)    NOT NULL,
    avatar_version INT UNSIGNED    NOT NULL DEFAULT 0,
    avatar_suffix  CHAR(8)         NULL,
    row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,  -- bumped on any mutating write; cache-key versioning only (§4.1/§4.4.4 of system_design.md), never used in a filename
    created_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
