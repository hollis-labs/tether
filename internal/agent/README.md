# internal/agent

Durable agent identity — the `LogicalAgent` type.

**Purpose:** separates the identity of an agent (name, role, policies,
responsibilities, capabilities) from the ephemeral sessions that embody it.
Sessions reference a `logical_agent_id`; multiple sessions can run against
one logical agent over time.

**Entry points:**

- `LogicalAgent` struct (`model.go`) — mirrors the `logical_agents` table.
  Fields match context-pack §02 (id, role, name, responsibilities,
  capabilities, memory_scopes, policies, permitted_tools, escalation_rules,
  checkpoint_policy, hot_cold_policy, created_at, updated_at).

**Neighbors:**

- Written by [`internal/store`](../store) — per-entity CRUD lives there.
- Seeded by [`internal/app`](../app) at startup from the catalog. See
  [ADR 0003](../../docs/adr/0003-logical-agents-seeding.md).
- Referenced by session rows and by [`internal/runtime`](../runtime)
  (SessionInfo.LogicalAgentID + event filter keys).

**Gotchas:**

- Types-only package by design. No business logic here — seeding, lookup,
  upsert, and any cross-entity invariants live in `store` + `app`.
- Pre-launch, the catalog's agent ID and the logical agent ID are the
  same string. A later version may decouple them; don't assume they stay
  unified.
