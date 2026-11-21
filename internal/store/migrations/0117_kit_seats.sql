-- CW-20261121-0114: external kit-booted seats registered through
-- POST /correlation/register, kept locally so tether_agents_live can list them.
-- Descriptive facts only; no credentials. One row per (agent_urn, session).
CREATE TABLE kit_seats (
    agent_urn           TEXT NOT NULL,
    session             TEXT NOT NULL,
    run_id              TEXT NOT NULL DEFAULT '',
    task_id             TEXT NOT NULL DEFAULT '',
    workstream_id       TEXT NOT NULL DEFAULT '',
    host                TEXT NOT NULL DEFAULT '',
    display_name        TEXT NOT NULL DEFAULT '',
    summary             TEXT NOT NULL DEFAULT '',
    provider_session_id TEXT NOT NULL DEFAULT '',
    first_seen_at       TEXT NOT NULL,
    last_seen_at        TEXT NOT NULL,
    PRIMARY KEY (agent_urn, session)
);
CREATE INDEX kit_seats_last_seen ON kit_seats(last_seen_at);
