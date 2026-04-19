-- client_attachments: one row per live attach subscription, stamped on
-- detach. Enables operators to see who is watching a session and gives
-- the daemon a way to reconcile orphaned rows after a crash.
--
-- This migration is scoped to T-v002-s02-03. Sprint v002-03 will add
-- logical_agents / checkpoints / broker_envelopes in later migrations.
CREATE TABLE IF NOT EXISTS client_attachments (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES sessions(id),
    client_kind   TEXT,
    attached_at   TEXT NOT NULL,
    detached_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_client_attachments_session
    ON client_attachments(session_id, attached_at);
