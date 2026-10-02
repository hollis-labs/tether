-- Manual content-removal receipts are independent of expiring event history.
-- No message FK: the receipt must survive deletion of the structural row.
-- Authorization is a self-asserted URN; no payload/metadata is copied here.
CREATE TABLE message_purge_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    table_name TEXT NOT NULL,
    message_id TEXT NOT NULL,
    authorized_by TEXT NOT NULL
);
CREATE INDEX idx_message_purge_audit_message ON message_purge_audit(message_id, id);
