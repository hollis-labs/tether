-- Context delivery cursors are independent of mailbox acknowledgements and
-- routing delivery leases. Recovery never consumes or replays those messages.
CREATE TABLE agent_recovery_cursors (
    agent_id TEXT NOT NULL,
    channel TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK(sequence >= 0),
    PRIMARY KEY(agent_id, channel)
);
