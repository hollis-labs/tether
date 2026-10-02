CREATE TABLE principals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    principal_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('operator', 'session', 'service', 'interactive')),
    display TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE CHECK(length(token_hash) = 64),
    scopes_json TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    addresses_json TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    revoked_at TEXT,
    expires_at TEXT
);
CREATE INDEX idx_principals_identity ON principals(principal_id);
CREATE INDEX idx_principals_session ON principals(session_id);
CREATE TABLE identity_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at TEXT NOT NULL,
    principal_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL,
    authentication TEXT NOT NULL,
    method TEXT NOT NULL,
    route TEXT NOT NULL
);

CREATE INDEX idx_identity_audit_at ON identity_audit(at);
