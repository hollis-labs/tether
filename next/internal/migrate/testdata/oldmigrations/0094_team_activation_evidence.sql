-- Value-free, report-only historical observations. Missing old rows remain unknown.
CREATE TABLE team_activation_evidence (
 kind TEXT NOT NULL,
 evidence_key TEXT NOT NULL,
 payload TEXT NOT NULL,
 recorded_at TEXT NOT NULL,
 PRIMARY KEY(kind,evidence_key)
);
