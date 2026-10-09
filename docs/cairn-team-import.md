# Import a prepared Cairn/team role

`tether import-team` converts an explicitly reviewed Codex tmux baseline and an
already prepared Cairn boot directory into a **new isolated Tether catalog**.
It is opt-in and offline: it does not run Cairn, launch a provider, contact a
daemon, mint an identity, resolve a credential, install hooks or modify a unit.
Chrispian chooses any actual team switch separately.

The source task is CW-20261009-0013. The existing preparation draft is
`project/tether/workspace/drafts/cw_20261009_0013_cairn_team_tether_opt_in_runbook`.
The importer implements the conversion boundary; it does not move a running
tmux process or copy its mailbox/history into another daemon store.

## Capture the effective baseline

Read the current team launcher and prepared role before filling the manifest.
For the agent-os Codex launcher, preserve:

- The existing `msg://agent/agent-mux/<agent-id>` URN, role, project and name.
- The effective model and reasoning effort, including runtime selections that
  override a host config. The launcher itself does not pin them.
- Exact upstream IDs and **final gateway tool names or existing patterns**.
  Explicit `[]` means no targets. Missing/null grants are refused. `*` is
  appropriate only when the existing reviewed tool grant already permits it.
- The original native credential scope list in `native_scopes`. This is audit
  evidence, not a request to issue those scopes. Normal Tether workers currently
  receive session/message/catalog write scopes. A tmux baseline requesting
  registry/group writes cannot assume equal mutation authority after conversion;
  reconcile that capability before selecting a real trial. The importer does
  not change credential issuance or add a grant to compensate.
- Every additional MCP server in the effective provider configuration. Map it
  to a reviewed catalog definition and final tool names; do not silently drop
  browser/PDF servers or broaden a narrower native-tool scope into `*`.
- Project scope, team root, worktrees root, tool PATH, permission posture and
  the assignment/start prompt. The importer supplies the same TEAM identity
  variables to both the provider environment and Codex's shell env policy.

`baseline_evidence` records the non-secret source/selection references used for
each role. This is an operator-reviewed conversion input, not an independent
authorization mechanism. The current caller's authority and normal Tether
credential/MCP ceilings still apply at actual launch.

Do not copy a host provider config, token argv or credential values into the
manifest. `non_secret_sources: true` attests that the selected charter and skill
assets are safe to persist in launch plans. The importer copies only `AGENTS.md`
and regular UTF-8 files in the complete `.agents/skills` tree, including scripts
and text assets. It refuses symlinks, credential-like asset names and binary
assets instead of silently omitting or corrupting them. Source provider config,
auth, hooks and `launch.sh` are never copied or executed.

## Manifest and conversion

This schema example intentionally contains unresolved selections. Replace them
with the reviewed existing role baseline; it is not a live grant or model default.
All paths must resolve before import. The destination's parent must already exist.
The destination path must have no whitespace or glob characters, because the current `team` helper
expands `TEAM_TETHER` as one executable command word. `tether_command` names one
reviewed binary or wrapper executable, not a shell command with arguments.

```json
{
  "version": 1,
  "non_secret_sources": true,
  "team_home": "/absolute/team",
  "team_session": "owned-trial",
  "tether_command": "/absolute/bin/tether",
  "worktrees_root": "/absolute/worktrees",
  "path": "/absolute/tools:/usr/bin:/bin",
  "roles": [{
    "name": "task-example-1",
    "role": "task",
    "project": "example",
    "scope": "/absolute/project",
    "boot_dir": "/absolute/prepared-cairn-task",
    "urn": "msg://agent/agent-mux/agt_existing",
    "runtime": "codex",
    "model": "REPLACE_WITH_EFFECTIVE_EXISTING_MODEL",
    "effort": "REPLACE_WITH_EFFECTIVE_EXISTING_EFFORT",
    "permission_mode": "bypass",
    "prompt": "Read the current team run, mission and exact assignment, then begin.",
    "baseline_evidence": "REPLACE_WITH_CURRENT_SELECTION_AND_GRANT_REFERENCES",
    "native_scopes": ["session.write", "message.write", "catalog.write"],
    "mcp_servers": ["reviewed-read-server"],
    "mcp_tools": ["reviewed_read_tool"]
  }],
  "mcp": [{
    "id": "reviewed-read-server",
    "transport": "stdio",
    "command": "/absolute/installed/upstream",
    "args": ["mcp"],
    "token_file": "/absolute/existing/private/token-file"
  }]
}
```

Roles can have different exact grants, models and effort. Each granted server
needs its reviewed definition in `mcp`; unused definitions are refused. Supported
server fields are `id`, `transport`, `command`, `args`, `env`, `url`, `token`,
`token_file`, `proxy_service_token_file`, `tool_prefix`, `scopes` and
`allow_unconfined_remote`. `token` accepts only an existing file/helper/keychain
reference. Legacy token argv and literal credential environment fields are
refused. File paths are preserved as references; their contents are never read,
copied or provisioned. Remote URLs containing userinfo/query/fragment are refused.

