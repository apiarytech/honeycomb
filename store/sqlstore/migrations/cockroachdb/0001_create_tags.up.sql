-- One row per persisted tag. instance_id separates TagDatabase instances
-- (one per PLC/runtime) that share the same database.
-- Values are JSONB so primitives, arrays and UDTs share one column and stay queryable.
-- The (instance_id, tag_name) key spreads writes across ranges, so no hash-sharded index is needed.
CREATE TABLE IF NOT EXISTS honeycomb_tags (
    instance_id     VARCHAR(128) NOT NULL,
    tag_name        VARCHAR(255) NOT NULL,
    data_type       VARCHAR(128) NOT NULL,
    type_info       JSONB,
    tag_value       JSONB,
    alias           VARCHAR(255)  NOT NULL DEFAULT '',
    description     VARCHAR(1024) NOT NULL DEFAULT '',
    direct_address  VARCHAR(64)   NOT NULL DEFAULT '',
    is_retain       BOOLEAN NOT NULL DEFAULT FALSE,
    is_constant     BOOLEAN NOT NULL DEFAULT FALSE,
    is_forced       BOOLEAN NOT NULL DEFAULT FALSE,
    force_value     JSONB,
    remote_db_id    VARCHAR(255) NOT NULL DEFAULT '',
    remote_tag_name VARCHAR(255) NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (instance_id, tag_name)
);
