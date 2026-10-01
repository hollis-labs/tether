-- 0033_session_pid_started_at.sql
--
-- Records when the process behind sessions.pid started, as the OS reports it
-- (UTC, second precision), so the daemon-start sweep can tell a session's own
-- surviving process from an unrelated process that later reused its pid
-- (CW-20260912-0085). NULL for sessions launched before this column existed;
-- the sweep falls back to a weaker check for those.
ALTER TABLE sessions ADD COLUMN pid_started_at TEXT;
