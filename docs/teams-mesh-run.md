# Running a team across machines (mesh run)

How to form a Tether team whose members run on connected workers, message it,
and tear it down. This builds on [teams-activation.md](teams-activation.md) and
[teams-first-team.md](teams-first-team.md); read those first. Commands below were
checked against the `tether team` verbs in `internal/teamcli/` and the examples in
`examples/teams/`. Anything not verifiable from the repo is marked **(unverified)**.

## 1. Prerequisites

- `teams: {enabled: true}` in the catalog `global.yaml`. Without it `tether team`
  does not exist (`unknown command "team"`).
- Identity mode `enforce` for the `teams` point (`tether identity mode`).
- `teams-policy.yaml` in the catalog root binding each team launch to a pinned
  definition (`examples/teams/teams-policy.yaml`; a Claude-only variant is
  `teams-policy-claude-only.yaml`). Pins come from
  `tether team pin examples/teams/definitions/*.md`; copy the definitions to
  `<catalog>/team-definitions/`. Each launch needs its own pin.
- The launches the policy names exist in the catalog and are enabled, and each
  has `worker: <label>` (section 4).
- The workers named by those labels are enrolled and **connected** to the hub.
  Placement fails closed otherwise; it never falls back to the hub.
- Run the CLI without a stale `TETHER_TOKEN` in the environment (it overrides the
  operator token and team verbs answer `unauthenticated`).

`tether catalog validate` warns about policy launches that are missing, disabled,
or have no `worker:` while their project `repo_root` is a placeholder
(`internal/catalogcheck/teams_policy.go`).

## 2. Form

```sh
tether team form --key mesh-1 --request-file examples/teams/team-form-claude.json
tether team ls
```

`--key` is a required caller-scoped idempotency key; reuse it only to retry the
same request. `team-form-claude.json` has the three Stage A slots: `coordinator`
(the operator's seat, no member), `workers` (one, `claude-agent`) and `reviewer`
(one, `claude-worktree-agent`). Its `definition_ref` digests must match the
policy pins. The result is one JSON line containing the run id; `RUN` below is
that id.

## 3. Delegate work and receive replies

```sh
tether team msg RUN @workers 'text' --key mesh-msg-1
tether team msg RUN @reviewer --body-file brief.md --key mesh-msg-2   # or - for stdin
```

- Slot addresses need the `@`. `BODY` is either the third argument or
  `--body-file`, not both.
- The sender's slot must hold a grant to the recipient's slot; the example
  grants coordinator to workers and reviewer, workers to reviewer.
- Delivery needs a member runtime with a turn feed (streaming-stdio or
  app-server); a PTY runtime is refused.
- Members reply by mail to the coordinator address given in the turn, e.g.
  `tether messages notify --from <member> --to <coordinator> --kind response ...`
  (this is what `scripts/team-smoke.sh` does). With the operator in the
  coordinator seat, the replies land in the operator's inbox.
- Members can also use the `tether_team_*` MCP tools (see
  [teams-first-team.md](teams-first-team.md) and the MCP section of the
  activation docs); `session_formation: true` in the policy lets a session form
  a team and become the run owner.

## 4. Cross-machine placement

Set `worker:` on every launch the policy names, for example the launches used
for the worker and reviewer slots:

```yaml
id: tether-claude
...
worker: worker-a          # enrolled worker label (or id)
---
id: tether-claude-worktree
...
worker: worker-b
```

(Two separate launch files; the `---` just separates the two examples.)

- A team member session is created on the launch's worker. If the worker is
  unknown or offline, creation fails; there is no hub fallback
  (`internal/app/team_ports.go`). A launch with no `worker:` runs on the hub
  as before.
- The label may be a different worker per launch, so slots can land on
  different machines.
- `worker:` is a catalog change; the daemon loads the catalog at start, so
  restart (or reload per your normal catalog procedure) before forming.
- Worktree mode: the `*-worktree` launches use `workspace: {mode: worktree}`, so
  the member gets a fresh git worktree of the project's `repo_root` **on the
  worker's filesystem**. The project's `repo_root` therefore has to exist on that
  worker; `repo_root` is never edited or deleted. See
  [catalog-launch-profiles.md](catalog-launch-profiles.md#workspace-roots-and-worktree-lifecycle).
  Whether a stale hub-side `work_dir` is accepted for a worker launch is
  **(unverified)**; see the launchparity note for `agent-workspaces-*` launches.

## 5. Stop and dissolve

```sh
tether team stop RUN --all --key mesh-stop-1          # dissolve the whole run
tether team stop RUN MEMBER --key mesh-stop-2         # cancel one member
tether team stop RUN MEMBER --cascade --key mesh-stop-3   # and its descendants
```

`--all` takes no MEMBER and cannot be combined with `--cascade`. Worker-placed
members are stopped on their worker; if the worker is unreachable the stop is
retryable, so rerun it once the worker is back. To block new formations without
stopping running teams, create `<catalog>/teams-policy.paused`.

## 6. Known limits

Verified from the repo:

- Stage A shape only: slots `coordinator` (0..1, operator seat), `workers`
  (1..2) and `reviewer` (exactly 1); `MaxDepth` 1.
- Reviewer must use a different runtime than the workers when the allowlist has
  more than one runtime.
- Policy edits need a daemon restart; only the pause file is live.
- No hub fallback for worker-placed launches (section 4).

Observed in the 2026-10-14 run: **(unverified; not reproducible from the repo)**
the run owner (coordinator) is the operator, so workers learn their task only
from messages sent with `team msg`; a worker started with no message has nothing
to act on.
