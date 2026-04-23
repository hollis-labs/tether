# ADR 0022: Agent Mux as an Optional Provider/Session Substrate

**Status:** Accepted — 2026-04-22
**Context:** v0.0.3 pre-implementation — provider surface contract
**Deciders:** agent-mux v0.0.3 execution session; task CW-20260423-0016
**Supersedes:** none. Extends ADR 0006 (provider contract), ADR 0009 (create/launch split), ADR 0010 (typed error envelope).

---

## Context

Agent Mux is a local session substrate: it resolves launch profiles, materialises
workspaces, spawns provider runtimes, routes output to attach subscribers, and
persists lifecycle events. It does **not** own task execution policy, sprint
scheduling, orchestration, or review/PR/close flows.

As v0.0.3 integration work begins, two external consumers are ready to adopt Mux:

1. **Clockwork** — task execution scheduler that may delegate agent worker
   sessions to Mux as one of several provider paths.
2. **Nanite** — developer agent that accesses Mux through an MCP plugin to list,
   inspect, attach to, and send input to sessions.

Both consumers share the same HTTP (daemon) and MCP (stdio adapter) surfaces.
The contract between Mux and its consumers was previously implicit — emergent
from handler code and `SessionDTO` field names. This ADR makes it explicit before
implementation changes land, so breaking changes require a named ADR and consumers
can program to a stable contract.

---

## Ownership Table

| Concern | Owner | Notes |
|---|---|---|
| Session environment (workspace, env vars, log path) | **Mux** | Resolved from catalog at CreateSession time |
| Session lifecycle (create → launch → running → terminal) | **Mux** | State machine in `internal/session/state.go`, driven by `runtime.Manager` |
| Routing metadata (provider_id, launch_id, logical_agent_id) | **Mux** | Resolved from catalog, stored on the session row |
| Task execution policy (which task runs, when, on what sprint) | **Clockwork** | Clockwork calls Mux as a provider path; Mux never reads Clockwork task state |
| Sprint selection, orchestration, review, PR, close, escalation | **Clockwork** | Out of scope for Mux entirely |
| Plugin UX, connectivity, MCP client wiring | **Nanite** | Nanite consumes Mux MCP tools; Mux does not know about Nanite's UI layer |
| Asset installation, skill management | **Agent Ops** | Out of scope for Mux |
| Memory, context, semantic recall | **Vanta** | Vanta supplies boot context via boot-profile slots; Mux injects it as BootPrompt |

---

## Decision

Define a stable, versioned provider/session contract consisting of four layers:

1. **Launch Profile Inputs** — the catalog-level inputs that determine what session is created.
2. **Boot Prompt Payload** — the structured text injected as the agent's initial context.
3. **Session Identity** — the durable IDs that anchor a session to its launch and logical agent.
4. **Lifecycle States and Terminal Outcome** — the state machine and its observable events.
5. **Attach/Read Behavior** — how consumers stream or replay session output.
6. **Checkpoint/Resume Relationship** — how logical agent continuity crosses sessions.
7. **Typed Error Envelope** — the shape of all error responses.

Each layer is specified below, with current field status (required / optional / missing / provider-specific).

---

## Layer 1: Launch Profile Inputs

A **launch profile** is the catalog record that binds a project, agent, and provider
together. Consumers reference a launch profile by ID; Mux resolves the full plan
internally.

### Current fields (catalog `launches/*.yaml`)

```yaml
id: string                  # required — opaque slug, referenced by create calls
project: string             # required — project ID (resolves repo_root, workspace config)
agent: string               # required — agent ID (resolves boot fragments, permissions)
provider: string            # required — provider ID (resolves runtime kind + command)
workspace:
  mode: string              # optional — "hybrid" | "shared" | "isolated"; default: hybrid
prompt:
  include_project_boot: bool   # optional; default true
  include_agent_boot: bool     # optional; default true
  include_knowledge_base: bool # optional; default false
overrides: {}               # optional — freeform overrides forwarded to the runtime
```

### Create call (HTTP and MCP)

```
POST /sessions
  Body: { "launch": "<launch_id>" }              # HTTP daemon
  Tool: mux_session_create(launch_id, boot_prompt?) # MCP adapter
```

`boot_prompt` (MCP) / `boot_prompt` override (service method `CreateSessionWithBootPrompt`)
replaces the catalog's assembled boot fragments entirely. Consumers that assemble
their own boot text (e.g. Nanite using `mux_boot_generate`) inject it here.

**Missing field (gap):** No consumer-supplied metadata bag on `POST /sessions`.
Clockwork may want to tag the session with `task_id`, `sprint_id`, or `workflow_id`
at create time without waiting for a follow-up checkpoint write. Implementation gap:
add `metadata map[string]string` to the HTTP/MCP create request body and persist it
on the session row.

