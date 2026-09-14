-- 0028_project_correlation_fields.sql
--
-- CW-20260912-0094 (C1): Project correlation entry: derived/authored field split.
-- Adds authored fields (tags, guidelines, entry_points) and per-field provenance/freshness
-- metadata tracking (field_metadata) to registry_entries.
--
-- The split is determinism: derived fields (slug, ids, counts) are deterministically
-- recreatable; authored fields (description, tags, guidelines/taste, entry_points)
-- represent project intent and what a project wants others to know.
--
-- tags_json: JSON array of string tags (e.g. ["agent-setup", "cli"])
-- guidelines: project taste, instructions, constraints, and boundaries (markdown text)
-- entry_points_json: JSON array of start-with pointers/paths
-- field_metadata_json: JSON map of field_name -> {class, last_updated_by, updated_at, cached_at}

ALTER TABLE registry_entries ADD COLUMN tags_json TEXT;
ALTER TABLE registry_entries ADD COLUMN guidelines TEXT;
ALTER TABLE registry_entries ADD COLUMN entry_points_json TEXT;
ALTER TABLE registry_entries ADD COLUMN field_metadata_json TEXT;
