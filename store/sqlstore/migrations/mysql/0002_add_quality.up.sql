-- Adds the quality of each stored value (0 Unknown, 1 Good, 2 Uncertain, 3 Bad).
-- Rows saved before this migration default to Uncertain: they hold real values of unknown freshness.
ALTER TABLE honeycomb_tags ADD COLUMN quality TINYINT UNSIGNED NOT NULL DEFAULT 2;
