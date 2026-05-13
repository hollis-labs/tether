# Agent Mux Roadmap

## What Agent Mux is

Agent Mux is the local runtime and session control plane for agentic systems. It owns launching, process/PTY ownership, attach/detach, workspace materialization, checkpoints, brokered messaging, and event streams. Other systems (Nanite for chat UX, Clockwork for planning, Agent Ops for assets, Vanta for memory, Cerberus for service management) are clients of this runtime, not owners of it.

This roadmap covers the evolution from the v0.0.1 launcher foundation into a durable, attachable, API-exposed runtime suitable for multi-client use (CLI, TUI, Nanite, Clockwork, future MCP/desktop surfaces).

## Version timeline

| Version | Status | Theme | One-line summary |
|---------|--------|-------|------------------|
| v0.0.1 | shipped | Foundation | Rough-but-functional Go CLI: config/launch/workspace/provider/PTY/SQLite glued through a service facade. |
| v0.0.2 | shipped | Runtime foundation | Daemon/runtime ownership, live attach, input injection, LogicalAgent split, checkpoint/broker skeletons, local API, event bus. |
| v0.0.3 | shipped (partial) | TUI MVP | Sprints 1–2 shipped: scaffold + launcher + detail views + palette + live attach. Sprints 3–6 (CRUD / wizard / runtime-concept CRUD / polish) re-slotted to v0.1. |
| v0.0.4 | shipped | Dogfood-Ready Infrastructure | PTY fidelity, claude stream-json provider, sandboxing, checkpoint resume, workspace prune, and dogfood-grade session-running primitives. Launch Wizard was deferred because backend foundation outranked TUI polish. |
| v0.0.5 | shipped / stabilizing | MCP + Provider Integration | Agent-facing MCP stdio adapter, go-messaging integration, MCP proxy aggregation + observability, opencode provider, and boot-tab quicklaunch. This is the inflection point: agents can now see and use mux/Clockwork/portfolio tools through one MCP surface. |
| v0.1 | next | Session Routing + Provider Surface | Shared session/message substrate: Clockwork, Nanite, and other systems can opt into Mux-managed sessions when they want routing, messaging, attach/resume, and provider abstraction, while retaining their own execution policy. |
| v0.2 | later | Agent Definition + Asset Resolution | Asset/catalog references, Agent Ops lookup, skills/agents handoff, local/global/project scope, cross-ecosystem import/export. |
| v0.3+ | deferred | Rich UX | Richer TUI, Nanite GUI control plane, workflow visualization, multiplexor flows, observability dashboards, Cerberus service-manager glue. |

### Parked

| Theme | Former slot | Notes |
|-------|-------------|-------|
| Integration Foundation | v0.0.3 (2026-04-19) | Mostly superseded by shipped work: checkpoint resume, mailbox/go-messaging, MCP server/proxy, opencode provider, and Nanite POC have landed or moved downstream. Remaining reusable theme is the API-backed provider, which should be re-evaluated against `go-providers` before scheduling. Epic + historical sprints live in [docs/parked/](./parked/). |

> **Historical note:** The `(v0.0.3)` references in shipped v0.0.1 / v0.0.2 epic and sprint files refer to the **Integration Foundation** theme, which was renumbered to parked on 2026-04-19. `v0.0.3` now names TUI MVP. When reading historical docs, disambiguate against [docs/parked/v0.0.3-integration-foundation.md](./parked/v0.0.3-integration-foundation.md).

## Epics

| Epic | Status | File |
|------|--------|------|
| v0.0.1 — Foundation (retrospective) | shipped | [epics/v0.0.1-foundation.md](epics/v0.0.1-foundation.md) |
| v0.0.2 — Runtime Foundation | shipped | [epics/v0.0.2-runtime-foundation.md](epics/v0.0.2-runtime-foundation.md) |
| v0.0.3 — TUI MVP | shipped (partial) | [epics/v0.0.3-tui-mvp.md](epics/v0.0.3-tui-mvp.md) |
| v0.0.4 — Dogfood-Ready Infrastructure | shipped | [epics/v0.0.4-dogfood-ready-infrastructure.md](epics/v0.0.4-dogfood-ready-infrastructure.md) |
| v0.0.5 — MCP + Provider Integration | shipped / stabilizing | [sprints/v005-01-mcp-stdio-adapter.md](sprints/v005-01-mcp-stdio-adapter.md) + Clockwork sprints `SP-20260421-0002` / `SP-20260421-0003` |
| v0.1 — Session Routing + Provider Surface | next | [epics/v0.1-operational-agents.md](epics/v0.1-operational-agents.md) |
| v0.2 — Agent Definition + Asset Resolution | later | [epics/v0.2-agent-ops-integration.md](epics/v0.2-agent-ops-integration.md) |
| v0.3+ — Rich UX & Advanced Orchestration | deferred | [epics/v0.3-rich-ux.md](epics/v0.3-rich-ux.md) |
| [Parked] — Integration Foundation | parked | [parked/v0.0.3-integration-foundation.md](parked/v0.0.3-integration-foundation.md) |

## Current planning read — 2026-04-22

The roadmap has evolved from "make mux a durable local runtime" to "make mux the shared session/routing substrate that other systems can opt into." The high-leverage unlock is not making Clockwork, Nanite, or any other app depend on Mux. It is giving them a common provider surface and message path when they want deeper session integration.

