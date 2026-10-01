-- One row per persisted tag. instance_id separates TagDatabase instances
-- (one per PLC/runtime) that share the same database.
-- Values are JSON text so primitives, arrays and UDTs share one column.
IF OBJECT_ID(N'dbo.honeycomb_tags', N'U') IS NULL
CREATE TABLE dbo.honeycomb_tags (
    instance_id     NVARCHAR(128)  NOT NULL,
    tag_name        NVARCHAR(255)  NOT NULL,
    data_type       NVARCHAR(128)  NOT NULL,
    type_info       NVARCHAR(MAX),
    tag_value       NVARCHAR(MAX),
    alias           NVARCHAR(255)  NOT NULL DEFAULT N'',
    description     NVARCHAR(1024) NOT NULL DEFAULT N'',
    direct_address  NVARCHAR(64)   NOT NULL DEFAULT N'',
    is_retain       BIT            NOT NULL DEFAULT 0,
    is_constant     BIT            NOT NULL DEFAULT 0,
    is_forced       BIT            NOT NULL DEFAULT 0,
    force_value     NVARCHAR(MAX),
    remote_db_id    NVARCHAR(255)  NOT NULL DEFAULT N'',
    remote_tag_name NVARCHAR(255)  NOT NULL DEFAULT N'',
    updated_at      DATETIME2(7)   NOT NULL,
    CONSTRAINT pk_honeycomb_tags PRIMARY KEY (instance_id, tag_name)
);
