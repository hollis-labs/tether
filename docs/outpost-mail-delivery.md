# Outpost mail delivery MVP

Build task: CW-20261012-0022 through -0027. Branch starts at the deployed
`99e7973`. S7 (-0028) real plugin installation/trust remains a separate task;
Antigravity upstream integration remains -0029. The prototype sources and live
operator dev mod are not modified by this branch.

Tether is still the only authority. An Outpost registers unmanaged agent routes
with the hub over its existing worker WebSocket. The hub sends only data:
`deliver{cmd_id,message_id,urn,binding,text}` -> `deliver_ack{cmd_id,error?}`.
No adapter names, local command paths, session credentials or native handles go
to the hub. Managed launches keep their shim `input/input_ack` path.

## Local boundary and commands

The Outpost listens on a group-owned 0660 Unix socket. Peer uid must be in its
configured allowlist (default own uid). The registered PID must belong to that
uid, and its OS start time must match. Linux uses SO_PEERCRED; Darwin uses
LOCAL_PEERCRED. Register returns a random capability; renew, deregister and
cross-connection native acknowledgements require it. Leases default to 60s,
heartbeat is 20s, max lease 120s. Private 0600 state preserves capability,
identity, expiry and the latest 256 successful message acknowledgements.

For a worker already enrolled:

```sh
tether-worker --root <outpost-state> mail configure \
  --socket <shared-dir>/mail.sock --gid <shared-group-id> \
  --allow-uid <outpost-uid>,<agent-uid>
```

Restart the Outpost service to apply. Alternatively use enroll's
`--mail-socket`, `--mail-gid`, `--mail-allow-uid` flags. The operator provisions
the shared directory and group membership: its parent directories must be
traversable by the allowed agent uid, and the Outpost user must be able to
chown its socket to that group. Do not make the private worker root/token
group-readable. Configuration does not automatically alter system groups or
directory ownership.

An unmanaged agent's uid runs the credential-free adapter helper:

```sh
tether-worker mail --socket <shared-dir>/mail.sock attach \
  --name my-agent --adapter codex-queue --handle <thread-id> --pid <agent-pid>
tether-worker mail --socket <shared-dir>/mail.sock attach \
  --name pane-agent --adapter tmux-paste --handle '%7' --pid <agent-pid>
```

This helper is the local adapter-execution half of the Outpost binary, not a
second daemon or authority. It must run as the agent's uid because Codex's
managed app-server and tmux are same-uid services. It receives data over the
socket and invokes only fixed argv for its locally selected adapter. There is
no sudo/su policy and no hub bearer token in the helper. Codex hooks can spawn
it in place of their private watcher/relay, with SessionStart session_id as
the thread handle. The helper exits when the registered agent PID/start time
ceases to match. No PTY/ANSI reading occurs.

The hub mints `msg://agent/<worker-id>/<name>` (not the non-unique worker label).
An existing URN needs an operator grant:

```sh
tether-remote workers --json
tether-remote workers grant <worker-id> msg://agent/agent-mux/my-agent
tether-remote workers grant <worker-id> msg://agent/agent-mux/my-agent --remove
```

Grant removal ends its live route. Device-token revocation ends all routes on
that connection. Revocation/expiry never kills the unmanaged agent.
`attach --urn <granted-urn>` selects the granted identity. `--allow-sender`
adds exact sender URNs; default only `msg://agent/agent-mux/operator` and
`msg://user/local/operator` may wake it. These are still self-asserted sender
identities under the existing messaging convention, not verified authorship.

## Adapters and Claude mod

tmux-paste cancels view mode, loads literal text, does bracketed paste, then
sends C-m in a separate call. `--escape-first` is opt-in for Codex TUIs that
need Escape before accepting input. Agy gets tmux-paste only in this MVP.
Human typing can collide; folder-trust prompts are not solved by send-keys.

codex-queue invokes `codex queue --thread <id> --message <text>` with no shell.
It inherits the registering user's environment (including CODEX_HOME).

