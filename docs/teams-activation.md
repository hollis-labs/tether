# Teams activation policy (Stage A and Stage B)

Teams are built but formation is refused until the operator provisions an
activation policy. This page describes the policy: who may form a team, what
shape it may take, and how to pause it. Stage A (operator, coordinator seat,
conservative limits) still works unchanged apart from the ceilings; Stage B adds
an agent orchestrator, formation by a service, generous ceilings and automatic
dissolve. See [Stage B](#stage-b-orchestrator-led-teams) below. Nothing here starts a team by
itself; `teams.enabled: true` in `global.yaml` only mounts the verbs.

## Prerequisites

- `teams.enabled: true` in the catalog `global.yaml`.
- Identity mode `enforce` for the `teams` point (`tether identity mode`,
  `docs/identity-enforce-runbook.md`). Under observe/off the team verbs
  authenticate nobody and formation is unreachable.

## The policy file

`<catalog root>/teams-policy.yaml`, read once at daemon start.

- **Absent: default deny.** Every formation is refused.
- **Present but invalid: the daemon refuses to start team verbs** with an error
  naming the file and the offending entry. Unknown fields are errors.
- Edits take effect at the next daemon restart (the pause below does not).

```yaml
version: 1
launches:
  - launch: tether-claude            # exact catalog launch id; no patterns
    runtime: claude                  # runtime family, for the reviewer rule
    definition: {id: claude-agent, revision: r1, digest: sha256:...}
  - launch: agent-mux-codex-app-server
    runtime: codex
    definition: {id: codex-agent, revision: r1, digest: sha256:...}
```

Each entry may add `source: catalog:<relative path>` to its definition file; the
daemon indexes it at start and fails if the pin differs. Without it nothing
indexes the definition. See [teams-first-team.md](teams-first-team.md).

Each entry binds one exact launch id to one verified definition pin
(id, revision and digest are all required). A pin appears once and a launch id
appears once. The pinned definitions are the only ones the team host trusts, and
they are the only launch targets a team session may use. The proven launches are
`tether-claude`, `tether-claude-worktree`, `agent-mux-codex-app-server`,
`agent-mux-codex-app-server-worktree` and `tether-agy`; add others only on the
owner's say-so. The definition capability vocabulary is the mesh spawn
capability only; a definition declaring any other capability is rejected.

## What Stage A allows

- **Who:** the local operator, authenticated over the Unix socket (verified
  operator principal), and any service listed under `services:` (Stage B).
  Unlisted services, sessions, interactive users and TCP callers are refused.
- **Shape:** exactly three slots named `coordinator`, `workers`, `reviewer`.
  - `coordinator`: fresh, 0..1 members. The operator holds this seat as run
    owner (a governance seat with no session of its own); an agent coordinator is
    not provisioned in Stage A.
  - `workers`: 1..2 members, either one fresh member or a pool of up to two
    pre-enrolled agents (the library requires a pool for concurrent members).
  - `reviewer`: fresh, exactly 1 member.
  - Every slot's definition must be an allowlisted exact pin.
- **Reviewer runtime:** see [Reviewer pairing](#reviewer-pairing).
- **Ceilings:** the policy's limits (see [Ceilings](#ceilings); Stage A used
  `teamsvc.ConservativePolicy()` with depth 1, budget 1, five minutes, 16 rounds
  and 4 stalls, now replaced), `may_message`, `may_assign` and `may_delegate` as
  the only grantable permissions, strict authority. A verb only works when the
  team's grants allow the sender's slot to reach the recipient's slot (the
  example grants coordinator to workers and reviewer, workers to reviewer).

A refused formation writes nothing: no definition, run, enrollment, binding or
session.

## Admission pause

Create `<catalog root>/teams-policy.paused` (any content) to refuse new
formations immediately, without a restart; delete it to resume. A paused refusal
is `unavailable`; a policy refusal is `denied`. Pause stops formation only; it
does not stop running teams (use `tether team dissolve`).

## Not covered here

Distinct logical agent ids per team session, scheduled reconcile/flush, spawn
budget enforcement at launch and MCP exposure of `tether_team_*` are separate
activation preconditions.

# Teams MCP tools

When `modules.teams` (or legacy `teams.enabled`) is on, the daemon MCP endpoint
registers one tool per team verb. When teams are off the tools are not listed.
`tether mcp` (stdio, including `--daemon-only` and `--proxy`, which is what
launched sessions use) probes the daemon's `/teams/*` routes at start and, when
they exist, registers the same tools backed by the daemon over HTTP with the
session's own credential.

## Session formation

