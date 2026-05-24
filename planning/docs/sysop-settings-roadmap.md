# Sysop Settings Roadmap

## Current State

The Sysop GUI had operational views for overview, launch/session inspection,
messaging, MCP server/tool activity, and events. It did not have a setup or
configuration surface. Configuration data was split across:

- `global.yaml`: catalog roots, daemon defaults, storage paths, permission mode,
  launch engine.
- `providers/*.yaml`: runtime adapter, command, bootstrap, and environment
  posture.
- `mcp-servers/*.yaml`: upstream MCP server definitions, env keys, token refs,
  scopes, tags, enabled state.
- `launches/*.yaml`: project/agent/provider binding, workspace mode, prompt
  flags, MCP server selection, injection, and env overrides.
- The daemon API/state DB: live sessions, checkpoint/resume, events, proxy tool
  calls, and session mutations.

The safe first step was read-only inventory plus health checks. Sysop has now
crossed that line: it exposes a dedicated settings surface, most first-order
catalog edits are live in the GUI, lifecycle actions route through the daemon,
and MCP/system changes visibly call out reload or restart requirements.

The remaining work is no longer "add settings." It is hardening and deeper
runtime parity: backups, diff previews, richer validation, better destructive
action guardrails, and live daemon/proxy capability surfaces.

## Action Tracks

- **Launches:** launch and create/edit/clone/delete now; project, agent,
  provider, workspace mode, worktree naming, prompt flags, MCP server
  selection, env overrides, and file injection are editable. Preview is now
  diff-oriented and backup/comment-loss aware. Next: carry the same
  backup/diff/operator-safety model across the rest of settings.
- **Sessions:** stop, send turn/input, checkpoint, resume logical agent, wait,
  resize, and live output streaming with raw input now; attach/tail commands
  are copyable; command history, control-key helpers, resize sync, retention
  cleanup, stronger destructive confirms, and GUI audit rows for lifecycle
  actions now. A real logical-agent policy seam also exists for
  `checkpoint_policy`, including Sysop editing and daemon-honored `on_stop`
  checkpointing. Next: lifecycle polish, provider-hint fidelity for auto-stop
  checkpoints, and only exposing broader policy where the daemon truly honors
  it.
- **MCP:** create/edit/enable/disable/delete server catalog entries,
  secret/env editing, restart-required indicators, and daemon reload prompts
  now. The backend also already supports proxy exposure modes (`--proxy`,
  `--servers`, `--only`) plus project/launch MCP allowlists, but Sysop does
  not yet expose those effective runtime choices. Next: proxy lifecycle,
  effective allowlist inspection, live daemon tool registry, and tool-scope
  policy.
- **Tools:** usage/error drilldowns over proxy telemetry now. The backend also
  has discovery tools and a live proxy registry, but Sysop still infers tool
  surface from history instead of current runtime state. Next: live daemon
  registry, native-vs-proxy visibility, per-server allow/deny, per-launch tool
  scope, and discovery-score explanation.
- **Broker:** the backend supports broker envelopes with priority,
  workflow/correlation IDs, reply semantics, and wait-for-response dispatch,
  but Sysop currently exposes only the newer messaging-store/group surface.
  Next: broker envelope inspection, request/reply tracing, and a clear split
  between protocol semantics and operator-tunable policy.
- **Registry:** agent/project registry browse, register, update, deregister,
  sync, and bootstrap management now. Next: callback-edit support once the
  registry patch API grows it, richer search/filtering, and group-kind admin.
- **System:** frontend and daemon health, socket/PID status, guarded global
  config edits, and controlled Cerberus status/reload/apply/deploy actions
  now; restart-required indicators now. Next: richer daemon/proxy health,
  listen/PID edits, and backup/diff support.
- **Providers:** create/edit/delete provider catalog entries now, with
  launch-reference guards. Next: backups, diff preview, binary health checks,
  and provider-specific capability controls.

## Phase 1: Settings Surface And Inventory

- [Done] Add a bottom settings gear to the left rail.
- [Done] Add `/api/settings` with redacted setup/config inventory.
- [Done] Show path health for catalog roots, state DB, workspace root, temp root,
  launch specs root, and MCP server root.
- [Done] Show frontend server runtime, daemon socket/PID health, provider runtime
  summaries, launch/session coverage, and a visible roadmap.
- [Done] Replace copy-only Cerberus commands with guarded GUI actions for
  whitelisted frontend/daemon resources.
- [Done] Split the settings page into setup, MCP, system, launches, providers,
  and roadmap tabs so configuration work is no longer buried inside
  operational views.

## Phase 2: MCP, Tools, And Broker

- [Done] Add MCP server save/delete/toggle endpoints and GUI controls.
- [Done] Add MCP server create/edit/enable/disable/delete flows for
  `mcp-servers/*.yaml`.
- [Done] Validate transport-specific fields before save.
- [Done] Redact token/env values in list/detail responses.
- [Done] Add secret-safe token/env editing with preserve/delete markers.
- [Done] Add daemon reload/status so saved MCP changes can be applied without
  guessing.
- [Done] Show MCP restart-required status and daemon reload control on the MCP
  page.
- [Gap] The backend already supports proxy exposure/filter modes
  (`--proxy`, `--servers`, `--only`) plus project/launch MCP allowlists, but
  Sysop does not show the effective runtime composition or where a tool is
  becoming visible from.
- [Gap] Tool discovery exists today, but ranking is still fixed keyword-match
  scoring; there are no operator-tunable priority or ranking knobs yet, and
  Sysop does not explain that.
- [Gap] Broker envelopes already support priority, workflow ID,
  correlation ID, request/reply flow, and blocking wait-for-response semantics,
  but Sysop only exposes messaging-store messages and groups rather than the
  broker surface itself.
