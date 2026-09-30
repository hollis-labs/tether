-- 0032_a2a_tasks.sql
--
-- Durable task records for the inbound A2A relay (internal/a2aadapter).
--
-- The A2A SDK's default task store is in-memory, so every delegated task an
-- external peer had created vanished on a daemon restart: tasks/get answered
-- "not found" for a task the peer had just been told about. The adapter now
-- keeps the SDK's task record here instead.
--
-- One row per (binding, task). The task is scoped to its binding because each
-- binding has always had its own store, so a peer of one binding has never
-- been able to read another binding's tasks; a single shared table must not
-- widen that. task_json is the SDK's own JSON encoding of the task and is
-- opaque to this schema; state, context_id and owner are lifted out so the
-- restart sweep and tasks/list can filter without decoding every row. owner is
-- the SDK's task-owner identity ('' while the adapter runs without an
-- authenticator). version is the SDK's optimistic-concurrency counter.
-- updated_ns is unix nanoseconds, the list ordering key.
--
-- A task's in-flight wait (the Execute call blocked on a consumer transition)
-- is process-local and is not, and cannot be, persisted; on startup the
-- adapter moves tasks left in submitted or working to failed so none is left
-- looking alive.

CREATE TABLE IF NOT EXISTS a2a_tasks (
    binding_id TEXT    NOT NULL,
    task_id    TEXT    NOT NULL,
    owner      TEXT    NOT NULL DEFAULT '',
    context_id TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL,
    version    INTEGER NOT NULL,
    task_json  TEXT    NOT NULL,
    created_ns INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL,
    PRIMARY KEY (binding_id, task_id)
);

CREATE INDEX IF NOT EXISTS idx_a2a_tasks_state ON a2a_tasks(state);
