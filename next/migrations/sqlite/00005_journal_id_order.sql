-- message_event_journal ids commit in order: a Postgres-only lock (postgres/00005_journal_id_order.sql).
-- SQLite has one writer, so journal ids already commit in order. This placeholder keeps the two sets
-- numbered alike.

-- +goose Up
SELECT 1;

