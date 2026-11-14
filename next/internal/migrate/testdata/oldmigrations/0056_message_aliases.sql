-- Readable addresses are mutable; message envelope URNs remain stable.
CREATE TABLE message_aliases (
    urn TEXT PRIMARY KEY NOT NULL,
    alias TEXT UNIQUE COLLATE NOCASE NOT NULL
);
