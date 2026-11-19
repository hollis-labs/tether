-- Teams Stage B (CW-20261118-0046): the hard-stop deadline of a team run and an
-- optional request to end it early. The run lifecycle reaper reads this table;
-- a row is written when a run is formed and closed once the run is dissolved.
-- New table only, no backfill: runs formed before this have no row and are
-- never reaped (dissolve them with the team verbs).
CREATE TABLE team_run_deadlines (
 run_id TEXT PRIMARY KEY,
 deadline TEXT NOT NULL,
 end_reason TEXT NOT NULL DEFAULT '',
 closed INTEGER NOT NULL DEFAULT 0,
 recorded_at TEXT NOT NULL
);
CREATE INDEX team_run_deadlines_open ON team_run_deadlines(closed, deadline);
