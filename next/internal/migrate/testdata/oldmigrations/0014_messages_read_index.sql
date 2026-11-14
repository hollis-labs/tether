-- 0014_messages_read_index.sql
--
-- Index supporting the non-destructive List query's unread filter
-- (CW-20260517-0003). The UnreadOnly path scans messages by recipient
-- with read_at IS NULL; this composite index lets SQLite satisfy the
-- (to_urn, read_at) predicate without a table scan.
--
-- Additive and idempotent: IF NOT EXISTS makes re-runs safe. Complements
-- idx_messages_to_archived from migration 0013.

CREATE INDEX IF NOT EXISTS idx_messages_to_read ON messages(to_urn, read_at);
