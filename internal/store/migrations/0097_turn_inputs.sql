-- Who asked for a turn (CW-20261021-0010).
-- Every input handed to a session (a chat send, a /turn call, a mail wake, a routing
-- reply, a system injection) is recorded here before the text reaches the runtime,
-- with a sender class derived from authenticated facts only (never from prose or
-- timestamps). A routed output is bound to the oldest unbound ACCEPTED input when
-- its turn begins; a failed or never-accepted input never binds. Sender URNs stay
-- in this table and are never copied onto an output. Rows follow the events
-- retention window. Additive and forward-only; no backfill: history has no rows,
-- so historical outputs read as audience unknown.
CREATE TABLE IF NOT EXISTS turn_inputs (
    input_id       TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL,
    seq            INTEGER NOT NULL,
    source         TEXT NOT NULL CHECK (source IN ('chat_send','session_turn','mcp_send_turn','mail_wake','routing_reply','system')),
    sender_urn     TEXT,
    sender_class   TEXT NOT NULL CHECK (sender_class IN ('operator','agent','system','unknown')),
    basis          TEXT NOT NULL CHECK (basis IN ('authenticated_principal','system_injection','none') OR basis LIKE 'mail_from=%'),
    ref_id         TEXT,
    state          TEXT NOT NULL CHECK (state IN ('submitting','accepted','failed')),
    queued         INTEGER NOT NULL DEFAULT 0,
    submitted_at   TEXT NOT NULL,
    accepted_at    TEXT,
    turn_id        TEXT,
    audience       TEXT,
    audience_basis TEXT,
    classes        TEXT,
    UNIQUE (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_turn_inputs_unbound ON turn_inputs(session_id, seq) WHERE turn_id IS NULL AND state IN ('submitting','accepted');
CREATE INDEX IF NOT EXISTS idx_turn_inputs_turn ON turn_inputs(session_id, turn_id) WHERE turn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_turn_inputs_submitted ON turn_inputs(submitted_at);
