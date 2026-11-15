-- Operator-attested replies (CW-20261114-0152, docs/operator-attested-replies.md).

-- Single-use challenges Tachyon asks for and Tangent signs over.
CREATE TABLE operator_reply_challenges (
    challenge_id TEXT PRIMARY KEY NOT NULL,
    nonce_sha256 TEXT NOT NULL,
    message_id   TEXT NOT NULL,
    option       TEXT NOT NULL,
    binding      TEXT NOT NULL,
    body_sha256  TEXT NOT NULL,
    device_id    TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    consumed_at  TEXT
);
CREATE INDEX idx_operator_reply_challenges_device ON operator_reply_challenges(device_id, consumed_at, expires_at);

-- One-use proofs: a jti or elevation_id is spent once, by the reply-time
-- transaction; UNIQUE is the replay guard against two concurrent replies.
CREATE TABLE operator_reply_proofs (
    jti          TEXT NOT NULL UNIQUE,
    elevation_id TEXT NOT NULL UNIQUE,
    challenge_id TEXT NOT NULL,
    message_id   TEXT NOT NULL,
    created_at   TEXT NOT NULL
);

-- One attested answer per parent: reserved in the same transaction as the
-- proofs and the already-answered re-check, released only if storing the
-- reply then fails.
CREATE TABLE operator_reply_answers (
    message_id   TEXT PRIMARY KEY NOT NULL,
    challenge_id TEXT NOT NULL,
    reply_id     TEXT,
    created_at   TEXT NOT NULL
);

-- The approved trust root: the digest of the operator_attestation block an
-- operator-approved plan applied. A catalog whose block has another digest is
-- refused. One row.
CREATE TABLE operator_attestation_trust (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    digest       TEXT NOT NULL,
    key_ids_json TEXT NOT NULL,
    plan_id      TEXT NOT NULL,
    approved_at  TEXT NOT NULL
);

-- Structured detail for audit rows that need it (the attested-reply device,
-- parent, challenge, elevation and refusal code; old and new trust-root keys).
ALTER TABLE identity_audit ADD COLUMN detail TEXT;
