-- Every change a forwarder delivered, per source (one per TagDatabase and
-- node: both nodes of a redundant pair forward under their own source).
-- kind: change, snapshot (a value read after a gap), gap, or lost (records
-- dropped from a full spool before they were sent).
CREATE TABLE honeycomb_history (
    source_id   TEXT    NOT NULL,
    epoch       TEXT    NOT NULL,
    seq         INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    tag_name    TEXT    NOT NULL,
    tag_value   TEXT,
    quality     INTEGER NOT NULL,
    value_time  TIMESTAMP NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    recorded_at TIMESTAMP NOT NULL,
    PRIMARY KEY (source_id, epoch, seq, kind, tag_name)
);
CREATE INDEX honeycomb_history_tag_time ON honeycomb_history (source_id, tag_name, value_time);
