-- 0030_settings.sql
--
-- CW-20260914-0042: Global > Project > User settings cascade.
-- Dedicated settings table for onboarding and deployment configuration,
-- distinct from launch-time catalog configuration.
--
-- scope: 'global', 'project', 'user'
-- scope_id: empty for global, project URN/ID for project, user URN/ID for user
-- key: setting identifier (e.g. 'onboarding', 'required_props', 'mcp_opt_in_offered')
-- value_json: JSON encoded value

CREATE TABLE IF NOT EXISTS settings (
    scope TEXT NOT NULL,
    scope_id TEXT NOT NULL DEFAULT '',
    key TEXT NOT NULL,
    value_json TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (scope, scope_id, key)
);

CREATE INDEX IF NOT EXISTS idx_settings_scope
    ON settings(scope, scope_id);
