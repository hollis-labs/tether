-- 0013_messages_inbox_state.sql
--
-- Real inbox semantics for the messages table (CW-20260517-0003).
--
-- Adds explicit read state and a soft-delete archive flag so a UI can
-- list, read, and archive messages idempotently and repeatably WITHOUT
-- consuming them. The destructive Inbox() atomic-delivery pull model
-- (delivered_at) is unchanged — see ADR-0023 §4.
--
-- Both columns are nullable and additive: existing rows backfill to NULL,
-- meaning unread and not-archived. No data loss.
--
--   read_at      non-NULL once a human/agent has viewed the message in a
--                UI. Distinct from delivered_at (handed to a consumer)
--                and consumed_at (finished processing). Set via MarkRead.
--   archived_at  non-NULL once the recipient soft-deletes the message.
--                Archived rows are excluded from default List results.

ALTER TABLE messages ADD COLUMN read_at TEXT;
ALTER TABLE messages ADD COLUMN archived_at TEXT;

-- Supports the non-destructive List query: recipient scan, archived split,
-- newest-first ordering.
CREATE INDEX IF NOT EXISTS idx_messages_to_archived
    ON messages(to_urn, archived_at, created_at);
