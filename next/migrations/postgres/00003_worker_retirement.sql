-- Carried from main: old migration 0102 (CW-20261114-0007, #559); see sqlite/00003_worker_retirement.sql.

-- +goose Up
ALTER TABLE workers ADD COLUMN retired_at TEXT COLLATE "C";

ALTER TABLE workers ADD COLUMN superseded_by TEXT COLLATE "C";
