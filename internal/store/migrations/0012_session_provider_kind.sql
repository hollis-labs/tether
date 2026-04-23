-- ADR 0022 (G3): surface provider_kind on session rows so consumers can
-- branch on runtime family ("cli" | "api") without a separate provider
-- catalog lookup. Populated at CreateSession time; empty for rows created
-- before this migration.
ALTER TABLE sessions ADD COLUMN provider_kind TEXT NOT NULL DEFAULT '';
