-- Carried from main: old migration 0102 (CW-20261114-0007, #559). A worker row superseded by a newer
-- enrollment of the same label is retired, not deleted: ended sessions, mail grants and events still name
-- its id. migrate-from copies both columns as they are (no transform).

-- +goose Up
ALTER TABLE workers ADD COLUMN retired_at TEXT;

ALTER TABLE workers ADD COLUMN superseded_by TEXT;
