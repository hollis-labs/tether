-- messages.seq: an explicit, monotonic message sequence (port plan section 13.1). The mailbox watch cursor
-- reads seq instead of SQLite's implicit rowid, so the cursor values clients hold stay valid:
-- migrate-from writes seq = the old rowid for every migrated row (internal/migrate.VerifyMessagesSeq
-- proves it), and the database assigns MAX(seq)+1 to a new row, as rowid assignment did. Postgres
-- assigns it from an identity column (postgres/00002_messages_seq.sql).

-- +goose Up
ALTER TABLE messages ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

UPDATE messages SET seq = rowid;

CREATE UNIQUE INDEX idx_messages_seq ON messages(seq);

-- +goose StatementBegin
CREATE TRIGGER messages_assign_seq AFTER INSERT ON messages
WHEN NEW.seq = 0
BEGIN
    UPDATE messages SET seq = (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages) WHERE rowid = NEW.rowid;
END;
-- +goose StatementEnd
