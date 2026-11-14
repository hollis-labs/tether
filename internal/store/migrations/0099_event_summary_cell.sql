-- CW-20261114-0019: a redacted one-line summary and the seat's cell on every
-- event (design v2 section 11). Both nullable, no backfill: rows written before
-- this read back with an empty summary and cell.
ALTER TABLE events ADD COLUMN summary TEXT;
ALTER TABLE events ADD COLUMN scope_cell TEXT;
CREATE INDEX idx_events_scope_cell ON events(scope_cell, id) WHERE scope_cell IS NOT NULL;