---

## Layer 2: Boot Prompt Payload

A boot prompt is an opaque UTF-8 string injected into the session as its initial
context. Mux does not interpret or validate its content.

### Assembly (current)

Boot fragments are concatenated from:
1. Project boot files (`project.boot_fragments[]` → catalog paths)
2. Agent boot files (from `agent` catalog record)
3. Optional knowledge-base fragments
4. Any override provided at create time

### Dynamic assembly via boot-profile

The `mux_boot_generate` MCP tool assembles a prompt from a YAML boot-profile,
which can include static file content, shell command output, and HTTP endpoint
responses. The assembled string is then passed as `boot_prompt` to
`mux_session_create`.

### Delivery modes

| `boot_mode` | Behaviour |
|---|---|
| `stdin` | Prompt bytes are written to the PTY master before returning from `Start`; the CLI tool sees them as initial input. |
| _(empty/other)_ | Prompt is passed to the runtime adapter but delivery is adapter-defined (e.g. claudestream injects via `--system` flag). |

**Missing field (gap):** `boot_mode` is carried through `launch.Plan` and
`provider.StartOptions` but is not documented in the catalog schema or the API
README. Consumers cannot currently inspect what mode will be used without reading
source. Implementation gap: surface `boot_mode` in `GET /catalog/launches` and
in the `SessionDTO`.

---

## Layer 3: Session Identity

A session carries four identity fields that bind it to its origin and to its
durable agent identity.

| Field | Type | Required | Source | Notes |
|---|---|---|---|---|
| `id` (session_id) | UUIDv4 | yes | server-assigned at CreateSession | Unique per session; not reused. |
| `launch_id` | string | yes | from create request | References the catalog launch profile. |
| `project_id` | string | yes | resolved from launch | Project the session runs in. |
| `logical_agent_id` | string | yes | resolved from agent catalog | Durable identity across sessions; checkpoints are keyed by this. |
| `provider_id` | string | yes | resolved from provider catalog | Identifies the runtime kind and adapter in use. |

### Durable logical agent ID

`logical_agent_id` is the stable handle for continuity. Checkpoints are stored
against it; `mux_logical_agent_resume` uses the most recent checkpoint to seed the
next session's boot context.

**Missing field (gap):** The `logical_agent_id` is not returned on the
`mux_session_create` MCP response (current: only `session_id`, `workspace`, `log`
are returned). Clockwork and Nanite need it to correlate sessions with logical
agents without a follow-up `mux_session_get`. Implementation gap: add
`logical_agent_id` and `provider_id` to the MCP create/launch response.

---

## Layer 4: Lifecycle States and Terminal Outcome

### State machine

```
created → launching → running → completed
                              → failed
                              → killed
```

| State | Meaning |
|---|---|
| `created` | Workspace materialised, row persisted, runtime not yet started. |
| `launching` | `runtime.Manager.Start` called; runtime `Prepare` + `Start` in progress. |
| `running` | Runtime started successfully; session is live. |
| `completed` | Session exited with code 0. |
| `failed` | Session exited with non-zero code, or runtime start failed. |
| `killed` | Session was explicitly stopped via `Stop`. |

`ready` is defined in `internal/session/state.go` but not yet used in the manager
or persisted. **Gap:** either remove `StateReady` or define its semantics (e.g.
"Prepare completed, awaiting Launch").

### Provider capability hints

Consumers may branch on `provider_id` to select PTY-specific affordances (resize,
raw input) vs. API-mode affordances (structured turns). The `kind` field (`cli` |
`api`) is a more durable branch point.

**Missing field (gap):** `provider_kind` (`cli` | `api`) is not surfaced in the
`SessionDTO` or the MCP session response. Consumers must look up the provider
catalog entry separately. Implementation gap: add `provider_kind` to `SessionDTO`
and to the `GET /sessions/{id}` response.

### Terminal outcome

`GET /sessions/{id}/wait` (HTTP) and `mux_session_wait` (MCP) both return:

```json
{ "exit_code": 0 }
```

Exit code semantics: `0` = success (`completed`), non-zero = failure (`failed`),
`-1` = signaled/killed without a clean exit code.

---

## Layer 5: Attach / Read Behavior

### Live attach (HTTP daemon only)

`GET /sessions/{id}/attach` streams the session's PTY output as an unframed
`application/octet-stream`. The connection stays open until the session exits
or the client disconnects.

| Query param | Type | Meaning |
|---|---|---|
| `since_seq` | int64 | Resume: replay bytes beyond this session-byte offset. 0 = full ring replay. |

When `since_seq` is beyond the oldest retained byte (ring eviction), the full
ring is replayed silently; clients detect gaps by byte-count comparison.

### MCP attach limitation

