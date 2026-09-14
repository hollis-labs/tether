-- 0026_session_ref_parent_item.sql
--
-- CW-20260914-0003: Preserve parent item membership evidence on session_refs.
-- Enables item-wide deduplication and scope in digest roll-ups without
-- altering the captured ref identity or kind/ref_id/relation uniqueness.

ALTER TABLE session_refs ADD COLUMN parent_item_id TEXT;

CREATE INDEX IF NOT EXISTS idx_session_refs_parent_item ON session_refs(parent_item_id);
