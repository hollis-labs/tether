-- Retry scheduling is separate from immutable launch journals and port receipts.
CREATE TABLE team_recovery_attempts (
 kind TEXT NOT NULL CHECK(kind IN ('session','enrollment','formation')),
 intent_key TEXT NOT NULL,
 mode TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 7),
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 error_hash TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(kind,intent_key)
);
