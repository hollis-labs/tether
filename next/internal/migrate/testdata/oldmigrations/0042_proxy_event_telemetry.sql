-- Compatibility query projection for telemetry v2. Canonical call payloads
-- continue to be persisted on the events bus and share its age retention.
ALTER TABLE proxy_events ADD COLUMN telemetry_json TEXT NOT NULL DEFAULT '{}';
