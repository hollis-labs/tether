-- Only identity authorization lives here, never adapter handles or secrets.
CREATE TABLE worker_mail_grants (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    urn TEXT NOT NULL,
    PRIMARY KEY (worker_id, urn)
);
