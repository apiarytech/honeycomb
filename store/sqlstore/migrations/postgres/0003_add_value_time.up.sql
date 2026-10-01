-- Adds when each stored value or quality last changed: the device time if a driver
-- supplied it, else the time of the write. NULL means unknown (rows saved before this migration).
ALTER TABLE honeycomb_tags ADD COLUMN value_time TIMESTAMPTZ;
