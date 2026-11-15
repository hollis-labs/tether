-- Kind-filtered newest-first history reads (CW-20261114-0166): GET /events?kind=X
-- and tether_events_history scanned the whole events table (about 4.8M rows,
-- 2 minutes on the hub). Measured on a VACUUM INTO copy of the hub DB
-- (12.0 GB): 10.5 s to build, 152 MB.
CREATE INDEX IF NOT EXISTS idx_events_kind_id ON events(kind, id);
