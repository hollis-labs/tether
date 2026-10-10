-- Bindings and retirement tombstones are retained; there is no delete/move path.
CREATE TABLE environment_directory (
    environment_id TEXT PRIMARY KEY NOT NULL,
    authority TEXT UNIQUE NOT NULL,
    record_json TEXT NOT NULL
);
CREATE TABLE environment_agent_homes (
    urn TEXT PRIMARY KEY NOT NULL,
    environment_id TEXT NOT NULL,
    authority TEXT NOT NULL CHECK (authority IN ('environment', 'hub')),
    management_mode TEXT NOT NULL CHECK (management_mode IN ('', 'INDEPENDENT', 'HUB-MANAGED'))
);
