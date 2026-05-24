-- 0017_registry_external_ids.sql
--
-- v060-02 cross-substrate registry dedup. Adds:
--   1. registry_entries.merged_into tombstone pointer for manual merge cleanup
--   2. registry_external_ids sibling table for substrate-local identifiers

ALTER TABLE registry_entries
    ADD COLUMN merged_into TEXT REFERENCES registry_entries(urn) ON DELETE SET NULL;

CREATE TABLE registry_external_ids (
    urn         TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    substrate   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    attached_at DATETIME NOT NULL,
    PRIMARY KEY (urn, substrate)
);

CREATE INDEX IF NOT EXISTS idx_registry_external_ids_lookup
    ON registry_external_ids (substrate, external_id);
