-- Retention receipts are kept indefinitely, independently of swept history.
-- Each receipt commits in the same transaction as its deletion batch.
CREATE TABLE retention_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    table_name TEXT NOT NULL,
    cutoff TEXT NOT NULL,
    removed INTEGER NOT NULL CHECK (removed > 0)
);
CREATE INDEX idx_events_retention_at ON events(at);
