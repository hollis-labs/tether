-- Secret-free effective MCP authority, captured before credential delivery.
-- Old sessions deliberately have no inferred policy and must resume/relaunch.
CREATE TABLE session_mcp_policy (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    policy_json TEXT NOT NULL
);
