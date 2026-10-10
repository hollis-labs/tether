-- Current request state survives retention of the full event history.
-- Updates are committed atomically with their durable environment event.
CREATE TABLE environment_request_status (
 session_id TEXT NOT NULL,
 turn_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 request_kind TEXT NOT NULL CHECK(request_kind IN ('question','approval')),
 is_open INTEGER NOT NULL,
 event_seq INTEGER NOT NULL,
 source_sequence TEXT NOT NULL,
 PRIMARY KEY(session_id,turn_id,request_id)
);
