-- Every change a forwarder delivered, per source (one per TagDatabase and
-- node: both nodes of a redundant pair forward under their own source).
-- kind: change, snapshot (a value read after a gap), gap, or lost (records
-- dropped from a full spool before they were sent).
CREATE TABLE IF NOT EXISTS honeycomb_history (
    source_id   VARCHAR(128) NOT NULL,
    epoch       VARCHAR(64)  NOT NULL,
    seq         BIGINT       NOT NULL,
    kind        VARCHAR(16)  NOT NULL,
    tag_name    VARCHAR(255) NOT NULL,
    tag_value   JSONB,
    quality     INTEGER      NOT NULL,
    value_time  TIMESTAMPTZ  NOT NULL,
    reason      VARCHAR(1024) NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (source_id, epoch, seq, kind, tag_name)
);
CREATE INDEX IF NOT EXISTS honeycomb_history_tag_time ON honeycomb_history (source_id, tag_name, value_time);
