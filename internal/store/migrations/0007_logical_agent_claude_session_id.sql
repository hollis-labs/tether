-- 0007_logical_agent_claude_session_id.sql
--
-- Scope: T-v004-s02-05.
--
-- Persist the claude CLI session_id observed on a `system/init` event so
-- subsequent claudestream launches can pass `--resume <id>` and continue
-- the prior conversation across daemon restarts. Keyed on the logical
-- agent row because each logical agent holds at most one active claude
-- conversation in v0.0.4 (latest-only resume — see sprint file T-05
-- scope fences).
--
-- Nullable: pre-existing rows from v0.0.2 and non-claudestream agents
-- both sit at NULL until their first system/init event lands.

ALTER TABLE logical_agents ADD COLUMN claude_session_id TEXT;