The MCP adapter (`mux mcp`) does not expose a streaming attach tool. Consumers
that need live output must use the HTTP daemon's attach endpoint or the
`go-agentmux-client` library.

**Gap:** No MCP-native streaming output. Implementation note: MCP resources
(as opposed to tools) are the natural fit for streaming session output.
This is deferred; the current workaround is polling `mux_session_wait` +
reading the log file from `workspace`.

---

## Layer 6: Checkpoint / Resume Relationship

### Checkpoint write (current)

```
POST /sessions/{id}/checkpoint
Body: {
  "task_id": string,          // optional — caller-supplied task reference
  "workflow_id": string,      // optional
  "status": string,           // optional — "in_progress" | "done" | ...
  "completed_work": string,   // optional — freeform summary
  "pending_work": string,     // optional
  "key_decisions": string,    // optional
  "referenced_artifacts": string, // optional
  "summary": string,          // optional
  "next_recommendation": string   // optional
}
```

Checkpoint is stored against `logical_agent_id` (resolved from the session row).
Multiple sessions for the same logical agent accumulate checkpoints in order.

### Resume

`POST /logical-agents/{id}/resume` (HTTP) / `mux_logical_agent_resume` (MCP):
fetches the most recent checkpoint for the logical agent, injects its `summary`
and `next_recommendation` as boot context, creates a new session, and launches it.

**Status in v0.0.2:** HTTP endpoint returns `501 not_implemented`. MCP tool is
registered but calls through to the same 501 path. Resume lands in v0.0.3 Sprint
v003-04.

**Checkpoint payload schema (gap):** The full `CheckpointPayload` shape is defined
in ADR 0015 but the `referenced_artifacts` field is a freeform string rather than
a typed array. Implementation gap (`internal/checkpoint/`): migrate to a typed
`[]ArtifactRef{kind, uri}` before the resume path uses it.

---

## Layer 7: Typed Error Envelope

All non-2xx JSON responses (HTTP and MCP tool errors) use the shape defined in
ADR 0010:

```json
{
  "error": {
    "code": "<stable_snake_code>",
    "message": "<human readable>"
  }
}
```

| Code | HTTP | Meaning |
|---|---|---|
| `invalid_request` | 400 | Malformed body, missing required field, validation failure |
| `not_found` | 404 | Resource does not exist |
| `method_not_allowed` | 405 | Route exists, method does not |
| `conflict` | 409 | State precondition failed (e.g. launch on non-`created` session) |
| `payload_too_large` | 413 | Body exceeded per-route cap |
| `not_implemented` | 501 | Route exists, semantics deferred |
| `internal_error` | 500 | Unexpected server failure |

**MCP adapter note:** MCP tool errors return `toolError(code, message)` which
emits `{"ok": false, "error": {"code": ..., "message": ...}}` — the same codes
apply. The `ok` boolean is an MCP-adapter convention not present in the HTTP surface.

**Gap:** The MCP adapter's `isNotFound` and `isConflict` helpers in
`internal/mcpadapter/sessions.go` use string-matching heuristics
(`strings.Contains(s, "no rows")`) rather than sentinel error values. This is
fragile; implementation gap: define sentinel errors in `internal/store` and
`internal/runtime` and use `errors.Is` at call sites.

---

## Consumer Examples

### Example 1: Clockwork launching a worker through Mux as an optional provider

Clockwork owns the task execution decision. When it decides to run task `T-42`
via an agent-mux worker, it:

```
1. Selects a launch_id appropriate for the task kind
   (e.g. "backend-worker" — Clockwork's policy, not Mux's).

2. Optionally calls mux_boot_generate(profile_id) to assemble a
   task-specific boot prompt that includes the task description.

3. Calls mux_session_create(launch_id, boot_prompt)
   → receives session_id, workspace, log.

4. Calls mux_session_launch(session_id)
   → session transitions created → running.

5. Waits on mux_session_wait(session_id) or polls mux_session_get
   for state transitions.

6. On terminal state (completed/failed/killed), reads exit_code.
   Clockwork decides what happens next (retry, escalate, close T-42).
   Mux has no knowledge of T-42 — it only knows session_id.
```

**Non-goal:** Mux does not know the task ID, sprint, or execution policy. If
Clockwork wants to tag the session for its own correlation, it should use the
(gap) `metadata` field on create, or write a checkpoint after the session starts.

### Example 2: Nanite listing, attaching, and sending to a Mux session

Nanite connects to `mux mcp` as a plugin consumer. A typical flow:

