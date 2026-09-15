-- 0029_registry_props.sql
--
-- CW-20260914-0038: Add open, flat props k/v bag to registry_entries.
-- Replaces C1's closed authored-field list as the general mechanism
-- for projects publishing arbitrary authored facts (docs_url, project_root,
-- help_file, inbox, primary_agent, etc.).
--
-- props_json: JSON object mapping string keys to string values

ALTER TABLE registry_entries ADD COLUMN props_json TEXT;
