-- Replies to a routed message. The reply body is stored once as a messages row
-- addressed to a service (no inbox, delivery-core or wake obligation); this table
-- is the durable queue Tether's reply dispatcher drains at a session's idle
-- boundary, and the delivery state consumers read back. Rows are never deleted
-- by the dispatcher: an unreachable target becomes 'undeliverable' with a reason.
CREATE TABLE routing_replies (
    reply_id TEXT PRIMARY KEY REFERENCES messages(id),
    parent_id TEXT NOT NULL,
    original_session_id TEXT NOT NULL,
    target_session_id TEXT NOT NULL,
    delivered_to_session_id TEXT NOT NULL DEFAULT '',
    logical_agent_id TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL DEFAULT '',
    interrupt INTEGER NOT NULL DEFAULT 0 CHECK (interrupt IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('queued', 'delivering', 'delivered', 'undeliverable')),
    reason TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT,
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    settled_at TEXT
);
CREATE INDEX idx_routing_replies_queue ON routing_replies(target_session_id, state, created_at, reply_id);
CREATE INDEX idx_routing_replies_parent ON routing_replies(parent_id);
CREATE UNIQUE INDEX idx_routing_replies_idempotency ON routing_replies(parent_id, actor, idempotency_key) WHERE idempotency_key != '';
