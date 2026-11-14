-- Team adoption (CW-20261016-0151): an already-running, already-enrolled
-- actor and its live session join a team run without being provisioned by it.
-- The row is the host's proof of what was adopted and the reason cleanup must
-- release the member instead of stopping its session. A provisioned member has
-- no row here.
CREATE TABLE team_host_adoptions (
 intent_key TEXT PRIMARY KEY REFERENCES team_host_intents(intent_key),
 run_id TEXT NOT NULL,
 actor TEXT NOT NULL,
 session_id TEXT NOT NULL,
 binding_generation INTEGER NOT NULL CHECK(binding_generation > 0),
 launch_id TEXT NOT NULL,
 definition_id TEXT NOT NULL,
 definition_revision TEXT NOT NULL,
 definition_digest TEXT NOT NULL,
 principal TEXT NOT NULL,
 suspended INTEGER NOT NULL DEFAULT 0 CHECK(suspended IN (0,1)),
 released INTEGER NOT NULL DEFAULT 0 CHECK(released IN (0,1))
);
-- One active team per actor and per session. team_host_bindings enforces the
-- same across provisioned and adopted members; these make the adoption table
-- refuse a duplicate on its own.
CREATE UNIQUE INDEX team_host_adoption_active_actor ON team_host_adoptions(actor) WHERE released=0;
CREATE UNIQUE INDEX team_host_adoption_active_session ON team_host_adoptions(session_id) WHERE released=0;
CREATE INDEX team_host_adoption_run ON team_host_adoptions(run_id) WHERE released=0;
