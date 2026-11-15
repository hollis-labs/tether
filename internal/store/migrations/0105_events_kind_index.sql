-- Events history indexes (CW-20261114-0166). Build times and sizes measured on
-- a VACUUM INTO copy of the hub DB (12.0 GB, 5.4M events).
--
-- Kind-filtered newest-first reads: GET /events?kind=X and
-- tether_events_history scanned the whole table (95.9 s); now 5 ms.
-- 10.5 s to build, 152 MB.
CREATE INDEX IF NOT EXISTS idx_events_kind_id ON events(kind, id);
-- Session + kind by id. Without it, idx_events_kind_id wins the plan for any
-- session_id=? AND kind=? query ordered or maxed by id (it saves a sort), and
-- walks every row of the kind: 3.3 s for one old session's output, against
-- 1 ms here. 6.2 s to build, 346 MB.
CREATE INDEX IF NOT EXISTS idx_events_session_kind_id ON events(session_id, kind, id);
