# tether-worker MVP (guerilla): design and handoff

Status: decided 2026-10-12 with the operator. Owner of the build: the Mac agent (Codex), full speed.
Branch: `task/tether-worker-mvp` in hollis-labs/tether (base origin/main `beda7a4` + the `--target` header fix
`c1e403b` + the agent's `scripts/wire-hub-mcp.py` `75d3665`). This file lives at `docs/tether-worker-mvp.md` there.
Torque: CW-20261012-0019 (the build), CW-20261012-0017 (hardened worker, post-MVP), CW-20261012-0016 (OS parity).

## 0. Rules for this build

- **Guerilla.** Get a hub-launched Claude session running on a worker, surviving a disconnect, receiving messages.
  No hardening, no polish, no redesign. A smoke script per slice is the test bar. A dedicated hardening session
  comes after we can use the system.
- **Do not change local behaviour.** Every hub change is behind `sessions.worker_id != ''`. Existing local sessions
  must behave exactly as before.
- **Start in this repo**, new binary `cmd/tether-worker`, reusing internal packages. A clean/ideal version (its own
  repo, distinct release) comes later.
- **Fix forward.** Nobody depends on this yet. Small commits to the branch, open the PR early, record progress as
  comments on CW-20261012-0019.

## 1. The decision

The earlier plan treated a worker as a second Tether instance (peer authority, federation, directory, connection
manager, placement). That is wrong. **Tether is the only server.** A `tether-worker` is a thin outpost on a machine
that runs agents for the hub.

| | Hub (Tether daemon) | tether-worker |
|---|---|---|
| Owns | sessions, launches, messaging, teams, registry, history | local process custody, workspaces, sandbox, a journal of output |
| Does not own | execution | any sessions DB, message store, catalog authority |

Consequences: **no federation** (one store, nothing to reconcile); "message delivery to an agent on a worker" is
just the hub sending that session an input command; `environment_directory`, `connection_manager`, placement,
and team-host binding are **cut** from the MVP (they were stubs for the peer model).

**Direction (MVP): the worker dials the hub** (outbound WebSocket over the tailnet). Laptops that sleep or roam need
no inbound path; the Macs need no Remote Login, SSH keys, or host-key pinning for this. The worker also keeps a
local attach endpoint (the existing shim control socket) for re-attach. Server-side discovery of disconnected,
idle or wedged nodes is post-MVP.

## 2. Architecture

```
agent (claude) --stdio/pty--> shim-host (local custody, journal) <--> tether-worker --WSS--> hub /worker/v1/connect
                                   ^ survives channel loss                                        |
                                   +-- local attach socket (c/control.sock)       sessions, events, messages, MCP
```

Three responsibilities of the worker:
1. **Handshake**: one-time pairing code -> device token (scope `worker`), then every connect authenticates with it.
2. **Control channel**: hub sends `launch|stop|input|resize|replay`; worker sends `hello|launched|output|state|exit`.
3. **Custody + reconnect**: agents run under the shim host with a journal; on reconnect the worker reports which
   sessions it holds and the last sequence number, and the hub asks for replay.

## 3. Wire protocol (v1)

Transport: WebSocket (`gorilla/websocket`, already an indirect dep of go.mod, v1.5.3; promote with `go mod tidy`).
Endpoint on the hub remote listener: `GET /worker/v1/connect`, bearer = worker device token, header
`Tether-Protocol: 1` (required by `environment.ProtocolGate`). Public path through Caddy:
`wss://tether.hub.nanite.cloud/worker/v1/connect` (tailnet only; the name already routes to the remote listener
127.0.0.1:8948 and is in the daemon's `--allowed-host`/`--allowed-origin` lists). JSON text frames, one object per
frame, `t` is the type. Ping every 15 s; hub marks a worker offline after 45 s of silence.

Worker -> hub

| t | fields |
|---|---|
| `hello` | `proto:1, worker:{label, version, platform, arch}, caps:<report.Report>, sessions:[{id, state, last_seq, exit_code?}]` |
| `launched` | `cmd_id, session, pid, workspace` |
| `launch_failed` | `cmd_id, session, error` |
| `output` | `session, seq, data(base64), at` (seq is per session, monotonic, gap-free) |
| `state` | `session, state: running|idle|exited, exit_code?` |
| `pong` | `ts` |

Hub -> worker

| t | fields |
|---|---|
| `welcome` | `worker_id, resume:[{session, from_seq}], kill:[session]` (kill = sessions the hub does not know or has finished) |
| `launch` | `cmd_id, session, spec:<LaunchSpec>` |
| `input` | `cmd_id, session, data(base64)` (hub frames turns; worker writes raw bytes) |
| `stop` | `session, grace_ms` |
| `resize` | `session, cols, rows` |
| `replay` | `session, from_seq` |
| `ack` | `session, seq` (optional in MVP; worker never prunes) |
| `ping` | `ts` |

Reconnect: worker retries with 1,2,4,8,15 s capped backoff. On every connect it sends `hello` with its inventory;
the hub answers `welcome` with the replay cursors; the worker re-sends `output` from `from_seq`. The hub dedupes
by `(session, seq)`. Agents are never touched by a channel drop.

Idempotency: `launch.cmd_id` is the hub's idempotency key; a duplicate `launch` for a running session returns
`launched` again.

## 4. LaunchSpec (self-contained, no worker catalog)

`launch.Plan` (`internal/launchprofile/plan.go:8`) is already JSON-serializable and persisted in
`launch_plans.plan_json`, and already carries `Command`, `Args`, `Env`, `BootPrompt`, `BootMode`,
`NativeFiles[]` (rel_path+content), `BootDirOverlay`, `ProviderID`, `RuntimeKind`, `SandboxProfile`. Derive the spec
from it on the hub; the worker never reads the hub catalog. Replace these machine-specific parts:

| In the plan today | In the spec |
|---|---|
| `Command` (likely absolute provider path) | `provider` name (`claude`), worker resolves it locally on PATH |
| `RepoRoot`, `WorkRoot`, `WriteHome`, `WorktreeBase`, `NativeStateRoot` | `workspace:{kind: scratch|git, url, ref, subdir}`; the worker materializes under its own root (`internal/workspace`) |
| `BootProfileFile`, `Shared.MCPFile/BootFile/TransientFile` (paths) | inline `boot_files:{rel_path: content}` |
| MCP config built at launch time (`TetherMCPPlant`, `prepareSharedLaunch`, `session_lifecycle.go:~335-400`) | MVP: the worker plants hub MCP clients itself from `~/.tether-worker/hub-mcp.json` using the logic of `scripts/wire-hub-mcp.py` (torque, tesseract, tether daemon-only over ssh). No local Tether authority is ever created. |
| `SandboxProfile` name; body from `Catalog.AgentSandbox` | `sandbox:{profile, body}` inline; MVP: use it if bwrap/sandbox-exec is usable, else run unsandboxed and report `sandboxed:false` in `launched` |
| session credential minted by `mintSessionCredential` | not needed in MVP (agents reach the hub through the planted MCP entries) |

`spec = {session, provider, runtime_kind, args, env (non-secret), boot_prompt, boot_mode, boot_files, native_files,
workspace, sandbox, resume?}`.

## 5. Hub changes (all gated on `worker_id`)

Anchors (all `origin/main`; verified by a code read unless marked inferred):

1. **Scope**: add `ScopeWorker = "worker"` in `internal/identity/scopes.go` (allow it in `NormalizeDeviceScopes`;
   cap is 5 scopes, fine). Route table: add `routeScope("/worker/v1/connect", ..., {"GET": worker})` in
   `internal/api/remote_scopes.go` (`RemoteRouteScopes` ~:45-195, deny-by-default) and register the handler.
2. **Package `internal/workerhub`** (new): connected-worker registry; the WebSocket handler (upgrade, auth, read/write
   pumps, ping); `Send(workerID, cmd)` with request/response by `cmd_id` and timeout; event ingest. Templates for the
   event/replay half: `internal/environmentstream/server.go` (`after_seq` / `Last-Event-ID`, `Gap{snapshot_required}`).
   There is no existing long-lived command channel and no WebSocket server code: this is new.
3. **Migration `0070_worker.sql`** (latest is 0069): `ALTER TABLE sessions ADD COLUMN worker_id TEXT NOT NULL DEFAULT ''`;
   table `workers(id, label, device_id, platform, caps_json, last_seen, created_at)`; table
   `session_worker_seq(session_id, last_seq)`. Update `SessionRow` and its scans (inferred).
4. **Launch**: `LaunchRequest` (`internal/api/types.go:141`) gains `Worker` (label or id); `tether launch --worker X`.
   In `launchSessionWithContext` (`internal/app/session_lifecycle.go:~225-700`) if `Worker != ""`: reuse
   `BuildLaunchPlan` and `createSessionFromPlan` (`CreateSessionKeyed`, `internal/store/sqlite.go:166`), set
   `worker_id`, derive the spec, `workerhub.Send(launch)`, wait for `launched`/`launch_failed` (30 s), set state.
5. **Output ingest**: on `output`, dedupe by seq, append bytes to the session's `session.log` (so
   `GET /sessions/{id}/log` works, `internal/api/session_logs.go:79-169`), publish on the `events.Bus` with the
   same `kind`s local sessions use (`Store.InsertEvent`, `internal/store/events.go:30`) so `/events`,
   `environmentstream` and the GUI work unchanged. `state`/`exit` update the session row.
6. **Message delivery to a running session** (the real tension): wake sweep `runWakeSweepLoop`
   (`internal/daemon/server.go:~284`) -> `Service.RunWakeSweep` (`internal/app/wake.go:339`); inline path
   `AttemptWake` (`wake.go:323`). The clean seam is `wakeRuntime{health, sendTurn}` built by `runtimeSeam()`
   (`wake.go:302,307`). For `worker_id != ''`: `health` = worker connected and session running; `sendTurn` = frame
   the turn the same way `Service.SendTurn` does (`internal/app/session_io.go:55`, framing by session capabilities,
   `frameUserMessage`) and emit `input` to the worker. Worker offline -> return an error; the sweep parks and retries
   (a failed `sendTurn` becomes `turn-submit-failed`, nacked retryable, `wake.go:638`). Refactor `SendTurn` so framing
   is separate from `Manager.SendInput`. Confirm delivery rows are keyed by session or recipient address, not by
   process (inferred, not read).
7. **Stop/input/resize APIs** for worker-bound sessions route through `workerhub` instead of the local manager.
8. **CLI**: `tether workers` (list: label, platform, connected, last_seen, session count), `tether pair --scope worker`.

Not touched: federation, `team_host_*`, the directory/connection modules, placement.

## 6. Worker: `cmd/tether-worker`

Subcommands: `enroll --hub URL --code CODE --label NAME`, `run`, `status`, `version`.

- **Enroll**: `POST {hub}/auth/pair/exchange` with `{code}` and header `Tether-Protocol: 1`
  (`internal/api/device_auth.go:105`); the client side exists only as the unexported `exchange()` in
  `internal/sshenroll/credentials.go:181`: lift it into a small shared package rather than importing sshenroll.
  Writes `~/.tether-worker/device.token` (0600, `identity.ReadTokenFile` semantics, `tth_` prefix) and
  `config.json {hub, label}`, plus `hub-mcp.json`.
- **Run**: dial, `hello` with `report.Report` (use the detectors directly, no daemon needed:
  `internal/environment/report`: `DetectProviders`, `DetectHosting`, `DetectFilesystem`, `MeasureResources`),
  serve commands, reconnect forever.
- **Launch execution** (the one place that is not copy-paste): `app.Service.launchSessionWithContext` is too heavy
  to import (needs the store). Re-implement a slim version: factory runtime -> `rt.Prepare` -> sandbox -> shim
  `StartOptions` -> `prepareShimStart` (`internal/app/shim_launch.go:105`) -> shim manager `Start`. Importable
  boundary: `internal/shimhost` (placement), `internal/shimbridge`, `internal/workspace`
  (`Create`/`Open` `manager.go:20,55`, `MaterializeWorkRoot` `workroot.go:32`), `internal/provider` (`BuildEnv`),
  `internal/environment/report`. The shim server/journal and `agentsessions.Manager` live in the external module
  `substrate/harness@v0.5.0/shim` (importable). The sandbox wrapper is external too (not located; find it).
- **Custody and reconnect**: the shim journal is `%08d.seg` segments under `j/` with `Replay(session, afterCursor)`
  and `Ack` (`shim/client.go:108`); cursors are strings. The worker keeps `sessions/<id>/cursor-map.json` mapping
  its integer `seq` to shim cursors, tails the shim, and answers `replay` from it. The worker is the sole shim
  controller (shim hello is HMAC-fenced with an epoch; takeover bumps the epoch, `shim/server.go:140-235`); the hub
  is fenced at the channel level. Surviving a worker restart: re-adopt shims like `reattachShim`
  (`internal/app/shim_recovery.go:137`).
- **Fallback if shim reuse costs more than a day**: run the provider in a detached `tmux` session with
  `pipe-pane` to a journal file (Cerberus remote-execution sketch, "simplest MVP"); `input` -> `tmux send-keys`/
  `paste-buffer`; local re-attach = `tmux attach`. Mark it clearly; replace later.
- **Service**: Linux systemd user unit and macOS launchd plist, written by `tether-worker enroll --install`
  (or documented by hand for the MVP). Binary self-update is post-MVP.

## 7. Slices (each ends with a smoke script and a Torque comment)

| Slice | Deliverable | Done when |
|---|---|---|
| **S0 ops** (Claude/operator, parallel) | hub binary deploy recipe; worker accounts can `ssh chrispian@hub` unattended; pairing codes | listed in section 9 |
| **S1 hub channel** | scope, route, `internal/workerhub`, `workers` table, `tether workers`, ping/hello/welcome; a fake worker (test) can connect | `tether workers` shows a connected fake worker |
| **S2 worker skeleton** | `cmd/tether-worker` enroll + run + reconnect + caps in `hello` | real worker-1 enrolled and shown connected; kill the hub, it reconnects |
| **S3 launch** | migration, `--worker`, spec derivation, worker executes launch (shim or tmux fallback), stop | `tether launch --launch tether-claude --worker worker-1` starts Claude on worker-1; `stop` ends it |
| **S4 output + replay** | `output` ingest, `session.log`, events, seq/dedupe/replay, disconnect survival | output visible via `/sessions/{id}/log` and the GUI; drop the connection mid-run; agent keeps running; after reconnect no gap and no duplicate |
| **S5 messaging** | `runtimeSeam` routing, `input` command | `tether_message_send` to the agent's address reaches the agent on worker-1 and it replies; worker offline -> parked then delivered on reconnect |
| **S6 real task + Macs** | hub MCP planting on the worker, one real task each on worker-1 and agent-os; darwin build + launchd; enroll macbook-1 | agent calls `torque_health`, `tesseract_recall`, `tether_whoami` through the planted MCP; a Mac runs a session |

## 8. Post-MVP (explicitly out)

Server-side discovery of disconnected/idle/wedged nodes; capability-based placement and resource reservation;
a durable directory; `ack`-driven journal pruning; worker self-update and signed releases; credential broker /
secrets handling beyond file-based device tokens; reaper and idle policy; hub-side fencing epochs on the channel;
owning-environment on every session (also silences the foreign-session wake-sweep noise, design-smells 11/34);
Windows; distinct repo/binary hardening (CW-20261012-0017, OS parity -0016); retiring `tether env add` /
`tether service` and the full-Tether worker; team-host binding to a worker.

## 9. Operational facts (so nobody has to rediscover them)

- **Hub**: `100.74.49.44` (tailnet), user `chrispian`, host key `SHA256:bRUb1YRc6LI38gN9fEgkxvGi9YJHZRbRMA8/r/bZplU`.
  Tether runs as the Cerberus resource `hub-tether` (`/var/lib/fabric/config/hub-fabric.cerberus.yaml`):
  `/var/lib/fabric/bin/tether --catalog /var/lib/fabric/tether/catalog serve --remote-listen tcp:127.0.0.1:8948
  --allowed-host tether.hub.nanite.cloud --allowed-host tether.nanite.cloud --allowed-origin ...`.
  Deploy a new hub binary: build linux/amd64, copy to `/var/lib/fabric/bin/tether`, then
  `cerberus resource reload hub-tether --ack` (**`apply` does not restart a running daemon; `reload` does**).
  Logs: `~/.cerberus/apps/hub-fabric/hub-tether/logs/stderr.log`. API name (device tokens, WebSocket):
  `tether.hub.nanite.cloud`; GUI name (tailnet gate, Sysop): `tether.nanite.cloud`.
- **Hub config traps**: Cerberus `command:`/`env:` values must be quoted strings (unquoted numbers are silently
  dropped). The hub's `global.yaml` is the merged one (`ai:`, `teams`, identity `teams: enforce`); the operator
  token is agent-os's (`run/operator.token`), matched with the migrated DB.
- **Workers** (full-Tether workers enrolled today, to be replaced): `tether-worker` account on agent-os and worker-1,
  Claude installed and logged in under that account, linger on, bwrap usable. Hub aliases `agent-os-tw`,
  `worker-1-tw` (key `~/.ssh/id_ed25519_machines`). Old services: `tether-worker.service` (user), loopback :7181.
  Stop them (`systemctl --user stop tether-worker`) when the new worker runs, to avoid two Tether authorities.
- **Macs**: macbook-1 = `home-laptop` (login `chrispian`), macbook-2 = `work-laptop` (login `cburks`); operator
  enables what is needed. Claude login is interactive. Hub MCP clients already wired in their user configs.
- **Hub MCP for agents**: torque (`torque-mcp-hub` wrapper, token file `~/.config/torque/hub.token`), tesseract
  (`ssh -T ... chrispian@hub /var/lib/fabric/bin/tesseract-mcp-stdio <label>`), tether
  (`ssh -T ... chrispian@hub /var/lib/fabric/bin/tether --catalog /var/lib/fabric/tether/catalog mcp --daemon-only
  --token local-dev --scopes message.write,registry.write,groups.write`). These need `ssh chrispian@hub`
  unattended from the worker's account (known_hosts pinned); that is S0.
- **Pairing**: on the hub, `tether pair --scope worker --ttl 10m --label <name>` prints a one-time code (needs S1).
- **Builds**: `CGO_ENABLED=0 go build ./cmd/tether-worker` per OS/arch; `scripts/release.sh` builds all four
  platforms and the sysop frontend (slow, and deletes `apps/sysop/internal/webui/dist/.gitkeep`: restore it with
  `git checkout`). Always build from the commit that is deployed on the hub, not the local `main` (stale).
- **LLM gateway** is live on the hub (14 providers; keys in `/var/lib/fabric/config/llm/`).

## 10. Risks (accepted for the MVP)

1. Reusing `substrate/harness` shim/manager from a thin binary may drag more than expected: use the tmux fallback.
2. WebSocket through Caddy and the exact Host/Origin allowlist: non-browser clients send no Origin; verify early (S1).
3. The sandbox wrapper is an external module: run unsandboxed with `sandboxed:false` rather than block.
4. `launch.Plan` has machine paths; the spec derivation is the main correctness risk. Test with a Claude launch
   on a different machine than the hub's catalog assumes.
5. The agent's `tether` MCP entry needs unattended SSH from the worker account to the hub (S0).
6. Two Tether authorities on one account if the old worker service is left running.
