-- Per-session, per-kind lookups on the events table (session reaper activity and start times,
-- latest shim_status for shim health). Until now these filtered idx_events_session(session_id, at) by
-- kind and JSON, i.e. read EVERY event of the session on every call; a few dead sessions with tens of
-- thousands of events pegged a core on the hub (CW-20261012-0120).
--
-- Cost: one index build over the events table. On the 2026-10-13 hub DB (about 1.0M events, 750MB) the
-- build is expected to take several seconds; it runs once, inside the migration transaction at daemon
-- start. CREATE INDEX IF NOT EXISTS keeps the migration safe to re-run.
CREATE INDEX IF NOT EXISTS idx_events_session_kind_at ON events(session_id, kind, at);
