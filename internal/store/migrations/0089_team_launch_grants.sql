-- CW-20261016-0233: the authority a team session was first launched under.
-- Recovery and reconcile launches run without a verified caller; they reuse the
-- grant recorded here (first write wins, never replaced) so they are bounded by
-- the launcher's scopes instead of being unattributed. New table only, no
-- backfill: intents created before this have no grant and stay unattributed.
CREATE TABLE team_launch_grants (
 intent_key TEXT PRIMARY KEY,
 principal_id TEXT NOT NULL,
 scopes_json TEXT NOT NULL,
 recorded_at TEXT NOT NULL
);
