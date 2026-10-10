-- Execution allocation metadata is separate from the immutable launch plan.
-- Reserved/unknown resources are retained; this table grants no cleanup rights.
CREATE TABLE workspace_scratch_allocations (
    session_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL,
    session_digest TEXT NOT NULL,
    plan_digest TEXT NOT NULL,
    root TEXT NOT NULL,
    physical_identity TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('reserved', 'admitted'))
);