`plugins/tether-mail/claude` is the replacement mod source. It uses
`$.process.spawn` to run `tether-worker mail attach --adapter claude-mod`;
`$.prompt.submit` accepts the full framed message, then `$.process.run`
invokes `mail ack` with capability JSON on stdin. Claude's mod loader rejects
node:net, so the thin binary bridges the local socket through the supported
process API. Configure `socket` (or TETHER_MAIL_SOCKET), `binary`, a unique
`name`, optional granted `urn`, and comma-separated `allowSenders`.

Do not leave the old private watcher active on the same URN during rollout.
An Outpost binding owns push; mailbox watch is a non-destructive fallback and
does not itself suppress duplicate UI submissions from an old relay.

## Semantics and limits

Bindings are short leases, refreshed/re-sent in reconnect hello. The hub's
live route map is not persisted. The existing registry records generation
history; disconnect revokes the current route. After a crash/lease expiry,
that history fences fallback to an unrelated same-ID local session. One live
Outpost registration per URN/name is accepted; a duplicate is refused, not
chosen by newest timestamp. The hub refuses unmanaged registration for an
already-live managed actor; before submission it also fences generation.

The existing wake admission, durable Claim/Ack/Nack and retry sweep are reused.
Only actual adapter acceptance returns success. Successful Outpost delivery
sets delivered_at, never read_at/consumed_at. Bodies are framed as data, but
that framing is convention, not a prompt-injection security boundary. Full
body is included without the prototype's clipped preview.

MVP loop controls: exact sender allowlist, `notice` never wakes, at most one
delivery attempt per URN per 1.5s. Denial/rate failures are visible in the wake
outcome and use existing bounded retry. Verified senders, hop/depth, budgets,
batching, kill switches, Agy hooks/agentapi and Windows are deferred.

Known residual: a native acceptance followed by a crash before its receipt is
persisted is uncertain; this is not end-to-end exactly-once execution. The
last 256 successful IDs are deduped locally; old IDs are protected by hub
admission in ordinary operation. Leases, not automatic agent restarts, handle
lost helpers. Restarting a helper without its old capability waits for the
old lease/process identity to expire. Linux cross-uid deployment and real
Claude/Codex idle/mid-turn acceptance still need operational proof.

Channel prerequisites included: up to four launches run independently with
main's four-minute worker deadline (below the five-minute hub wait), so the
bounded command dispatcher can still deliver stop/input during a slow launch.
Welcome resets reconnect backoff; replay is capped
at 256 frames/1MiB per batch with explicit continuation. This is not a replay
retention/compaction or hardening project.

## Verification and rollout

```sh
for slice in 1 2 3 4 5 6; do bash scripts/outpost-mail-smoke-s${slice}.sh; done
go test -race ./internal/outpostmail ./internal/workerhub \
  ./internal/workerruntime ./internal/app -run 'TestOutpostMail|TestSmokeS[12]$'
bash scripts/worker-smoke-s4.sh
bash scripts/worker-smoke-s5.sh
bash scripts/worker-smoke-s6.sh
```

S3 owns an isolated native tmux fixture and a fake Codex argv fixture. S4 uses
the actual Claude mod test loader but mocks prompt submission, not a logged-in
Claude session. S2/S5/S6 use real sockets, authenticated WebSockets and SQLite,
not the production hub. Hub deployment remains lead/Claude-owned; deploy hub
first, then Outposts built from the same candidate. Provision the group/socket
boundary, replace old watchers, and prove real unmanaged Claude/Codex delivery
idle and mid-turn on scratch URNs before enabling agent-to-agent senders.
Mac enrollment/login remain operator-owned. No full make check/hardening claim.

Broader app verification finds `TestTeamRecoveryReplacesLostExecutionAndRetainsBinding`
failing with `codex_explicit_provider_home_required`. The exact isolated test
also fails on unchanged deployed/base `99e7973`; it is not fixed by this change.

CI follow-up: main `a6d37cc` is merged, preserving its repo_url, permission
posture and dead-shim recovery fixes. The initial CI lint findings are fixed;
Linux lint is checked with CI's pinned golangci-lint v2.14.0, not the older
preinstalled Mac tool. Full Linux CI remains the authority for its race suite.
