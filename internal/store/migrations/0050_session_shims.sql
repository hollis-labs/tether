-- Placement metadata is additive and secret-free. Capabilities stay in private
-- descriptor files; TEXT authority counters preserve the full uint64 range.
CREATE TABLE IF NOT EXISTS session_shims (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 shim_key TEXT NOT NULL UNIQUE,
 host_backend TEXT NOT NULL CHECK(host_backend IN ('detached','systemd-user')),
 unit_name TEXT NOT NULL DEFAULT '',
 socket_path TEXT NOT NULL,
 descriptor_path TEXT NOT NULL,
 journal_id TEXT NOT NULL DEFAULT '',
 runtime TEXT NOT NULL,
 runtime_generation TEXT NOT NULL,
 boot_generation TEXT NOT NULL,
 controller_epoch TEXT NOT NULL DEFAULT '0',
 last_committed_cursor TEXT NOT NULL DEFAULT '',
 inject_counter TEXT NOT NULL DEFAULT '0',
 host_pid INTEGER NOT NULL DEFAULT 0,
 shim_pid INTEGER NOT NULL DEFAULT 0,
 provider_pid INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
