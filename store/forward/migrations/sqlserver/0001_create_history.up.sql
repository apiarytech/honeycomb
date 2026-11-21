-- Every change a forwarder delivered, per source (one per TagDatabase and
-- node: both nodes of a redundant pair forward under their own source).
-- kind: change, snapshot (a value read after a gap), gap, or lost (records
-- dropped from a full spool before they were sent).
CREATE TABLE dbo.honeycomb_history (
    source_id   NVARCHAR(128)  NOT NULL,
    epoch       NVARCHAR(64)   NOT NULL,
    seq         BIGINT         NOT NULL,
    kind        NVARCHAR(16)   NOT NULL,
    tag_name    NVARCHAR(255)  NOT NULL,
    tag_value   NVARCHAR(MAX),
    quality     INT            NOT NULL,
    value_time  DATETIME2(7)   NOT NULL,
    reason      NVARCHAR(1024) NOT NULL DEFAULT N'',
    recorded_at DATETIME2(7)   NOT NULL,
    CONSTRAINT pk_honeycomb_history PRIMARY KEY (source_id, epoch, seq, kind, tag_name)
);
CREATE INDEX honeycomb_history_tag_time ON dbo.honeycomb_history (source_id, tag_name, value_time);