```
1. mux_session_list(state="running")
   → Nanite presents the user with a list of live sessions.

2. User picks session_id "88e1c18c-..."

3. mux_session_get(session_id)
   → Nanite reads workspace, log, provider_id, logical_agent_id.

4. Nanite opens the log file at `log` path for offline reading,
   OR directs the user to the daemon's HTTP attach endpoint for
   live streaming (mux mcp does not stream output today).

5. mux_session_send_input(session_id, input="check status\n")
   → Nanite sends a command to the live PTY.

6. mux_message_send(
     from="urn:agent:nanite:session-abc",
     to="urn:agent:mux:logical-agent-xyz",
     kind="notification",
     payload_json="{\"note\":\"user reviewed output\"}"
   )
   → Cross-agent message delivered via Mux broker; session does not need
     to be running for message delivery.
```

---

## Non-Goals

The following behaviors are **explicitly out of scope for Mux** and must not be
introduced into the Mux codebase:

- **Clockwork task state**: Mux must not read or write Clockwork task records,
  sprint IDs, or execution policies. The `task_id` field in a checkpoint is an
  opaque caller-supplied string that Mux stores but does not interpret.
- **Sprint selection**: Which sprint a task runs in is Clockwork's decision.
- **Review/approval gates**: Mux does not pause sessions awaiting human approval;
  that is a Clockwork checkpoint policy.
- **PR creation or merge**: Git operations are performed by the agent running
  inside the session; Mux has no SCM knowledge.
- **Escalation chains**: Escalation policy belongs to Clockwork; Mux may emit
  events that Clockwork listens to, but Mux does not route escalations.
- **Nanite UI state**: Mux has no knowledge of which Nanite view is active.
- **Vanta memory operations**: Mux receives assembled boot prompts; it does not
  call Vanta's embedding or recall APIs.
- **Agent Ops asset installation**: Skill and tool installation is not a Mux
  concern; Mux runs whatever the provider binary resolves to.

---

## Implementation Gaps (with file/package hints)

The following are missing or under-specified fields and behaviors that must be
addressed before consumers can program to this contract:

| # | Gap | File/Package hint | Priority |
|---|---|---|---|
| G1 | `metadata map[string]string` on session create request — Clockwork correlation | `internal/api/handlers_sessions.go`, `internal/store/sessions.go` (schema migration), `internal/mcpadapter/sessions.go` | Medium |
| G2 | `logical_agent_id` + `provider_id` missing from MCP create/launch response | `internal/mcpadapter/sessions.go:handleSessionCreate`, `handleSessionLaunch` | High |
| G3 | `provider_kind` (`cli`\|`api`) missing from `SessionDTO` and `GET /sessions/{id}` | `internal/api/dto.go`, `internal/api/handlers_sessions.go` | High |
| G4 | `boot_mode` not exposed in catalog launch list or `SessionDTO` | `internal/config/model.go`, `internal/api/dto.go`, `docs/api/README.md` | Low |
| G5 | `StateReady` defined but unused — remove or specify its semantics | `internal/session/state.go`, `internal/runtime/manager.go` | Low |
| G6 | `isNotFound`/`isConflict` use string-matching; replace with sentinel errors | `internal/mcpadapter/sessions.go`, `internal/store/`, `internal/runtime/manager.go` | High |
| G7 | `CheckpointPayload.referenced_artifacts` is freeform string; migrate to typed `[]ArtifactRef` | `internal/checkpoint/`, ADR 0015 follow-up | Medium |
| G8 | Resume (`POST /logical-agents/{id}/resume`) returns 501; implement in v0.0.3 Sprint v003-04 | `internal/app/service.go`, `internal/api/handlers_agents.go` | High (v003-04) |
| G9 | No MCP-native output streaming; workaround is log-file read + HTTP attach | `internal/mcpadapter/` — defer to MCP resources proposal | Low |

---

## Consequences

- **Consumers gain a stable surface.** Clockwork and Nanite may program to
  the fields and error codes named here. Breaking changes require a new ADR that
  supersedes this one.
- **Additive changes are non-breaking.** New optional response fields, new error
  codes, and new MCP tools may be added without a new ADR, provided existing
  field semantics are unchanged.
- **Mux stays policy-free.** The non-goals list is binding. Reviewers should
  reject PRs that add Clockwork task state, sprint logic, or escalation routing
  into Mux.
- **Implementation work is sequenced.** G2, G3, and G6 (high priority) should
  land before the v0.0.3 Clockwork/Nanite integration sprint. G1, G7, and G8
  follow in Sprint v003-04. G4, G5, and G9 are low-priority cleanup.

---

## Related ADRs

- [ADR 0006](0006-provider-contract-shape.md) — Runtime + Session interface split
- [ADR 0009](0009-local-api-create-launch-split.md) — Create/Launch endpoint split
- [ADR 0010](0010-local-api-typed-error-envelope.md) — Typed error envelope
- [ADR 0012](0012-catalog-read-api.md) — Catalog read API
- [ADR 0015](0015-checkpoint-payload-schema.md) — Checkpoint payload schema
- [ADR 0019](0019-mcp-stdio-adapter.md) — MCP stdio adapter design
