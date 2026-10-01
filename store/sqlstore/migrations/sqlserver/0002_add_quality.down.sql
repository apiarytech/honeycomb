-- Removes the quality column.
ALTER TABLE dbo.honeycomb_tags DROP CONSTRAINT DF_honeycomb_tags_quality;
ALTER TABLE dbo.honeycomb_tags DROP COLUMN quality;
