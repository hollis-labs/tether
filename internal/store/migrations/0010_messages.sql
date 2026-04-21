-- 0010_messages.sql
--
-- go-messaging-native envelope table. Aligned 1:1 with the
-- messaging.Envelope type from github.com/hollis-labs/go-messaging.
--
-- This table coexists with broker_envelopes (v002 compat). New code
-- uses /messages/* routes backed by this table. broker_envelopes and
-- /broker/* routes remain for backwards compat until a future migration
-- consolidates them.
--
-- Addresses are stored as URNs (msg://<kind>/<authority>/<id>[/<subid>]).
-- payload and metadata are JSON text. Times are RFC3339 in UTC.
-- canceled_at is a non-contract extension: non-NULL means the sender
-- aborted the envelope so Request waits resolve with ErrCanceled.

CREATE TABLE messages (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    channel      TEXT,
    from_urn     TEXT NOT NULL,
    to_urn       TEXT NOT NULL,
    thread_id    TEXT,
    in_reply_to  TEXT,
    payload      TEXT,
    content_type TEXT,
    metadata     TEXT,
    created_at   TEXT NOT NULL,
    delivered_at TEXT,
    consumed_at  TEXT,
    canceled_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_to_delivered
    ON messages(to_urn, delivered_at);
CREATE INDEX IF NOT EXISTS idx_messages_thread
    ON messages(thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_in_reply_to
    ON messages(in_reply_to);
