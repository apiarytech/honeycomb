-- One row per persisted tag. instance_id separates TagDatabase instances
-- (one per PLC/runtime) that share the same database.
-- Values are JSON so primitives, arrays and UDTs share one column.
CREATE TABLE IF NOT EXISTS honeycomb_tags (
    instance_id     TEXT      NOT NULL,
    tag_name        TEXT      NOT NULL,
    data_type       TEXT      NOT NULL,
    type_info       TEXT,
    tag_value       TEXT,
    alias           TEXT      NOT NULL DEFAULT '',
    description     TEXT      NOT NULL DEFAULT '',
    direct_address  TEXT      NOT NULL DEFAULT '',
    is_retain       BOOLEAN   NOT NULL DEFAULT 0,
    is_constant     BOOLEAN   NOT NULL DEFAULT 0,
    is_forced       BOOLEAN   NOT NULL DEFAULT 0,
    force_value     TEXT,
    remote_db_id    TEXT      NOT NULL DEFAULT '',
    remote_tag_name TEXT      NOT NULL DEFAULT '',
    updated_at      TIMESTAMP NOT NULL,
    PRIMARY KEY (instance_id, tag_name)
);
