-- Carried from main: old migration 0103 (CW-20261114-0067, #554); see sqlite/00004_address_aliases.sql.

-- +goose Up
CREATE TABLE address_aliases (
    alias_urn     TEXT COLLATE "C" PRIMARY KEY NOT NULL,
    canonical_urn TEXT COLLATE "C" NOT NULL,
    created_at    TEXT COLLATE "C" NOT NULL,
    created_by    TEXT COLLATE "C" NOT NULL DEFAULT '',
    CHECK (alias_urn <> canonical_urn)
);

CREATE INDEX idx_address_aliases_canonical ON address_aliases(canonical_urn);
