-- Recovery may acknowledge a proven end but must never turn a failed/raced
-- observation into a runtime stop on a later retry or after daemon restart.
ALTER TABLE team_host_intents ADD COLUMN observation_only INTEGER NOT NULL DEFAULT 0 CHECK(observation_only IN (0,1));
ALTER TABLE team_port_intents ADD COLUMN observation_only INTEGER NOT NULL DEFAULT 0 CHECK(observation_only IN (0,1));
ALTER TABLE team_port_intents ADD COLUMN observed_sequence INTEGER NOT NULL DEFAULT 0;
