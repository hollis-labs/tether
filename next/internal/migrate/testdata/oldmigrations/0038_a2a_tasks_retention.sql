-- Only terminal tasks expire. Order their age index like the bounded sweep,
-- keeping old non-terminal tasks out of retention scans entirely.
CREATE INDEX idx_a2a_tasks_retention ON a2a_tasks(updated_ns, binding_id, task_id)
WHERE state IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED');
