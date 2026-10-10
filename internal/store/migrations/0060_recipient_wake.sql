-- Mail delivery and wake admission are separate. A replay of the same durable
-- message never re-runs recipient wake admission, even across daemon restarts.
CREATE TABLE recipient_wake_admissions (
    message_id TEXT PRIMARY KEY NOT NULL,
    admitted_at TEXT NOT NULL,
    outcome_json TEXT
);
