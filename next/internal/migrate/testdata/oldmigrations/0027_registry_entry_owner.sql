-- 0027_registry_entry_owner.sql
--
-- CW-20260912-0052: Add minter/owner provenance to registry_entries.
-- Distinguishes which substrate or minter created the row (e.g. 'cerberus', 'tether', 'loom').
-- Advisory metadata for reconciliation and deduplication (the minter owns the row),
-- distinct from registry_external_ids (which records external aliases/links).

ALTER TABLE registry_entries ADD COLUMN owner TEXT;

CREATE INDEX IF NOT EXISTS idx_registry_entries_owner
    ON registry_entries(owner);
