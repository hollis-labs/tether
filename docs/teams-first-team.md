# Forming a first team (local, Stage A)

Prerequisite reading: [teams-activation.md](teams-activation.md). Files live in
`examples/teams/`; `scripts/team-smoke.sh` runs this whole recipe against a
throwaway daemon and prints PASS/FAIL.

## 1. Definitions and pins

A pin is `{id, revision, digest}` of an authored agent definition (ADR 0054
schema 2: YAML frontmatter plus Markdown body, see `examples/teams/definitions/`).
The digest is the semantic digest, not a file hash; compute it offline:

```sh
tether team pin examples/teams/definitions/*.md     # JSON line per file
```

(`tether team` exists only while `teams.enabled: true` in the catalog.) Each
launch in the policy needs its own pin, so there is one definition per launch.
Changing a definition's semantics requires a new `revision` and a new pin.

## 2. Catalog files

```sh
C=~/.tether/catalog
mkdir -p $C/team-definitions
cp examples/teams/definitions/*.md $C/team-definitions/
cp examples/teams/teams-policy.yaml $C/teams-policy.yaml
```

Each policy entry carries `source: catalog:team-definitions/<file>.md`. At start
the daemon indexes that file and refuses to start team verbs if its pin differs
from the policy (without `source`, nothing ever indexes the definition and every
formation fails to load its pin). The four launches (`tether-claude`,
`tether-claude-worktree`, `agent-mux-codex-app-server`,
`agent-mux-codex-app-server-worktree`) must already exist in the catalog.

## 3. Identity and restart

```sh
tether identity flip --point teams --to enforce --evidence "first team"
# restart the daemon, then:
tether identity mode        # teams: configured enforce, running enforce
```

`global.yaml` also needs `teams: {enabled: true}`. Run the CLI without a stale
`TETHER_TOKEN` in the environment: it overrides the operator token and every
team verb then answers `unauthenticated`.

## 4. Form, message, dissolve

```sh
tether team form --key first-1 --request-file examples/teams/team-form.json
tether team ls
tether team msg RUN_ID @workers 'hello' --key first-msg-1   # slot addresses need @
tether team stop RUN_ID --all --key first-stop-1
```

The form request is Stage A shaped: slots `coordinator` (the operator's seat, no
member), `workers` (one) and `reviewer` (one, on the other runtime). Messages are
delivered to member sessions whose runtime has a turn feed (streaming-stdio or
app-server); a PTY runtime is refused.

## Request shape and diagnostics

Use [team-form.json](../examples/teams/team-form.json) as the complete request
example. The CLI file contains `{"team": {...}, "launch": {...}}`; the
idempotency key comes from `--key`. For MCP, pass that same object inside
`request`, alongside `key`:

```sh
jq '{key: "first-1", request: .}' examples/teams/team-form.json
```

Pass the resulting object as the arguments to `tether_team_form`.
For HTTP, POST the original file contents to `/teams/form` with the caller's credential
and `Idempotency-Key: first-1`.

| Field in `team` | Meaning |
| --- | --- |
| `id`, `name`, `version` | Definition identity, display name and numeric version. |
| `slots` | Named roles with definition pins (`definition_ref`), resolution, activation and member bounds. |
| `phases` | Phase IDs, active slots, owner slot and exit triggers. |
| `authority` | Authority mode and grants between slots. |
| `routing` | Routing rules and `coordinator_slot`. |
| `policy` | Spawn ceilings and history policy. |

The MCP tool schema describes these nested fields. Host policy still decides
which definitions and launches are permitted; a structurally valid request is
not necessarily allowed by Stage A.

`team.coordinator` and `team.members: [{urn, role}]` are not supported fields.
They describe a roster, not a team definition. Unknown fields are rejected
strictly, with a field path such as
`invalid_request: team.coordinator: unknown field` or
`invalid_request: team.slots[0].urn: unknown field`. Missing fields and incorrect
JSON types also return field diagnostics. Request values are not included in
these diagnostics. Host failure details remain restricted to the verified
local operator.

Registration of durable actors does not enroll them into a team. Stage A keeps
the operator's coordinator seat, approved worker/reviewer definitions and its
existing runtime and resource limits. Forming a team from an already-running
fleet requires a separate adoption and ownership design. Do not add automatic
team formation to fleet boot using a roster request; keep it manual until that
capability and its host policy are explicitly supported.
