-- Address aliases (CW-20261114-0067): alias_urn is another name for
-- canonical_urn's mailbox. Stored envelopes are never rewritten; mailbox reads
-- match every form. One hop only: a canonical address is never itself an alias.
CREATE TABLE address_aliases (
    alias_urn     TEXT PRIMARY KEY NOT NULL,
    canonical_urn TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    created_by    TEXT NOT NULL DEFAULT '',
    CHECK (alias_urn <> canonical_urn)
);
CREATE INDEX idx_address_aliases_canonical ON address_aliases(canonical_urn);
