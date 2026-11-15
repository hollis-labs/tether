-- Carried from main: old migration 0103 (CW-20261114-0067, #554). alias_urn is another name for
-- canonical_urn's mailbox; one hop only. migrate-from copies the table as it is (no transform).

-- +goose Up
CREATE TABLE address_aliases (
    alias_urn     TEXT PRIMARY KEY NOT NULL,
    canonical_urn TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    created_by    TEXT NOT NULL DEFAULT '',
    CHECK (alias_urn <> canonical_urn)
);

CREATE INDEX idx_address_aliases_canonical ON address_aliases(canonical_urn);