- **Clockwork owns task execution.** Clockwork can run work through its built-in scheduler/runner and direct providers, or choose Mux as another provider/session substrate when it wants Mux-managed sessions, routing, attach/resume, and messaging. Clockwork owns orchestration agents, task review agents, project/task review, queuing, dependency policy, PR/close/escalation behavior, and task state transitions.
- **Agent Mux provides sessions and routing.** Mux owns session environment creation, provider adapters, attach/resume/checkpoints, go-messaging, MCP-native session/message/catalog tools, MCP proxy aggregation, and event streams. It is not the orchestrator and not Clockwork's executor policy engine.
- **Nanite connects, interacts, and observes.** Nanite remains the human chat/GUI surface and is also building its Mux plugin. That plugin should become the conversational/connectivity bridge into Mux sessions, messages, tools, and subordinate-agent workflows, while Mux remains the runtime/session owner.
- **Vanta informs.** Memory/preference context belongs in Vanta, injected into boot/resume/orchestrator context rather than stored as mux-native memory logic.

Near-term roadmap priority should therefore be:

1. **Make Mux a clean opt-in provider for Clockwork:** Clockwork can launch an orchestrator or worker through Mux when it wants Mux sessions/routing, but can still use claude, opencode, or any other provider directly.
2. **Make messaging/routing boring:** request/reply, inbox, thread, consume, correlation, and addressing semantics need to be stable enough for Clockwork orchestrators, Nanite plugins, and agents to rely on them.
3. **Make the event spine useful without owning policy:** tool-call events, session lifecycle events, message/request events, and session outcomes should be observable/queryable; Clockwork decides what those signals mean.
4. **Design for Nanite's Mux plugin as a first-class consumer:** keep Mux APIs stable, typed, and plugin-friendly for Nanite connectivity: session list/detail, attach/event streams, message/request/reply, launch/resume/abort, and tool discovery.
5. **Promote agent/skill handoff deliberately:** Nanite folder-drop/DB ingestion and Agent Ops catalog resolution should feed mux launch profiles, not become mux-owned installer logic.
6. **Defer low-leverage TUI polish:** keep only fixes that unblock session/routing dogfooding; richer UX belongs after the shared contracts prove themselves.

## Organizational conventions

### File layout

```
docs/
  roadmap.md                 # this file — top-level index
  epics/
    v<version>-<slug>.md     # one epic per version, scope + sprint list
  sprints/
    v<ver>-<nn>-<slug>.md    # one sprint per milestone, task breakdowns
  parked/
    <epic + sprint files>    # epics and their sprints that are paused out of sequence
  agent-mux-vfuture-context-pack/
    00-readme.md … 12-boot-prompt.md
  superpowers/plans/
    2026-04-18-agent-mux-v0.md  # the v0.0.1 implementation plan (shipped)
```

### Hierarchy

- **Roadmap** (this doc): one paragraph per version, links to epics.
- **Epic**: one Markdown file per version. Declares scope, motivation, exit criteria, and links the sprints that deliver it.
- **Sprint**: one Markdown file per milestone. Contains multiple tasks, each following the task template skeleton.
- **Task**: a section inside a sprint file. Not a separate file. It has a stable ID so execution agents and trackers can reference it.

### ID format

- **Sprint ID:** `v<ver>-<nn>-<slug>`. Example: `v002-01-daemon-runtime`. `ver` is compact (`002` = 0.0.2), `nn` is zero-padded two digits.
- **Task ID:** `T-v<ver>-s<sprint>-<n>`. Example: `T-v002-s01-03` = v0.0.2, sprint 01, task 03. `n` is zero-padded two digits.

### Task description skeleton

Every task in a sprint file follows this skeleton (adapted from `/Users/chrispian/Projects-apps/agent-workspaces/knowledge/templates/task-create-template.md`):

- **Problem** — what's wrong today / what's missing
- **Evidence** — concrete current-state evidence (skip for net-new features)
- **Fix direction** — preferred path, not full spec
- **Files** — real paths (with line numbers where known)
- **Acceptance criteria** — binary pass/fail bullets
- **Test plan** — which tests to add, what to verify manually
- **Scope fences** — what NOT to do (at least 2 bullets)
- **Relationship** — sibling/blocking task IDs
- **Origin** — which context-pack doc motivated this

Every task is captured as `manual=true`. A separate readiness review (not covered here) closes gaps before dispatch to a scheduler.

## How to pick up a task

An execution agent should:

1. Open `docs/roadmap.md`, identify the active epic.
2. Open the epic file, pick a sprint from its sprint list.
3. Open the sprint file, pick a task by ID based on priority and dependencies declared in the **Relationship** field.
4. **Before touching code**, re-read the referenced context-pack doc (listed under **Origin**) and the real code paths under **Files**. Capture is intentionally lightweight — expect to enrich the task description during a readiness pass before running it.
5. If the task's Acceptance criteria feel ambiguous or File pointers are stale, surface that as a readiness gap and pause; don't silently improvise.
6. Respect Scope fences — if you find scope creep tempting, write a new task rather than expanding the current one.

## Cross-cutting anti-goals (from context pack)

- Do not build a GUI first.
- Do not couple the runtime to one vendor CLI.
- Do not turn Agent Mux into Clockwork (planning/orchestration) or Agent Ops (asset installers).
- Do not embed memory logic that belongs in Vanta.
- Do not create a giant monolithic `app` package that owns everything forever.
- Do not rewrite v0 from scratch; evolve it.
