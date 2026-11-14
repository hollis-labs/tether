-- 0005_broker_envelopes.sql
--
-- Scope: T-v002-s03-04.
--
-- Inter-session mailbox storage per context-pack §02 BrokerEnvelope.
-- v0.0.2 lands the storage primitive only — request/reply semantics
-- and routing logic arrive in Sprint v003-05 / Sprint v002-05.
-- Envelope IDs are TEXT (callers supply UUIDv7 per critical-state
-- note #9; storage layer doesn't enforce the format).
CREATE TABLE broker_envelopes (
    id             TEXT PRIMARY KEY,
    sender         TEXT,
    recipient      TEXT,
    workflow_id    TEXT,
    correlation_id TEXT,
    message_type   TEXT,
    priority       INTEGER,
    payload        TEXT,
    created_at     TEXT NOT NULL,
    delivered_at   TEXT,
    consumed_at    TEXT,
    audit_json     TEXT
);

CREATE INDEX IF NOT EXISTS idx_broker_envelopes_recipient
    ON broker_envelopes(recipient, delivered_at);
CREATE INDEX IF NOT EXISTS idx_broker_envelopes_correlation
    ON broker_envelopes(workflow_id, correlation_id);
