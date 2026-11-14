-- Durable team storage. Runtime and transport wiring are supplied separately.
CREATE TABLE team_definitions (
    team_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(team_id, version)
);
CREATE TABLE team_runs (
    run_id TEXT PRIMARY KEY,
    session_group_id TEXT NOT NULL UNIQUE REFERENCES session_groups(id),
    team_id TEXT NOT NULL,
    team_version INTEGER NOT NULL CHECK(team_version > 0),
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(team_id, team_version) REFERENCES team_definitions(team_id, version)
);
CREATE TABLE team_rosters (
    run_id TEXT PRIMARY KEY REFERENCES team_runs(run_id),
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL
);
CREATE TABLE team_roster_snapshots (
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    version INTEGER NOT NULL CHECK(version > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(run_id, version)
);
CREATE TABLE team_launch_leases (
    launch_key TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    fence INTEGER NOT NULL CHECK(fence > 0),
    expires_ns INTEGER NOT NULL
);
CREATE TABLE team_launches (
    launch_key TEXT PRIMARY KEY REFERENCES team_launch_leases(launch_key),
    digest TEXT NOT NULL,
    state TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    payload BLOB NOT NULL
);
CREATE TABLE team_launch_journal (
    launch_key TEXT NOT NULL REFERENCES team_launches(launch_key),
    revision INTEGER NOT NULL CHECK(revision > 0),
    fence INTEGER NOT NULL CHECK(fence > 0),
    payload BLOB NOT NULL,
    PRIMARY KEY(launch_key, revision)
);
CREATE TABLE team_phase_signals (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    phase_id TEXT NOT NULL,
    actor TEXT NOT NULL,
    payload BLOB NOT NULL,
    UNIQUE(run_id, phase_id, actor)
);
CREATE TABLE team_signal_resolutions (
    run_id TEXT NOT NULL REFERENCES team_runs(run_id),
    phase_id TEXT NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY(run_id, phase_id)
);
