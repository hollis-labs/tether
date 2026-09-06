-- 0021_group_fanout_and_scoped_bindings.sql
--
-- Messaging vNext T04 (CW-20260906-0035, plan CW-20260906-0023). Two
-- additive pieces:
--
--   1. messages.delivery_message_id: for a GROUP post, the delivery core's
--      own Message.ID for the fanout obligation created alongside the
--      canonical room-body row. Distinct from the existing
--      messages.delivery_id (T03, migration 0020), which names a single
--      RecipientDelivery -- a group post fans out to N recipients under
--      ONE delivery-core Message, so the mapping needed here is at the
--      Message level, looked up via delivery.Store.ListDeliveries(Filter{
--      MessageID: ...}) to enumerate the N per-member obligations. 1:1
--      sends never populate this column (messages.id already equals the
--      delivery core's Message.ID for those, by construction in T03 --
--      no separate mapping needed). NULL for every row until T04's Go
--      code starts populating it, and NULL forever for messages that
--      never had group members to fan out to.
--
--   2. scoped_role_bindings: the architecture's "narrow scoped-address
--      binding primitive" -- a consumer-owned scope plus role/slot name
--      maps to participant URNs (e.g. scope="torque:sprint:CW-20260906-0023"
--      slot="reviewer"). This is DELIBERATELY DISTINCT from ADR-0042's
--      group owner/moderator/member permission roles and from
--      registry_links (which ADR-0042 already ruled out for anything
--      carrying per-member state) -- it is pure resolution/mapping, no
--      workflow, staffing, activation or command authority (architecture:
--      "Saved teams, staffing counts, durable-versus-fresh activation,
--      phases, approval gates, semantic routing... remain consumer-owned").
--      revision is a monotonic counter per (scope, slot) so binding
--      resolution has full provenance and old bindings remain queryable
--      history rather than being overwritten -- "Binding resolution
--      provenance explains scope/revision/targets" and "Freeze resolved
--      recipients for an accepted fanout delivery... later rebinding must
--      not move a retry to a different actor" both require this.
--
-- FK declarations follow ADR-0008 (deferred enforcement) -- documentation
-- only; PRAGMA foreign_keys stays off, matching every migration since 0015.

ALTER TABLE messages ADD COLUMN delivery_message_id TEXT;

CREATE TABLE scoped_role_bindings (
    id                TEXT PRIMARY KEY,
    scope             TEXT NOT NULL,
    slot              TEXT NOT NULL,
    revision          INTEGER NOT NULL,
    target_urns_json  TEXT NOT NULL,
    relationship_json TEXT,
    created_by        TEXT,
    created_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_scoped_role_bindings_scope_slot_revision
    ON scoped_role_bindings(scope, slot, revision DESC);
