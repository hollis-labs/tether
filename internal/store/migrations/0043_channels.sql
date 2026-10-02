-- Public topics reuse messages for bodies, attribution, threads and retention.
-- A separate insertion sequence makes replay independent of producer clocks
-- and UUID generation order. Private mailbox channel labels are never imported.
CREATE TABLE channel_publications (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    message_id TEXT NOT NULL UNIQUE REFERENCES messages(id)
);
CREATE INDEX idx_channel_publications_name_seq ON channel_publications(name, seq);
