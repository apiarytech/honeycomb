-- Every change a forwarder delivered, per source (one per TagDatabase and
-- node: both nodes of a redundant pair forward under their own source).
-- kind: change, snapshot (a value read after a gap), gap, or lost (records
-- dropped from a full spool before they were sent).
-- Key columns are sized to stay under InnoDB's 3072-byte index limit with utf8mb4.
CREATE TABLE IF NOT EXISTS honeycomb_history (
    source_id   VARCHAR(128)  NOT NULL,
    epoch       VARCHAR(64)   NOT NULL,
    seq         BIGINT        NOT NULL,
    kind        VARCHAR(16)   NOT NULL,
    tag_name    VARCHAR(255)  NOT NULL,
    tag_value   JSON,
    quality     INT           NOT NULL,
    value_time  DATETIME(6)   NOT NULL,
    reason      VARCHAR(1024) NOT NULL DEFAULT '',
    recorded_at DATETIME(6)   NOT NULL,
    PRIMARY KEY (source_id, epoch, seq, kind, tag_name),
    INDEX honeycomb_history_tag_time (source_id, tag_name, value_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