Formation is operator-only by default. Add `session_formation: true` to
`teams-policy.yaml` to let a verified session principal form teams under the
same ceilings and allowlist; that session becomes the run owner in the
coordinator slot (it provisions no member). Default off; read at daemon start.
`scripts/team-smoke.sh` exercises this end to end.

| Tool | Verb |
|---|---|
| `tether_team_form` | create a team run |
| `tether_team_dissolve` | end the run (destructive) |
| `tether_team_member_add` / `tether_team_member_remove` | change membership |
| `tether_team_adopt` | adopt a running agent (needs `adoption: true`) |
| `tether_team_assign` / `tether_team_delegate` | hand work to members |
| `tether_team_address` | message a member or the team |
| `tether_team_cancel` | cancel in-flight work (destructive) |
| `tether_team_report_result` | member reports a result |

Arguments are `{key, request}`; `key` is a caller-scoped idempotency key. No
caller identity argument is accepted: the principal is the authenticated
bearer's, and extra fields are rejected. Calls need the `team.write` scope.
Launched sessions receive `team.write` in their minted credential only when
teams are enabled, so they reach these tools through `tether mcp --forward-daemon`.

## Adopting running agents

Teams normally provision their own sessions. With `adoption: true` in
`teams-policy.yaml` (default off, read at daemon start) the run owner can
instead adopt an agent that is already running, together with its live session,
without launching, enrolling or re-binding anything. Adoption never widens what
Stage A allows.

```
tether team adopt --key adopt-1 --request '{"run_id":"...","slot":"workers",
  "actor":"msg://agent/...","session_id":"...","binding_generation":3}'
```

The request names the actor, its session and the registry binding generation.
The daemon verifies all three from its own records; a roster name or a claimed
launch is never enough. Adoption is refused unless:

- the actor is an active, registered agent with a live registry binding whose
  session is the one named and whose generation is the one named;
- that session is running and its own live credential names the forming
  principal (the operator) as the one who launched it. Under identity `observe`
  or `off` there is no credential, so nothing is adoptable;
- the session's launch is in the policy allowlist, and its pin (the one the
  policy binds to that launch) equals the seat's definition pin and loads;
