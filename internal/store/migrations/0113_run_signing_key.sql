-- Launcher-only signing material shares the state DB's credential protection.
-- No run token or token-bearing launch environment is stored here.
CREATE TABLE run_signing_key (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    private_key BLOB NOT NULL CHECK (length(private_key) = 64)
);
