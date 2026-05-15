# ADR 0038: Brokered Progressive Discovery for Skills, Tools, and Context

**Status:** Proposed
**Date:** 2026-05-14
**Deciders:** Tether execution task CW-20260514-0017

## Context

Tether already has the primitives for progressive discovery, but not the
broker-shaped ask surfaces Nanite leans on:

- Skills: `mux_skill_list` enumerates the full catalog and `mux_skill_get`
  loads one skill body from `internal/skills/discovery.go`.
- Tools: `mux_discover` ranks proxied upstream tools with lightweight keyword
  matching, but it is still framed as a catalog search rather than an intent
  broker.
- Context: Tether boot profiles can point at Tesseract/Vanta recall, but the
  caller still has to assemble the right query, namespace, and tags.

Nanite uses broker patterns to hide the full inventory and answer a narrower
question: "what fits this turn?" The closest parity references are:

- Tool broker selection rules in Nanite ADR 023 (`go-toolbroker`-backed
  selection/gating/filtering).
- Nanite `internal/contextbroker.Broker.Fetch`, which queries multiple sources
  against a typed `Intent` plus budget.
- Nanite `internal/contextbroker.DecideAssembly`, which decides what context
  ships this turn instead of always shipping every slot.

Tether needs the same interaction style on the MCP surface, even though its
implementation substrate differs.

## Decision

Add broker-shaped MCP tools that answer targeted discovery requests without
forcing the caller to enumerate everything first.

### 1. Skill Broker

**Tool:** `mux_skill_broker`

**Purpose:** return ranked skill recommendations from the normal layered skill
resolver, with explicit reasoning and no body by default.

**Args**

- `query?: string` — free-text task or intent
- `role?: string` — requester role signal
- `project?: string` — requester project signal
- `task_id?: string` — Torque task identifier for future enrichment
- `triggers?: string` — comma-separated preferred trigger terms
- `layers?: string` — comma-separated layer filter
- `limit?: integer` — default 5, max 20

**Return**

```json
{
  "ok": true,
  "items": [
    {
      "id": "refactor-go",
      "name": "Refactor Go",
      "description": "Apply Go refactoring patterns.",
      "triggers": ["refactor", "cleanup"],
      "path": "/.../skills/refactor-go.md",
      "layer": "project",
      "score": {
        "query_matches": 1,
        "signal_matches": 1,
        "preferred_matches": 2,
        "priority": 10
      },
      "reasons": [
        "matched query: refactor",
        "matched role/project: backend",
        "preferred triggers: refactor"
      ],
      "next": "mux_skill_get"
    }
  ],
  "meta": {
    "returned": 1,
    "total_visible": 42,
    "filters": {
      "query": "refactor handler",
      "role": "backend",
      "project": "nanite",
      "task_id": "CW-20260514-0017",
      "triggers": ["refactor"],
      "layers": ["project"]
    },
    "progressive_discovery": true,
    "task_context_resolved": false
  }
}
```

**Notes**

- `task_id` is accepted now so callers can standardize on one contract. v1 does
  not dereference Torque task metadata in-process; a future Torque-aware
  enricher can populate role/project/tags from that ID.
- The broker intentionally omits `body`; the agent follows up with
  `mux_skill_get` only for the chosen hit.

**Nanite comparison**

- Analogous to Nanite's broker-selected skill surfacing, but exposed as an MCP
  tool rather than an internal boot/session hook.
- Tether's version is read-only and recommendation-oriented; it does not mutate
  an agent's loaded skill set.

### 2. Tool Broker

**Tool:** `mux_tool_broker`

**Purpose:** return ranked tool recommendations rather than raw discovery hits.
Build on the `mux_discover` index and proxy event history.

**Args**

- `intent?: string`
- `role?: string`
- `project?: string`
- `task_id?: string`
- `category?: string`
- `tags?: string`
- `servers?: string`
- `limit?: integer`

**Return**

- Same core shape as `mux_discover` plus `score`, `reasons`, `native`, and
  `next` (`call_direct` vs `mux_call`).
- Optional future `recent_usage` hints from `proxy_events`.

**Nanite comparison**

- Direct analog to Nanite ToolBroker's selection layer.
- Narrower scope than Nanite ADR 023: Tether's MCP broker recommends tools; it
  does not yet gate or filter execution.

### 3. Context Broker

**Tool:** `mux_context_broker`

**Purpose:** fetch the right recall/context bundle for a role + task without
making the agent author raw Tesseract/Vanta recall queries every turn.

**Args**

- `intent: string`
- `query?: string`
- `role?: string`
- `project?: string`
- `task_id?: string`
- `namespace?: string`
- `tags?: string`
- `limit?: integer`
- `token_budget?: integer`

**Return**

```json
{
  "ok": true,
  "items": [
    {
      "source": "tesseract",
      "content": "...",
      "score": 0.82,
      "metadata": {"namespace": "user/chris/memory"}
    }
  ],
  "manifest": {
    "intent": "resume_task",
    "namespace": "user/chris/memory",
    "tags": ["project:tether", "role:backend"],
    "token_budget": 4000
  }
}
```

**Nanite comparison**

- Closest analog is `contextbroker.Broker.Fetch(Intent)` for retrieval and
  `DecideAssembly` for deciding what to ship.
- Tether should keep retrieval and shipping separate: the MCP broker returns
  candidate context; boot/session assembly remains a higher-level concern.

## Consequences

### Positive

- Progressive discovery becomes explicit in the MCP contract instead of an
  emergent behavior from multiple low-level list tools.
- The first broker can ship with almost no new substrate: `mux_skill_broker`
  reuses layered skill discovery and the same trigger/priority ideas already
  present in bootgen's `skill_index`.
- The interfaces leave room for Torque-backed task enrichment without forcing
  that dependency into v1.

### Negative

- v1 `task_id` is forward-compatible but not fully resolved, so some callers
  still need to pass role/project directly.
- Skill and tool brokering use heuristic ranking, not semantic embeddings.

## Implementation Notes

- `mux_skill_broker` lands first and becomes the reference envelope for the
  other broker tools: ranked items, reasons, explicit `next`, and `meta`
  carrying the applied filters.
- Tool Broker should be layered on `internal/mcpadapter/discovery.go`.
- Context Broker should start with a narrow Tesseract/Vanta recall wrapper
  rather than import Nanite's full assembly model into Tether.
