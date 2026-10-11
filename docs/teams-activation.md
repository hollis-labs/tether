# Teams activation policy (Stage A)

Teams are built but formation is refused until the operator provisions an
activation policy. This page describes the Stage A policy: who may form a team,
what shape it may take, and how to pause it. Nothing here starts a team by
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

- **Who:** only the local operator, authenticated over the Unix socket (verified
  operator principal). Services, sessions, interactive users and TCP callers are
  refused.
- **Shape:** exactly three slots named `coordinator`, `workers`, `reviewer`.
  - `coordinator`: fresh, 0..1 members. The operator holds this seat as run
    owner (a governance seat with no session of its own); an agent coordinator is
    not provisioned in Stage A.
  - `workers`: 1..2 members, either one fresh member or a pool of up to two
    pre-enrolled agents (the library requires a pool for concurrent members).
  - `reviewer`: fresh, exactly 1 member.
  - Every slot's definition must be an allowlisted exact pin.
- **Reviewer runtime:** when the allowlist contains more than one runtime, the
  reviewer must run on a different runtime than the workers. With a single
  runtime it is only a preference.
- **Ceilings:** `teamsvc.ConservativePolicy()` with `MaxDepth` 1 (fan-out 4,
  budget 1, five-minute timeout, 16 rounds, 4 stalls), three slots, `may_message`
  as the only grantable permission, strict authority.

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
| `tether_team_assign` / `tether_team_delegate` | hand work to members |
| `tether_team_address` | message a member or the team |
| `tether_team_cancel` | cancel in-flight work (destructive) |
| `tether_team_report_result` | member reports a result |

Arguments are `{key, request}`; `key` is a caller-scoped idempotency key. No
caller identity argument is accepted: the principal is the authenticated
bearer's, and extra fields are rejected. Calls need the `team.write` scope.
Launched sessions receive `team.write` in their minted credential only when
teams are enabled, so they reach these tools through `tether mcp --forward-daemon`.
