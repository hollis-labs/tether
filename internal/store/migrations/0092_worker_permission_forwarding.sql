-- Parser checkpoints contain offsets into the existing worker session log,
-- never copies of provider params/bodies. Session/turn/request state remains
-- in the existing environment request reducer.
CREATE TABLE worker_permission_parser (
 session_id TEXT PRIMARY KEY,
 last_seq INTEGER NOT NULL,
 spans_json TEXT NOT NULL,
 discard INTEGER NOT NULL DEFAULT 0
);
-- Ordering fences do not establish pending-state support by themselves. A
-- resolution/terminal arriving first must not let an older request reopen later.
CREATE TABLE environment_request_resolutions (
 session_id TEXT NOT NULL, turn_id TEXT NOT NULL, request_id TEXT NOT NULL,
 source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id,request_id)
);
CREATE TABLE environment_request_turn_ends (
 session_id TEXT NOT NULL, turn_id TEXT NOT NULL, source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id)
);
