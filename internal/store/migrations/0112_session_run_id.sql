-- Launcher-asserted Torque runs.id, distinct from Tether team_runs.run_id.
-- Existing sessions stay unknown; no team/session/payload-derived backfill.
ALTER TABLE sessions ADD COLUMN run_id TEXT;
