-- Explicit registry project associations only. No inference/backfill from
-- launch-context project IDs, session membership, workflow strings or lineage.
CREATE TABLE workstream_projects (
 workstream_id TEXT NOT NULL REFERENCES workstreams(id),
 project_urn TEXT NOT NULL REFERENCES registry_entries(urn),
 PRIMARY KEY (workstream_id, project_urn)
);
CREATE INDEX idx_workstream_projects_project ON workstream_projects(project_urn, workstream_id);
