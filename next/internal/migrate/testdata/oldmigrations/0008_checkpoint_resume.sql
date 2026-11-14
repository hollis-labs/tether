-- 0008_checkpoint_resume.sql
--
-- Scope: T-v004-s03-01 (checkpoint payload schema pin + resume infrastructure).
--
-- Two additive changes:
-- 1. checkpoints.provider_hints — opaque JSON blob from Session.CheckpointHints().
--    Round-tripped back into StartOptions on resume. NULL = no hints provided.
-- 2. logical_agents.launch_id — the launch profile used most recently for this
--    agent. Set by app.Service.LaunchSession so resume can start a new session
--    from the same launch config. NULL until the agent's first session launch.

ALTER TABLE checkpoints ADD COLUMN provider_hints TEXT;
ALTER TABLE logical_agents ADD COLUMN launch_id TEXT;
