-- 0015_registry.sql
--
-- Federation directory service (v0.6 epic, sprint v060-01). Holds
-- public-identity profiles for agents and projects. The owning substrate
-- retains operational config behind `callback_json`; this table is
-- identity + thin-profile only (D1 two-store model).
--
-- Times are stored as ISO 8601 strings (RFC3339) in UTC, matching the
-- convention from migrations 0001+. DATETIME affinity is documentation
-- of intent; SQLite stores as TEXT regardless.
--
-- FK declarations are DOCUMENTATION per ADR 0008 (FK enforcement
-- deferred). PRAGMA foreign_keys is NOT flipped on at this migration;
-- Go-layer validation at the storage boundary rejects obvious orphans.
-- The reopen task lives in sprint v060-02 T-08 — couple the global flip
-- with the cerberus-bootstrap workload that meaningfully stress-tests
-- cross-substrate FK paths.
--
-- D11 soft-delete only — no hard DELETE FROM registry_entries in v1.
-- D18 no raw payload caching — no cached_payload_json column (substrate
-- ops-store files commonly contain plaintext secrets; caching would
-- leak them into state.db).

CREATE TABLE registry_entries (
    urn              TEXT NOT NULL PRIMARY KEY,
    kind             TEXT NOT NULL CHECK(kind IN ('agent', 'project')),
    mux_instance_id  TEXT NOT NULL DEFAULT 'agent-mux',
    display_name     TEXT NOT NULL,
    title            TEXT,
    role             TEXT,
    description      TEXT,
    avatar           TEXT,
    project          TEXT,
    status           TEXT NOT NULL DEFAULT 'active',
    callback_json    TEXT,
    cached_at        DATETIME,
    health_status    TEXT,
    last_seen_at     DATETIME,
    host_address     TEXT,
    kind_meta_json   TEXT,
    last_updated_by  TEXT,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_registry_entries_kind
    ON registry_entries(kind);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_status
    ON registry_entries(kind, status);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_project
    ON registry_entries(kind, project);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_role
    ON registry_entries(kind, role);

CREATE TABLE registry_capabilities (
    urn         TEXT NOT NULL,
    capability  TEXT NOT NULL,
    PRIMARY KEY (urn, capability),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_registry_capabilities_capability
    ON registry_capabilities(capability);

CREATE TABLE registry_skills (
    urn         TEXT NOT NULL,
    name        TEXT NOT NULL,
    learned_at  DATETIME NOT NULL,
    via         TEXT,
    level       TEXT,
    PRIMARY KEY (urn, name),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_registry_skills_name
    ON registry_skills(name);

CREATE TABLE registry_links (
    urn     TEXT NOT NULL,
    kind    TEXT NOT NULL,
    target  TEXT NOT NULL,
    PRIMARY KEY (urn, kind, target),
    FOREIGN KEY (urn) REFERENCES registry_entries(urn) ON DELETE CASCADE
);