- the seat is a **pool** (or durable) slot of the run that already declares that
  actor, has room (the owner's own seat counts), and the actor holds no other
  active team seat. A fresh slot mints its own identity, so an existing actor
  cannot fill one. A losing race is refused, never queued.

With adoption on, the Stage A shape also admits empty pool seats: workers 0..2,
reviewer 0..1, and a coordinator pool of 2 (the operator's owner seat plus one
delegate). Pool seats must start empty (`min: 0`) and a formation may provision
nothing at all. A pool must list at least as many identities as its maximum.
The reviewer runtime rule still applies through the seats' definition pins.

**The coordinator seat is the operator's, held by a delegate.** The operator
stays the run owner. An adopted agent may sit in the coordinator slot only as a
delegate: every grant in the team definition from that slot must be
`may_message` or `may_assign`; a definition that grants `may_delegate`,
`may_admin` or `may_spawn` from it is refused for adoption. Every identity in
the coordinator pool gets exactly the slot's grants. A delegate cannot dissolve,
remove members or adopt. To revoke the delegation, remove the member
(`tether team member_remove`); the run, the operator and the other members are
unaffected.

**Leaving a team never stops an adopted agent.** Dissolve, member removal, run
timeout and team failure terminate sessions the team *provisioned*. An adopted
member is released instead: it is told once, its seat is freed, and its
session, registry binding and registration are left exactly as they were. If a
member's binding is later re-leased (a new generation), the membership is
suspended (no messages) until the run owner re-adopts it explicitly.

Adopted members are never spawn-capable, and adoption does not touch Torque
claims or task assignment.

## Stage B: orchestrator-led teams

Stage B is shaped for the unit-of-work model: Torque's dispatcher forms a small
team, the team does the task and the run dissolves itself. It is selected by the
shape of the request: a team with an `orchestrator` slot is Stage B, anything
else must be Stage A shaped. Nothing about identity changes: the `teams`
identity point may stay at `observe`; the service is trusted as presented (a
valid credential, listed in the policy), and no identity feature is added.

### Shape

Four slots, exactly:

| Slot | Resolution | Size | Notes |
| --- | --- | --- | --- |
| `coordinator` | fresh | 0, no member | The owner's governance seat. The forming operator or service holds it as run owner; it launches no session. It can dissolve the run and receives the result. |
| `orchestrator` | fresh | exactly 1 | An agent. Runs the team and owns upward communication. |
| `workers` | fresh (1) or pool | 1..3 | A pool needs pre-registered agent identities (the library requires a pool for more than one concurrent member). |
| `reviewer` | fresh | exactly 1 | Independent code reviewer. |

So a team is the orchestrator, one to three workers and the reviewer: three to
five agents, never more than five (the owner seat is not an agent). The
coordinator slot is not the orchestrator on purpose: a slot's members share its
grants and its address, and the owner seat must not receive messages meant for
the orchestrator. Stage A shapes keep working as they were.

### Authority

A Stage B team may carry only these grants; any other grant (a worker to a
worker, a worker to the reviewer, an actor grant, `may_spawn`, `may_admin`, ...)
is refused at formation:

- `coordinator` to `orchestrator`: `may_message`, `may_assign`, `may_delegate`
  (the owner hands over the task; the orchestrator's report returns to it).
- `orchestrator` to `workers` and to `reviewer`: `may_message`, `may_assign`,
  `may_delegate`.
- `workers` to `orchestrator`: `may_message` only.
- `reviewer` to `orchestrator`: `may_message` only.

Workers therefore cannot message each other, the reviewer, the owner or anyone
but the orchestrator (`internal/teamruntime/stage_b_test.go` proves this through
the real service: peers by URN, slots, `@all`, self, the service and every verb).
A member reporting the result of a delegation does not need a reverse grant.

### Ceilings

`ceilings:` in `teams-policy.yaml` sets a `max` (hard cap) and a `default` (fills
what a request leaves unset). A team's `policy.spawn` and the launch request's
`limits` may only tighten them; asking for more than `max` is refused. Omitted
fields take the built-ins:

| Limit | Default | Max |
| --- | --- | --- |
| `timeout` (run lifetime) | 4h | 12h |
| `max_rounds` | 200 | 500 |
| `max_stalls` | 20 | 40 |
| `max_depth` | 2 | 3 |
| `max_children` / `fan_out` | 6 | 8 |
| `budget` | 2000 | 10000 |

A default the file leaves unset is clamped to a lowered max; a default set above
the max is an error. Max members per slot is 3, initial members at most six
(five agents and the owner), slots at most four.

### Formation by a service

```yaml
services:
  - msg://service/torque/dispatcher
```

A listed service that presents a valid credential (carrying `team.write`) may
form teams and dissolve the runs it owns over the authenticated daemon
transport, with identity in `observe` or `off` as well as `enforce`. The URN
must be a service principal (`msg://service/<a>/<b>`). Unlisted services, an
unverified credential, an asserted header and every other principal kind are
refused. The service is the run owner, so it holds the governance seat and uses
`tether_team_delegate` to hand the task to `@orchestrator`. An absent policy
file still denies everything, and the admission pause file still refuses
formation for services and the operator alike.

### Per-slot model and runtime

Each slot's definition pin resolves to exactly one allowlisted launch, so a role
maps to a model by naming the launch's pin: add several launches per runtime
(`claude`, `codex`, `antigravity`), give each its own definition file and pin and
an optional `model:`, then put the orchestrator and reviewer on the Astra pin
and workers on Sol or Agy. The launches must exist in the catalog.

### Reviewer pairing

The reviewer should differ from the workers by runtime, or by model when both
launches name one. With `reviewer_pairing: require` (the default) it is refused
when the allowlist holds two launches that differ and the request does not use
them; when no two launches differ (for example one launch) it is a preference:
the formation proceeds and the fallback is logged and recorded on the formation
result. `reviewer_pairing: prefer` makes it a preference always.

### Lifecycle

The run ends, and every member session stops, when:

- the orchestrator reports a result (`tether_team_report_result`; the owner's
  delegation to the orchestrator is what it answers). The run is marked to end
  and the daemon's team loop (every `TETHER_TEAMS_LOOP_INTERVAL`, 5s default)
  dissolves it, so the reporting call returns first. A worker's or reviewer's
  report does not end the run. Without a deadline store the dissolve is inline.
- the `timeout` ceiling passes, or
- no non-owner member is live any more (all stopped, failed or released).

The owner can still dissolve explicitly (`tether_team_dissolve`). A dissolved run
is closed; a failed dissolve is retried on the next pass. Deadlines live in
`team_run_deadlines` (migration 0108); runs formed before it have none and are
dissolved with the verbs. Limits other than `timeout` (`max_rounds`, `max_stalls`,
budget, depth) are passed to the library and member launches as before; Tether
does not itself count rounds or stalls.

Example files: [teams-policy-stage-b.yaml](../examples/teams/teams-policy-stage-b.yaml)
and [team-form-stage-b.json](../examples/teams/team-form-stage-b.json) (an
orchestrator, two workers and a reviewer).
