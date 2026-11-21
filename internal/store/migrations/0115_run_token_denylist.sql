-- Exact jti or execution run identifiers, never token contents. Retained across
-- daemon restarts; revoking a run also denies future proofs with that run id.
CREATE TABLE run_token_denylist (
 target TEXT PRIMARY KEY CHECK(length(target) BETWEEN 1 AND 512),
 revoked_at TEXT NOT NULL
);