```bash
tether import-team --manifest /absolute/reviewed-team-baseline.json \
  --output /absolute/owned-trial/catalog
```

The output contains ordinary project, agent, provider, launch, MCP and boot-profile
YAMLs plus the non-secret baseline and role prompt assets. Existing actor IDs are
the catalog agent IDs, preserving canonical Tether binding targets. Provider args
pin model, effort, shell environment and the same three declared roots. The tool
filter is present on the launch as well as the boot profile, so leaving out the
profile cannot turn an explicit empty tool grant into inheritance.

`TEAM_TETHER` points to the generated private `bin/team-tether` wrapper. It runs
the original reviewed `tether_command` with `--catalog` fixed to this destination,
refuses catalog overrides, preserves argument boundaries and inherits runtime authentication. It adds no
operator credential or shared-catalog fallback. The original binary selection
remains in the saved baseline; the owned catalog path is an explicit environment
projection. The generated prompt begins with `RUN CONTEXT: runtime-managed`.

Catalog files are 0600; the helper executable and directories are 0700. Per-role disk temp and write-home
directories, state DB path and control socket/PID paths stay beneath the owned
destination. Existing destinations and destinations under shared team/boot,
`~/.tether` or provider-home trees are refused. The catalog uses identity enforcement,
explicit grant lists and direct hosting; it does not provision a caller credential
or upstream authority.

## Proposed activation, steering and wake

These commands are a runbook for a separately selected trial. Importing alone
does not authorize or perform activation. Configure the owned daemon's independent
caller authority and explicit trusted `CODEX_HOME` through the existing supported
operator workflow, preserving the original role ceilings. Do not reuse a live
actor in two simultaneously active stores/processes. Inspect the generated
inputs first and select a quiescent role with a positive custody fence.

```bash
CAT=/absolute/owned-trial/catalog
AUTH=/absolute/owned-trial/operator-token-file
tether --catalog "$CAT" --token-file "$AUTH" daemon run
```

In a separate operator terminal, after the owned daemon is ready:

```bash
tether --catalog "$CAT" --token-file "$AUTH" launch \
  --launch task-example-1 \
  --boot-profile "$CAT/boot-profiles/task-example-1.yaml" \
  --idempotency-key trial-task-example-1
tether --catalog "$CAT" --token-file "$AUTH" sessions get SESSION_UUID
tether --catalog "$CAT" --token-file "$AUTH" sessions turn SESSION_UUID 'bounded steering instruction'
tether --catalog "$CAT" --token-file "$AUTH" messages notify \
  --from ORIGINAL_OPERATOR_OR_AGENT_URN --to ORIGINAL_AGENT_URN \
  --kind request --subject 'Owned trial work' 'bounded work instruction'
tether --catalog "$CAT" --token-file "$AUTH" messages list ORIGINAL_AGENT_URN
```

Use the daemon's current binding for wake; do not point to an obsolete session.
`team msg` still sends a tmux nudge and is not a hosted-session wake adapter.
Mailbox `list` is non-destructive. Inbox pull, mark-read, consume and terminal
acknowledgment have separate meanings; importing a role adds no ACK behavior.

## Restart and rollback

The generated catalog defaults to direct execution. Direct workers, and detached
shim workers remaining in the daemon's cgroup, do not provide survival across a
normal systemd service restart. A survival trial requires a separately selected
owned activation with `catalog.defaults.launch_host: shim` and
`catalog.defaults.shim_host.systemd_user: true`; see [shim-host.md](shim-host.md).
That uses transient user units and is outside offline importer validation.

Before any affected restart, the sole operator captures current provider/host,
thread, binding, custody, active-turn and pending-operation state. Restart only
the owned daemon unit, then inspect the same host/provider and normal retained
settlement. Do not infer survival from HTTP health, repeat input, drain an inbox
or start a replacement while effects are unknown. Existing scoped release
acceptance is not a new team-switch acceptance result.

Rollback first stops/fences the exact owned Tether session and verifies provider
custody is gone or otherwise explicitly reconciled. Then restore the original
tmux role with its original launcher/model/grants. Preserve the trial catalog,
state and receipts; do not delete unresolved work or mint another actor. If
custody/side effects are uncertain, stop and route that condition to the lifecycle
owner instead of starting a duplicate execution.

## Verification and limits

Owned fake fixtures exercise catalog loading, supported launch-plan resolution,
MCP zero/exact grants, original identity, role prompt generation, executable skill
assets and actual Codex provider artifact planting with owned fake auth. They do
not start a provider/daemon/unit or mutate host credentials, shared catalogs or a
real team.

This converter supports prepared **Codex** roles. Other provider runtimes are
refused rather than partially converted. It does not reproduce an interactive
TUI, import hooks/history/host config, provision credentials or reconstruct native
context. Declared roots are not an OS access boundary under the existing bypass
posture; shipped Codex protection limitations still apply. Old-home MCP availability,
repository availability, external hook installation and whole-runtime restart
guarantees are not established by this conversion.
