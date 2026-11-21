CREATE TABLE team_run_correlation (
 run_id TEXT PRIMARY KEY,
 launch_key TEXT NOT NULL UNIQUE,
 task_id TEXT NOT NULL DEFAULT '',
 workstream_id TEXT NOT NULL DEFAULT ''
);
