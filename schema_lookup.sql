-- MySQL schema for the lookup shards (see sharding_design.md).
--
-- Applied to every lookup shard (lookup_0, lookup_1). The users table lives on
-- the data shards instead; see schema_data.sql.

-- ---------------------------------------------------------------------------
-- Per-lookup-shard: username → user_id mapping for global uniqueness
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS username_lookup (
    username  VARCHAR(32)      NOT NULL,
    user_id   BIGINT UNSIGNED  NOT NULL,
    PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