- [Next] Replace usage-only tool rows with live daemon tool registry rows,
  including native vs proxied vs discover-only visibility.
- [Next] Show effective proxy mode, server filters, and launch/project MCP
  allowlists so operators can see why a tool is present or hidden.
- [Next] Add per-server and per-launch allow/deny/tool-scope settings where the
  backend already has real policy hooks.
- [Next] Add discovery search/score visibility and explicitly note that ranking
  is backend-fixed today unless new policy surfaces are added.
- [Next] Add a broker inspector for envelopes, replies, workflow/correlation
  tracing, and response waits before inventing broker "settings" that do not
  exist yet.

## Phase 3: Proxy And Server

- [Done] Surface muxd health and socket/PID status.
- [Next] Surface proxy readiness and event ingest status.
- [Partial] Add guarded edits for shutdown timeout, state DB, workspace root,
  temp root, launch engine, and launch specs root.
- [Next] Add guarded edits for daemon listen address and PID file.
- [Done] Add controlled Cerberus status/reload/apply/deploy actions for
  frontend and daemon resources.
- [Done] Add restart-required indicators for global/MCP catalog changes.

## Phase 4: Launch CRUD

- [Done] Add launch and stop buttons backed by daemon-routed Sysop endpoints.
- [Done] Add launch profile create/edit/clone/delete.
- [Done] Use project, agent, provider, workspace-mode, and MCP-server
  controls, including a real MCP picker.
- [Done] Add worktree-name editing where relevant.
- [Done] Add prompt flags.
- [Done] Add env override and file-injection editors.
- [Done] Add dry-run launch plan diff preview and validation before save.
- [Done] Route launch actions through the daemon.

## Phase 5: Session Management

- [Done] Add launch, input, stop, checkpoint, and resume actions from the GUI.
- [Done] Keep session mutations daemon-routed.
- [Done] Add wait, resize, live output stream, raw terminal input, and copyable
  attach/tail controls.
- [Done] Add terminal ergonomics such as command history, control-key helpers, and
  resize sync.
- [Done] Add retention cleanup controls.
- [Partial] Lifecycle semantics are clearer now, but the sessions table still
  compresses some backend state detail: sessions are `created` first, then
  launched, then may become `running` or terminal.
- [Done] Resume is now explicitly framed as "start a new session for this
  logical agent from its latest checkpoint and stored launch profile," not
  "reopen this ended session."
- [Partial] Stop affordances now track live runtime state better, but quick
  actions vs detail actions could still be separated more cleanly.
- [Partial] `logical_agents` now exposes a real daemon/API policy surface for
  `checkpoint_policy` with Sysop editing support, and `on_stop` is daemon-honored
  by creating a checkpoint before stop. `hot_cold_policy`, attach policy, and
  cleanup/retention policy still lack a backend-honored operator surface.
- [Partial] Add clearer lifecycle copy and badges for `created`, `launching`,
  `running`, and terminal states.
- [Next] Differentiate table-level quick actions from detail-level lifecycle
  actions so stop/resume/checkpoint semantics are less ambiguous.
- [Partial] Add session policy settings only where the backend has a real
  daemon-honored config seam. `checkpoint_policy` is live; broader policy
  settings remain blocked on backend support.
- [Done] Add confirmation flows for destructive session actions.
- [Blocked] Add session policy settings around attach modes, checkpoint
  retention, and terminal/session cleanup only after the daemon grows real
  operator-honored policy seams.

## Phase 6: Provider Configuration

- [Done] Add provider save/delete endpoints and GUI controls.
- [Done] Edit provider type, brand, runtime kind, command, args, adapter,
  bootstrap mode/prefix, env mode, passthrough, and redact lists.
- [Done] Guard provider deletion when launch profiles still reference it.
- [Next] Add binary/path health checks for command-backed providers.
- [Next] Add provider-specific capability and runtime-kind guidance.
- [Next] Add backups and diff previews before catalog writes.

## Phase 7: Registry Directory

- [Done] Add Registry to the left rail as a first-class admin surface.
- [Done] Add agent/project registry list views with status filtering and
  local search.
- [Done] Add register/update/deregister flows for agent and project profiles.
- [Done] Add per-row sync from callback plus daemon-routed bootstrap/force
  bootstrap controls.
- [Next] Add callback editing once the registry patch API supports it.
- [Next] Add richer filter composition, raw lookup by URN, and group-kind
  registry administration.

## Cross-Cutting Hardening

- [Partial] Create timestamped backups before writing `global.yaml`,
  `providers/*.yaml`, `mcp-servers/*.yaml`, and `launches/*.yaml`.
- [Partial] Add a dry-run/diff preview for catalog mutations.
- [Next] Preserve YAML comments where practical, or make comment loss explicit
  in the UI before rewriting a file.
- [Partial] Add audit/event rows for GUI-initiated catalog and lifecycle
  actions. Session lifecycle actions now emit best-effort events; catalog-write
  auditing remains to be wired.
- [Next] Add optimistic refresh and stale-state warnings when daemon runtime
  config lags catalog writes.

## Immediate Next Slice

- Keep MCP/tools/broker as the next admin-parity track: expose effective proxy
  composition, live tool registry/discovery state, and broker request/reply
  inspection before adding speculative policy knobs.
- Harden the new registry admin surface with better validation and callback
  visibility.
- Carry backup-plus-diff plumbing and stale-runtime warnings across remaining
  settings/catalog writes before broadening new edit surfaces further.
- Expose live daemon/proxy capability state so MCP and tools views are no
  longer inferred from static catalog data plus usage history.
- Improve session lifecycle polish where it clarifies real backend behavior,
  especially quick-action vs detail-action separation and provider-hint
  fidelity for auto-stop checkpoints.
