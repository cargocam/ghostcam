-- New cameras default to streaming-only (no HLS recording). Existing rows
-- keep whatever recording_mode value they already have — the default only
-- affects fresh INSERTs that omit the column.
ALTER TABLE cameras ALTER COLUMN recording_mode SET DEFAULT 'never';
