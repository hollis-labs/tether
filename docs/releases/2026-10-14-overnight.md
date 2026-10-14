# Overnight release notes and deploy runbook, 2026-10-14

Covers every PR merged to `hollis-labs/tether` `main` after #281, through #336
(main at `c201eb55` when written). Compiled from PR titles, changed-file lists
and the Torque task comments of the authors. Nothing here was deployed or
exercised live by the author of this document. Items marked **(unverified)**
are inferred from code or comments, not observed.

Hub reference point: `tether_health` on 2026-10-14 07:12Z reported version
0.8.1, build `010fe40`, built 2026-10-14T05:05Z (CW-20261013-0041 comment).
Everything below that is not in that build is new.

## 1. Changes

Placement: **H** = hub daemon (`tetherd`) restart needed, **W** = worker binary
(`tether-worker`) swap needed, **C** = CLI/client only (same `tether` binary),
**D** = docs, examples or tests only, no runtime effect. The hub and CLI are the
same `cmd/tether` binary; an H change also reaches the `tether` CLI once that
binary is installed.

Risk: L = additive or opt-in, nothing changes until configured; M = changes
existing runtime behaviour; H = touches shutdown, launch admission or a wire
protocol.

| PR | Behavior change | Config / catalog / schema fields added | Place | Risk |
|---|---|---|---|---|
| 282 | Team form rolls back already-started members on failure; typed errors show cause to local operator; failing provision stage is named | none | H | L |
| 283 | Common boot and `worker-agent` get a "close the loop on Torque tasks" step | `agent_prompt` on example `worker-agent.yaml` (catalog example only) | D (catalog copy needed) | L |
| 284 | `catalog apply` restores the previous files when the live reload is refused | none | H | L |
| 285 | Opt-in `stop_after_final_turn`: session stops (reason `final_turn`) after a turn completes and a grace passes with no activity | launch/agent `lifecycle.stop_after_final_turn` (duration string, default off); example worker-agent sets 60s | H | M when enabled |
| 286 | Workers reap orphaned shim-hosts, surface them, and add `tether workers reap <worker>`; new `POST /workers/reap` | wire change in `workerwire/protocol.go` (worker report + reap request) | H + W | M-H (wire) |
| 287 | Team member stop by session id in any state on workers; launching continues on unknown worker launch outcome; CLI shows operator cause | none | H | L |
| 288 | Team verbs detach from client cancellation; no client timeout for team verbs | none | H | L |
| 289 | Worker passes http/sse MCP entries through without empty `command`/`args`; headers only as `${VAR}` references | none | W | L |
| 290 | Test fix (temp roots on darwin) | none | D | L |
| 291 | `tether doctor` checks migrations read-only (no false SQLITE_BUSY against a live daemon) | none | C | L |
| 292 | Explicit open-routing-reply check before `stop_after_final_turn` stops a session (re-arms while a reply is queued or delivering) | none (new store query) | H | L |
| 293 | Test fix (boot proxy anonymity on darwin) | none | D | L |
| 294 | `tether_catalog_list_launches` reports `enabled` | output field `enabled` | H | L |
| 295 | Launch logs a WARN when a headless launch resolves to `permission_mode: default` with MCP servers attached (warn, not refuse) | none | H | L |
| 296 | Routing-reply dispatcher counts attempts for `ErrSessionNotRunning`, backs off linearly, settles `undeliverable/session_ended` at 5 attempts (was: retry every 1 s forever) | none | H | M |
| 297 | Example `task-worker` launch template | `examples/catalog/launches/task-worker.yaml` (references project `tether`; copy into live catalog) | D | L |
| 298 | Worker validates `hub-mcp.json` at start (WARN); hub records `session.launch_failed` with sanitized cause (300 chars); chat-send error reads `session launch failed: <cause>` | event `session.launch_failed` | H + W | M |
| 299 | Graceful shutdown: HTTP listeners shut down concurrently on one third of `shutdown_timeout`, then force-close; deadline error there is not fatal | none | H | H (shutdown path) |
| 300 | `tether launch --idempotency-key auto` derives a stable key from launch id, worker and sorted `--torque-task` ids | CLI flag value `auto` | C | L |
| 301 | Test assertion fix for #298 | none | D | L |
| 302 | `teams-policy.yaml` listed/readable via `tether_catalog_files`; `catalog validate` checks teams-policy (parse, one-launch-per-pin, placeholder `repo_root`, disabled/missing launch or agent) | validation only | H | L (new validate failures can reject plans that passed before) |
| 303 | Non-lossy agent edit; editable-agent allowlist (`--editable-agent`, manual-only) | `--editable-agent` flag on `tether mcp` | H + C | L (see CW-20261014-0047: nothing plants the allowlist yet) |
| 304 | `tether sessions turn --reply-to <addr>`; `docs/local-operator-sessions.md` | CLI flag | C | L |
| 305 | CLI unreachable errors name the targeted daemon; `TETHER_TARGET` env sets default `--target` | env `TETHER_TARGET` | C | L |
| 306 | `tether_catalog_files` on `team-definitions/` returns the pin `{id, revision, digest}` | output field `pin` / `pin_error` | H | L |
| 307 | `tether sessions peek <id> [n]` digest of a session log | none | C | L |
| 308 | Plan output shows `ttl_seconds`; re-planning an identical unapproved bundle re-arms the same plan id (expired plans kept 1 h for this) | plan output field `ttl_seconds` | H | L |
| 309 | Team member spawn timeout enforced at launch as session lifecycle `max_duration`; fails closed if the session service lacks the capability | none | H | M (changes team plan digest, see CW-20261014-0063) |
| 310 | `catalog apply` of `teams-policy.yaml` or `team-definitions/` reports `restart_required` (was `immediate`) | apply result `restart_required` | H | L |
| 311 | `tether launch --replace` rebinds an idempotency key once its session ended; 409 `idempotency_conflict` if still live | API `replace_idempotency` | H + C | M |
| 312 | Hub-local resume of a worker-placed source is refused | none | H | M (CW-20261014-0063 notes it may be over-broad for checkpoint-only resume) |
| 313 | Teams policy: reviewer pairing by runtime or model; single-runtime fallback logged | optional `model` on teams-policy launches | H | L |
| 314 | `tether launch --torque-task` records `torque_task` session refs; new `tether sessions tasks [--all] [--json]` | none (reuses refs store) | H + C | L |
| 315 | Test update for #296 | none | D | L |
| 316 | Docs: daemon-owned MCP upstreams | none | D | L |
| 317 | Sanitize launch failure text in chat; document `--editable-agent` as manual-only | none | H | L |
| 318 | Tests: canonicalize `TMPDIR` on darwin | none | D | L |
| 319 | Tests: skip darwin-unrepresentable fixtures | none | D | L |
| 320 | Per-tool-call machine-access enforcement for Cerberus. Read-only by default when Cerberus is granted; fail-closed read-verb classifier; durable `machine_authority_decision` event per decision | agent catalog `machine_access: read-only\|read-write`; env `TETHER_MACHINE_ACCESS`; `TETHER_DEV_MODE=1` = full access | H | H (denies Cerberus writes by default) |
| 321 | MCP tool `tether_session_peek` | new MCP tool | H | L |
| 322 | Tests: skip protected-Codex tests on darwin | none | D | L |
| 323 | Reviewer pairing kept per formation, keyed by `req.Key` (`PairingFor`); `LastReviewerPairing` removed | none | H | L |
| 324 | Session lineage limits and cascade stop in the app layer (resume path enforced); inert until wired by #336 | none (code only) | H | L |
| 325 | Result-bearing session wait | `WaitResponse` gains result fields | H | L |
| 326 | MCP reply and interrupt tools | new MCP tools | H | L |
| 327 | Execution policy: global go/pause, environment session cap, per-project caps, enforced at launch (409 `execution_paused`, `environment_cap_reached`, `project_cap_reached`; session stays `created`) | table `execution_policy` (migration **0077**); `GET/PUT /execution/policy`; `tether execution status\|pause\|resume\|cap`; MCP `tether_execution_status`. Default: running, no caps | H | M-H (migration, launch gate) |
| 328 | Operator retire for `outcome_unknown` detached-shim sessions: `POST /sessions/{id}/retire-shim` (maintain scope), `tether sessions retire-shim <id>`; audit event `session.shim_retired` | new route, new event kind | H + C | M |
| 329 | `ReviewerPairing` emitted on the formation result | result field | H | L |
| 330 | Tests for #328 refusals | none | D | L |
| 331 | Machine authority also enforced in the daemon's shared gateway view; classifier vetoes compound names (`get_and_apply`, ...). `handler.go` does not populate `MachineAccess`, so daemon views are read-only for Cerberus | `ProxyOptions.MachineAccess` | H | H (see #320) |
| 332 | `--replace` builds the plan before unbinding the key, so a failed build keeps the binding | none | H | L |
| 333 | `tether_session_peek` requires the `terminal` scope; wider secret redaction; rune-safe truncation | none | H | L (clients without `terminal` scope lose peek) |
| 334 | Launch admission is atomic (slot reservation); `GET /capacity` returns `{environment_id, max_sessions, active_sessions, admission, reason, observed_at}` | route `/capacity` | H | M |
| 335 | Remote scope entry for `/capacity` (fixes a failing route-policy test from #334) | none | H | L |
| 336 | Lineage limits from catalog; bootstrap with `parent_session_id` enforced (409 `lineage_limit_exceeded`); `POST /sessions/{id}/stop?cascade=true`, `tether sessions stop --cascade` | `global.yaml` `defaults.lineage_max_depth`, `defaults.lineage_max_children` (0 or unset = unlimited) | H (config read at start) + C | L until set |

Not in the table because they are not code changes: no schema migration other
than 0077; no new Go dependency was reviewed here **(unverified: `go.mod` diff
not inspected)**.

Open caveat on #334/#335: #334 briefly left `TestRemoteRoutePolicy` red on
`main` until #335 landed. Any build cut between those two commits is bad.

## 2. Deploy order

### Worker-side changes (need a worker swap)

- **#289**: worker only (`internal/workerruntime/mcp.go`).
- **#298**: worker half (`hub-mcp.json` validation) in `workerruntime`; hub half
  in `workerhub`, `app`, `api`. Hub-first is safe: an old worker simply never
  sends the cause text, so the chat error stays the bare form.
- **#286**: both halves; changes `workerwire/protocol.go`. A new hub with an old
  worker should show no orphans and `tether workers reap` should report nothing
  to do or fail for that worker **(unverified: the old-worker behavior was not
  tested)**. Wire negotiation is described in `docs/worker-service.md`; read it
  before mixing versions.

Everything else is hub-only or CLI-only. Hub restart is needed for every row
with H above; all of them are in one binary, so one restart covers them.

### Order

1. **Pre-flight, no change.** `tether doctor` against the hub (it is read-only
   for migrations since #291). Record `tether_health` build commit (expect
   `010fe40`) as the rollback target. Copy the state DB. The path is
   `defaults.state_db` in `~/.tether/catalog/global.yaml` (under `~/tether/`,
   not `~/.tether/`). Migration 0077 adds one table and does not alter any
   existing table, but a copy is still the only DB rollback
   **(unverified: whether an older binary tolerates DB version 77; check
   `docs/install.md` migration handling before relying on that)**.
2. **Catalog first, optional.** Copy `examples/catalog/launches/task-worker.yaml`
   and the `worker-agent.yaml` `stop_after_final_turn` / `agent_prompt` changes
   into the live catalog only after step 3, because an older daemon build
   ignores or rejects unknown lifecycle keys **(unverified)**. Do not set
   `machine_access`, `lineage_max_*` or execution caps before the hub build is
   running; the CHANGELOG already warns that a build predating a field ignores it.
3. **Hub binary.** Build from `main` at or after `c201eb55`
   (`make build`, then install per `docs/install.md`), restart `tetherd`.
   Expect: migration 0077 applied, clean exit of the old process
   (see #299 live check), workers reconnect (past restarts took ~10 s and
   workers reconnected in ~30 s per CW-20261012-0120). The restart mechanism on
   the hub (systemd unit name, command) is not recorded in this repo
   **(unverified)**; use whatever the previous deploy used.
4. **Smoke the hub.** `tether doctor`; `tether execution status` (running, no
   caps); `GET /capacity`; `tether_health` shows the new build commit.
5. **Workers.** Swap one worker first (a test worker), then the rest, since
   #286, #289 and #298 are worker-side. Per `docs/worker-service.md`:
   `tether service restart` after installing the new runtime version under
   `~/.tether/runtime/versions/<semver>/`. Verify the worker reconnects and
   `tether workers` lists it, then run the live checks in section 3 that need a
   worker.
6. **Catalog changes** (step 2 items) through `tether_catalog_plan` /
   operator approval / apply, not by editing the live catalog directly.
7. **CLI** on operator machines: install the new `tether` binary for the new
   flags (`--replace`, `--cascade`, `sessions peek|tasks|retire-shim`,
   `execution`, `workers reap`).

### Rollback

All commands assume the previous binaries are kept next to the new ones;
paths are placeholders **(unverified: actual install paths on the hub and
workers)**.

- **Hub binary:** reinstall the previous build (`010fe40`), restart `tetherd`
  the same way as in step 3. Restore the state DB copy only if the older
  binary refuses to start on a DB at version 77. Restoring the DB discards
  every write since the copy, so prefer leaving it.
- **Execution policy emergency stop without rollback:** `tether execution
  resume`, and `tether execution cap --environment 0` clears the environment
  cap (0 = no cap). The policy defaults to running with no caps.
- **Catalog:** every apply makes a backup named `catalog-pre-<plan>-<ts>`;
  `tether_catalog_backups` lists them and a plan with `rollback_of` restores
  one. #284 also restores files automatically when the live reload is refused.
- **Workers:** `tether service restart` with the previous runtime version
  selected. `docs/worker-service.md` states there is no automatic
  trial/commit or database rollback and that a failed restart retains the
  prior state; read that section before swapping.
- **Config rollback:** remove `defaults.lineage_max_*`, `machine_access`,
  `stop_after_final_turn` from the catalog and reload (`tether_catalog_reload`,
  or apply). `global.yaml` daemon settings need a restart (`restart_required`).

## 3. Live-check checklist

`scripts/post-deploy-check.sh` runs the read-only part of this checklist
against a deployed hub through the `tether` CLI, `GET /capacity` and the
read-only MCP tools, printing PASS/FAIL/SKIP per row id and MANUAL (with the
exact command, not run) for rows that need a state change. It exits non-zero on
any FAIL. Rows `G1`-`G9` are general checks (version, doctor, workers and
`orphaned_hosts`, sessions, MCP tool list, catalog validate/files/launches).
There is no `tether team ls`; row 15 only checks that `tether_team_form` is
exposed.

Source: tasks left in `review` in epics EP-20261013-0001 and EP-20261013-0002
and their comments. Each check was **not run** by the author. "Pass" is what
the task comment says to look for, or what follows from the PR description.

| # | Task / PR | Command or action | Pass looks like |
|---|---|---|---|
| 1 | CW-20261014-0022 / #285, #292 | Launch a headless `worker-agent` worker with `stop_after_final_turn`, let it finish its turn | Session ends with `session.reaper` reason `final_turn` about one grace period (60 s in the example) after the final turn |
| 2 | CW-20261014-0022 / #292 | Repeat, queue a routing reply during the grace window | Session is not stopped until the reply is delivered or settled |
| 3 | CW-20261013-0023 / #296 | Send a routing reply to a session that is not running; inspect `routing_replies` | No row stays `queued` with a growing `updated_at`; row settles `undeliverable/session_ended` after 5 attempts |
| 4 | CW-20261013-0031 / #298 | On a real worker, plant a bad `hub-mcp.json`, launch a session | Worker log has the validation WARN; chat send error reads `session launch failed: <cause>`; hub has a `session.launch_failed` event and a WARN with the session id |
| 5 | CW-20261013-0071 / #295 | Launch a headless session with MCP servers and `permission_mode: default`; read daemon logs | WARN line about default permission mode with MCP servers attached; launch still succeeds |
| 6 | CW-20261013-0074 / #297 | Copy `task-worker.yaml` plus a `tether` project into the live catalog, launch it with `--torque-task` | Launch resolves and the agent follows the closing step. Not verified before: the example's project `tether` must exist in the live catalog |
| 7 | CW-20261013-0075 / #305, #311, #314 | `tether sessions tasks` after a `--torque-task` launch; `tether launch --replace --idempotency-key K ...` against a stopped session, then against a live one | Task to session pair listed; replace on a stopped session rebinds, on a live session returns 409 `idempotency_conflict`; unreachable-daemon errors name the target |
| 8 | CW-20261013-0075 / #300 | `tether launch --idempotency-key auto ...` twice with identical inputs | Same key, second call returns the first session, not a new one |
| 9 | CW-20261013-0080 / #294; CW-20261013-0041 | `tether_catalog_list_launches` on the hub; launch or chat-send against a disabled agent (`zz-probe`, `team-coordinator`) from a session with catalog write; `tether_catalog_reload` | Launch list shows `enabled`; disabled target returns 409 `agent_disabled`; running sessions unaffected; reload with a broken catalog changes nothing. CW-20261013-0041 already confirmed `enabled` is surfaced on build `010fe40`; reload and 409 were deliberately not tested |
| 10 | CW-20261013-0080 | Check the Tangent chat roster while `zz-probe` is enabled | `zz-probe` appears when enabled and is absent when disabled. Roster visibility was never confirmed (the logical-agent list used as a proxy showed it absent in both states) |
| 11 | CW-20261014-0005 / #310 | Apply a change to `teams-policy.yaml` on the hub | Apply reports `restart_required` with a note (not `immediate`) |
| 12 | CW-20261014-0005 / #308 | Plan a bundle, let it expire, re-plan the identical bundle; approve with a real operator approval | Same `plan_id` returned with fresh expiry; plan output shows `ttl_seconds`; approve and apply of an expired plan are refused |
| 13 | CW-20261014-0005 / #306 | `tether_catalog_files` with a `team-definitions/<file>` path | Response includes `pin {id, revision, digest}` matching `tether team pin` |
| 14 | CW-20261012-0120 / #299 | Restart the hub; check `journalctl` for the unit | Unit stops without `status=1/FAILURE`; restart finishes inside `shutdown_timeout` with a stuck stream open. Residual part 2 (worker session stop returns 500 after 30 s) is not fixed and is tracked as CW-20261014-0036 |
| 15 | CW-20261011-0013 / #309 | Form a real 1 coordinator + 2 worker + reviewer team | Members launch with a lifecycle `max_duration`; the reaper stops one that runs past it. Note: digests of team creates in flight during the upgrade change (CW-20261014-0063) |
| 16 | CW-20261011-0020 / #313, #323, #329 | Run a team formation with two allowlisted runtimes, then with one | Reviewer on a different runtime or model with two; with one, formation is allowed and the fallback is logged; formation result carries `ReviewerPairing` for that formation |
| 17 | CW-20261014-0006 / #304 | Register a real local operator session (`tether registry register`, `tether messages claude-hook` per `docs/local-operator-sessions.md`), send it a message with `tether sessions turn --reply-to` | Message arrives in the mailbox; `[reply-to: ...]` line present |
| 18 | CW-20261013-0013 / #307, CW-20261014-0041 / #321, #333 | `tether sessions peek <worker-session-id>`; call `tether_session_peek` over MCP with and without `terminal` scope | Digest with state line, tool calls, FINAL, secrets redacted; MCP call without `terminal` scope is refused before any daemon call. Digest assumes the Claude stream-json shape; other providers not checked |
| 19 | CW-20261011-0015 / #327, #334 | `tether execution status`; `tether execution pause --reason test`; launch (expect 409 `execution_paused`, session stays `created`); `tether execution resume`; set a cap of 1 and start two launches at once; `GET /capacity` | Pause and caps refuse as described; at the cap exactly one of two concurrent launches is admitted; `/capacity` shows `admission` open, paused or full. The cap does not count worker-placed sessions (follow-up filed) |
| 20 | CW-20261014-0051 / #328 | On a session in `outcome_unknown` (e.g. 83a2c232): `tether sessions retire-shim <id>` | Session becomes `orphaned` (`shim_retired_by_operator`) and `session.shim_retired` is emitted. Known: an unreadable placement receipt is refused; Codex sessions need acknowledge_obligations; team members are refused. The gone-host path has only the unit tests from #330 |
| 21 | CW-20261014-0052, 0064 / #324, #336 | Set `defaults.lineage_max_depth: 1` in `global.yaml`, restart; bootstrap or resume a grandchild; `tether sessions stop --cascade <id>` | Over-limit returns 409 `lineage_limit_exceeded`; cascade stops descendants deepest first. Worker-placed descendants are not covered by cascade |
| 22 | CW-20261013-0065, CW-20261014-0061 / #320, #331 | From a session granted Cerberus: call a read tool and a write tool through the proxy and through the daemon shared view; check events for `machine_authority_decision` | Read allowed; write refused and not forwarded for `read-only`; allowed for `machine_access: read-write`; each decision has a durable event. Expected today: daemon views are read-only for Cerberus (handler does not populate `MachineAccess`). Real Cerberus tool names were not checked against the read-verb list |
| 23 | CW-20261011-0038 | none new | Root cause of the Codex `projection_invalid` detach is unproven; nothing in this batch fixes it |
| 24 | CW-20261014-0001 | On the macOS worker host, find what sets `GH_TOKEN` in the worker process environment (launchd plist or env file), replace with the working token, restart the worker, `git push` a throwaway branch | Push succeeds. Blocker is environment, not code (owner action) |
| 25 | CW-20261014-0065 | Run `go test ./internal/definitionresolve -run TestContentRefusesUnsafeNamesAndMembers -v` on macOS | Passes with no skip, or a fold-collision fixture is built without relying on the filesystem. Passes on Linux (7 subtests, 0 skipped) |
| 26 | CW-20261014-0024 | On macOS: `go test ./cmd/tether -run TestBootProxyEnvironmentIsExplicitlyAnonymous` | Passes; if not, record the failure output. Not reproducible on Linux |
| 27 | CW-20261014-0044, 0046 / #318, #319 | `go test ./...` on a Mac | Few or no failures from non-canonical `t.TempDir()`; sshenroll and definitionresolve fixtures skip instead of fail |
| 28 | CW-20261013-0012 | Operator decision, no deploy | Approve or edit the 112-entry registry retire list in the task comment; run binding_list per URN and export JSON first. Nothing was deleted |

Not live-checkable or needing an owner decision before more work:
CW-20261013-0048 (the Torque-side landing hook and non-repo role definitions),
CW-20261011-0010 (survey done, adapters filed as CW-20261014-0053 and
CW-20261014-0055), CW-20261011-0019 (recheck table not yet written to
Tesseract; items 4, 5, 10 need owners), CW-20261013-0006, 0011 and 0063
(design or catalog work with follow-ups, see their task comments).

## 4. Known gaps carried into the deploy

- Spawn budget has no meter (CW-20261014-0042). Only the timeout is enforced at
  launch (#309).
- `--editable-agent` allowlist is never planted by anything (CW-20261014-0047).
- #298 returns sanitized but not redacted worker error text to chat callers.
- Per-parent fan-out rate cap and worker-placed cascade are not built.
- `--replace` delete+create is not one transaction; the API ignores
  `replace_idempotency` without a key; the worker path releases before build
  (CW-20261014-0063).
