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
