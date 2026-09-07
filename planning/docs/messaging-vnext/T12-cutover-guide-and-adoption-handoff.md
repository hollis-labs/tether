# T12 — Platform-ready cutover guide and consumer adoption handoff

Task: CW-20260906-0043. Initiative: messaging-vnext-20260906. Working directory:
`/Users/chrispian/dev/hollis-labs/apps/tether`.

## 0. What this document is, and is not

This is the **platform-ready handoff** for the messaging vNext epic (T01–T11, all
landed on `main` in this repository). It documents what Tether now offers, how to
turn it on, how to troubleshoot it, and exactly what each of the three named
consumers (agent-setup, Nanite, Torque) needs to do to adopt it.

It is **not**:

- A claim that agent-setup, Nanite, or Torque have adopted anything. C01/C02/C03
  are separate, independent tasks in their own repositories/sessions, each
  depending on this one. Fixture-shaped proof that the seams work (this
  repository's own `e2e/` package) is not the same thing as real adoption.
- Authorization to tag, push, install, or activate a live daemon against real
  data. Every command below that would do one of those things is written as
  something *you* run when ready, not something this session ran.
- A claim that T13 (final cross-consumer rollout verification) can start. T13
  depends on this task plus all three consumer tasks landing and Chrispian
  separately authorizing local integration runs.

## 1. Status summary

All eleven prior tasks in this epic are `done` in Torque and merged to `main`:

| Task | Torque ID | What it landed |
|---|---|---|
| T01 | CW-20260906-0033 | Baseline/version matrix, current-path inventory, migration contract — `planning/docs/messaging-vnext/T01-compatibility-contract.md` |
| T02 | CW-20260906-0034 | Durable participant registry, canonical sessions, leased runtime bindings |
| T03 | CW-20260904-0100 | Migrated Tether persistence onto go-messaging's reliable delivery core |
| T04 | — | Durable group fanout (frozen per-recipient delivery rows at send time) |
| T05 | — | Recipient-scoped mailbox reads (`?as=`), ADR 0045 same-host trust model |
| T06 | — | Durable hosted-session handoff, route fencing, capability-based steering |
| T07 | — | Published-local bridges, pull-only opt-in, reconnect-safe federation |
| T08 | CW-20260906-0039 | CLI/MCP/go-tether-client parity, `whoami`, launch-bootstrap helper |
| T09 | CW-20260906-0040 | Delivery trace, authorized repair/redrive, privacy-safe retention |
| T10 | CW-20260906-0041 | Bounded A2A interoperability adapter (server-only inbound relay) |
| T11 | CW-20260906-0042 | Adversarial security/durability review + real-process `e2e/` suite |

`make check` is green on the landed baseline: 1659 tests (race-enabled), 0
`golangci-lint` issues, 0 `govulncheck` vulnerabilities, 66.9% aggregate
coverage. Every task above carries its own Torque evidence trail (subtodo
evidence text, cited commit SHAs) — this document doesn't repeat that detail,
it synthesizes what a consumer or operator needs from it.

## 2. Consumable versions / SHAs and safe release order

| Component | Current state | What needs to happen before a consumer can pin a released version |
|---|---|---|
| `github.com/hollis-labs/tether` (this repo) | `main` @ `75f80e8` (this closeout) | Nothing blocking — `main` is the landed baseline. No tag has been cut for this epic; cutting one (e.g. `v0.6.0`) is an explicit release action left to Chrispian. |
| `github.com/hollis-labs/go-messaging` | Tether's `go.mod` is now pinned to **`v0.5.1`** (tagged and pushed to `origin/main` as part of this closeout, commit `3964a1b`) — a real released fix for a late-`Nack`-reverses-a-completed-`Consume` race an independent review found (§7), not present in `v0.5.0`. | Done. Nothing further needed here; `v0.5.1` is a real, pushed, tagged release on GitHub. |
| `github.com/hollis-labs/go-tether-client` | `main` @ `a8fb61c`, one commit past `fa4df9a` (session-bootstrap helper, T08) — the new commit fixes the `?as=`-omitting `Get`/`Inbox`/`Thread`/`Subscribe` calls this task's own review found (§7); **not pushed to `origin` yet** | A new tag (e.g. `v0.2.0`) needs to be cut at `a8fb61c` (not `fa4df9a` — that commit predates the `?as=` fix and would ship a client every one of these four calls fails against). Its own `go.mod` still pins `go-messaging v0.2.0` — by its own CHANGELOG note this is a **deliberate, already-documented deferral** ("nothing added in this change depends on go-messaging at all, and bumping it is a separate, independently-reviewable change"), not an oversight. Bump it to the `v0.5.1` tag above in the same pass, or explicitly leave it and note why in that release's own changelog entry. |

**Safe release order**, if/when Chrispian authorizes cutting the remaining tags:

1. ~~Bump Tether's `go.mod` to a released `go-messaging` tag.~~ Done — pinned to `v0.5.1`.
2. Push `go-tether-client`'s `a8fb61c` to `origin`, update its `go.mod` to the `go-messaging v0.5.1` tag (or explicitly decline to, per its own CHANGELOG precedent), and tag it at the resulting commit.
3. Tag `tether` itself at `75f80e8` (or later, if more work lands first).
4. Only then should any consumer's `go.mod` be updated to reference tagged versions instead of `main`/pseudo-versions.

Nothing above requires a schema migration to run against a live database before
tagging — the schema changes this epic itself introduced (migrations `0019`
T02, `0020` T03, `0021` T04 — see §6) are additive and already exercised by
the landed test suite, including a real-process upgrade drill
(`e2e/upgrade_test.go`) against a pre-migrations fixture. Migrations `0015`
through `0018` predate or are unrelated to this epic (v060-01 registry,
v060-05 group messaging, v060-02 registry dedup, and an unrelated AI-gateway
audit, respectively) and are not this epic's to characterize.

## 3. Platform capability reference

Everything below is live on `main` today, reachable through the daemon's real
public surface (`internal/api`), and has HTTP + typed Go client
(`internal/client`) coverage; CLI (`mux ...`) and MCP tool coverage is called
out per area. Use `mux <command> --help` / `mux mcp` for the authoritative,
always-current flag/parameter reference — this table is deliberately a map,
not a copy of `--help` output that will drift.

### 3.1 Registry & identity (T02, T09 privacy)

- **HTTP**: `POST /registry/{agents|projects|groups}` (register), `GET
  /registry/{kind}` (search — **redacted**, see below), `GET
  /registry/{kind}/{urn}` (lookup — **redacted**), `PATCH
  /registry/{kind}/{urn}` (update self), `DELETE /registry/{kind}/{urn}`
  (deregister/soft-delete), `POST /registry/{kind}/{urn}/sync`, `POST
  /registry/{kind}/{urn}/merge`, `POST /registry/bootstrap`.
- **Redaction (T09)**: Search and Lookup responses never include `Callback`,
  `HostAddress`, `KindMeta`, or `ExternalIDs` — these are only visible to the
  row's own owner via the write-path endpoints above, or via `GET
  /whoami?as=<urn>` (self-discovery; also redacted as of T11's security fix —
  see §7).
- **CLI**: `mux registry register|lookup|lookup-by|search|update-self|merge|deregister|bootstrap|sync`.
- **MCP**: `tether_registry_register|lookup|lookup_by|search|update_self|merge|deregister|sync`.
- **Client**: `internal/client.RegistryClient` (`c.Registry()`).

### 3.2 Runtime bindings (T02, T06, T07, T11 durability)

- **HTTP**: `POST /registry/bindings` (lease — **published-local + pull-only
  only**; Tether's own internal launch path leases `private-local` bindings, not
  reachable via this public endpoint), `POST
  /registry/bindings/{id}/renew`, `POST /registry/bindings/{id}/revoke`, `GET
  /registry/bindings?target_urn=&current=true` (current) / `GET
  /registry/bindings?target_urn=` (full generation history).
- **Visibility model** (`registry.PublicationVisibility`): `private-local`
  (Tether's own launched sessions), `published-local` (external bridges, the
  only kind the public lease endpoint creates), `tether-hosted` (**defined,
  never actually creatable by any code path today** — see §9, known gap).
- **CLI**: `mux registry bindings lease|renew|revoke|current|list`.
- **MCP**: `tether_registry_binding_lease|renew|revoke|current|list`.

### 3.3 Groups (T04, ADR 0042)

- **HTTP**: `POST /groups` (create), `GET /groups?member=<urn>` (list for
  member), `GET /groups/{urn}` (lookup), `DELETE /groups/{urn}` (archive),
  `POST /groups/{urn}/members` (add) / `GET /groups/{urn}/members` (list),
  `DELETE /groups/{urn}/members/{member}` (remove) / `PATCH
  /groups/{urn}/members/{member}` (set role), `POST /groups/{urn}/leave`,
  `POST /groups/{urn}/messages` (send) / `GET /groups/{urn}/messages` (list —
  non-destructive, cursor-based), `POST /groups/{urn}/read` (mark-read
  cursor), `GET /mentions`.
- **Durability guarantee**: fanout freezes one delivery row per member **at
  send time** — a member added or removed afterward never retroactively
  gains or loses that specific delivery (proven at both the unit level,
  T04's own tests, and the real-process level, `e2e/registry_test.go`'s
  `TestRegistry_GroupMembershipReplacement_RealDaemon`).
- **CLI**: `mux group create|list|show|invite|kick|leave|post|read|mentions|archive`.
- **MCP**: `tether_group_create|list_for_member|lookup|invite|kick|leave|post|read|mark_read|mentions|archive|list_members|set_role`.

### 3.4 Messaging & delivery core (T03, T05)

- **HTTP**: `POST /messages` (send), `GET /messages/{id}?as=`, `DELETE
  /messages/{id}` (archive), `GET /messages/inbox?to=&as=` (destructive
  pull), `GET /messages/list?to=&as=` (non-destructive), `GET
  /messages/thread/{id}?as=`, `GET /messages/subscribe?to=&as=` (SSE — now
  requires `?as=`, T11 fix), `POST /messages/notify` (send + best-effort
  wake), `POST /messages/{id}/consume|cancel|read|archive|unarchive`, `POST
  /messages/{id}/claim|ack|nack` (durable claim primitives — no typed Go
  client wrapper exists yet for these three; see §9's known-gaps table).
- **Trust model**: every `?as=`/mailbox-owner check is a **self-asserted
  claim**, not cryptographically verified (ADR 0045). This is a deliberate,
  disclosed, same-host design — do not build consumer logic that assumes
  Tether verifies caller identity beyond "well-formed URN was supplied."
- **CLI**: `mux messages send|notify|get|inbox|list|thread|consume|cancel|read|archive|unarchive`.
- **MCP**: `mux_message_send|notify|get|inbox|list|thread|consume|cancel|mark_read|archive|unarchive`.

### 3.5 Delivery trace, repair, retention (T09)

- **HTTP**: `GET /messages/{id}/trace` (who→whom, binding/host resolution,
  attempt/receipt history, why retry/dead-letter happened), `POST
  /messages/{id}/redrive` (authorized retry of a dead-lettered delivery —
  idempotent, frozen-fanout-safe), `GET
  /messages/retention/candidates?older_than_hours=` (read-only preview),
  `POST /messages/{id}/purge` (clears body/metadata of a message whose
  delivery has reached a terminal, non-redrivable state — refuses on
  anything still pending or still dead-lettered-and-redrivable).
- **CLI**: `mux messages trace|redrive`, `mux messages retention candidates|purge`.
- **MCP**: `mux_message_trace`, `mux_message_redrive`, `mux_message_retention_candidates`, `mux_message_purge` — both `mux_message_redrive` and `mux_message_purge` (not just purge) are gated behind the `delivery.write` scope.
- See §5 for a worked troubleshooting walkthrough using these.

### 3.6 Session bootstrap & self-discovery (T08, T11 privacy fix)

- **HTTP**: `POST /sessions/bootstrap` (idempotent canonical-identity
  registration for an externally-launched session; T11 closed a
  concurrent-call race — see §7), `GET /whoami?as=<urn>` (self-discovery:
  redacted profile + external IDs + group memberships + current binding).
- **Client**: `internal/client.Client.BootstrapSession`; `go-tether-client`'s
  `ResolveSessionBootstrap` is the intended agent-setup-facing wrapper (see §8.1).

### 3.7 A2A interoperability adapter (T10)

- **Scope**: server-only inbound relay. Tether exposes selected
  registered agents as A2A (Agent2Agent protocol v1.0) servers; it never
  initiates an outbound A2A call (that half is backlog, CW-20260907-0028).
- **Opt-in config**: `<catalogRoot>/a2a/*.yaml` (one file per exposed agent —
  see §4.2 for the exact shape). Absent entirely when unconfigured; nothing
  about plain Tether messaging changes if you never create this directory.
- **Mounted at**: `/a2a/agents/{binding-id}/.well-known/agent-card.json`
  (discovery, unauthenticated by protocol convention), `/a2a/agents/{binding-id}/rpc`
  (JSON-RPC, bearer-token gated when `bearer_token` is set), `/a2a/agents/{binding-id}/tasks/{taskID}/transition`
  (authorized, binding-scoped task-outcome resolution for delegated work).
- **Design guarantees**: A2A message/context/task IDs are carried only as
  envelope metadata, never as the Tether message ID or a stand-in for
  AgentID/SESSION; whether a message becomes a delegated task is an explicit
  per-binding config choice (`task_mode`), never inferred; Tether never
  decides a delegated task's outcome itself — only the transition endpoint,
  under its own binding-scoped bearer-token check, can.
- **No CLI/MCP surface** — this is a catalog-file-configured, externally-facing
  protocol adapter, not an operator action surface. See §9 for the one
  standing architectural gap (network-boundary separation, backlog
  CW-20260907-0029) to read before exposing this beyond localhost.

## 4. Sample boots and opt-in registration

### 4.1 Registering a durable actor and leasing a published-local (pull-only) binding

```bash
# Register once, get back a stable URN. --file points at a YAML/JSON Profile
# document (display_name, etc.); --print-urn-only makes this script-friendly.
mux registry register --kind agent --file ./my-actor-profile.yaml --print-urn-only
# → msg://agent/agent-mux/agt_xxxxxxxxxxxx

# Each new host session leases a binding (pull-only is the only capability
# the public lease endpoint accepts — see §3.2).
mux registry bindings lease \
  --target-urn msg://agent/agent-mux/agt_xxxxxxxxxxxx \
  --session-id sess-1 --host-id host-1 --attempt-id attempt-1 \
  --capabilities pull-only --ttl-seconds 3600
```

### 4.2 Opting one agent into A2A (T10)

Create `<catalogRoot>/a2a/<binding-id>.yaml`:

```yaml
id: greeter                                    # path segment: /a2a/agents/greeter/...
target_urn: "msg://agent/agent-mux/agt_xxxxxxxxxxxx"
display_name: "My Public Greeter"
description: "Relays A2A messages into Tether's canonical mailbox."
base_url: "https://your-public-host/a2a"       # the adapter's OWN mount root —
                                                # do NOT include /agents/<id> here,
                                                # the adapter appends that itself
bearer_token: "keychain://a2a/greeter-token"    # or a literal / ${ENV_VAR} — see
                                                # internal/config/a2a.go's secret-ref
                                                # resolution, same mechanism as
                                                # mcp-servers/*.yaml
task_mode: false                                # true only if this binding should
                                                # accept delegated work, not just
                                                # plain messages
```

No daemon restart is required for other catalog changes in this epic (agents,
groups, bindings are all live-API-driven), but A2A bindings are loaded at
daemon startup (`buildA2AAdapter` in `cmd/mux/daemon.go`) — a new or edited
`a2a/*.yaml` file needs a daemon restart to take effect.

**Before exposing this beyond localhost**, read §9's network-boundary note
(backlog CW-20260907-0029) — `/a2a/` is mounted on the same listener as every
other same-host-trust route today.

## 5. Delivery troubleshooting walkthrough

Given a message ID (or, for a group-fanout recipient, that delivery's own ID —
`trace` doesn't resolve group deliveries by message ID, only `redrive` does):

```bash
mux messages trace <id>
```

This answers, in one call: who sent to whom; which binding/host generation
accepted it; which attempt(s) were made and when; and — if it's stuck — the
exact error and whether it's retryable. Typical next steps by what `trace`
shows:

| `trace` shows | Likely cause | Action |
|---|---|---|
| `status: pending`, no attempts | Recipient has no live binding, or hasn't pulled yet | Normal for an offline/pull-only actor. Not a bug — see T11's `capability_honesty_test.go` for why this is the safe default. |
| `status: dead_lettered` | Ran out of retries (transient failure, or a permanently-broken handler) | `mux messages redrive <id> --authorized-by <your-urn>` — idempotent, safe to call more than once. |
| `status: delivered` but recipient claims not to have seen it | Recipient may be looking at the wrong mailbox address, or already consumed and moved on | Check `attempt_count` and the attempt's `holder`/`host_id` against what the recipient expects. |
| Attempts exist but `host_id` is empty | The binding generation that accepted it has since been superseded/revoked | Informational only — the message itself is unaffected. |

For bulk hygiene once messages are old and safely terminal:

```bash
mux messages retention candidates --older-than-hours 720   # 30 days, read-only preview
mux messages retention purge <id> --authorized-by <your-urn>  # one at a time, explicit
```

`purge` refuses (409) anything still pending, leased, retry-scheduled, or
dead-lettered — a dead-lettered delivery is still redrivable, and purging its
body first would make a later redrive resend an empty message. There is no
bulk-purge endpoint and nothing purges automatically, by design (T09
acceptance #3: "no silent deletion").

## 6. Compatibility & retirement guidance

T01's compatibility contract (`T01-compatibility-contract.md`, §2.5, §2.6,
§2.10) identified three legacy/parallel paths at baseline. Current status
after T02–T11:

- **`internal/broker` / `broker_envelopes`**: still present, still has live
  write paths (`/broker/envelopes`, `/broker/requests`), and its own CLI/MCP
  surface. Messaging vNext's canonical path (`/messages/*`, delivery-core
  backed) is now the actively-developed and actively-tested one. **Not yet
  retired** — no task in this epic removed it. **This is a gap against T01's
  own execution contract**: T01 §3.3 explicitly assigned "final retirement
  timeline for `/broker/*` and for `logical_agents.claude_session_id` as a
  write path" to this task ("T12's 'legacy endpoint disposition'"). This
  document does not deliver that timeline — `SetClaudeSessionID`
  (`internal/store/logical_agents.go`) remains a live write path today, and
  no deprecation window has been set for either it or `/broker/*`. Flagging
  this explicitly rather than silently re-deferring it: if a timeline is
  needed before this handoff is treated as complete, it should be decided
  and added here (or filed as its own dedicated follow-up task) before T13.
- **Federation Router** (T01 §2.6, at baseline: "fully implemented, fully
  tested, and never actually invoked at runtime"): this is no longer
  accurate. T05 (this epic) wired it into the live daemon via
  `cmd/mux/daemon.go`'s `newFederatedMessageStore` and
  `internal/app/service.go`'s `federation.BuildRouter` — it is live-wired
  into the real Send/Inbox/Subscribe path today, opt-in and off-by-default
  behind `federation.enabled`/configured peers. What remains genuinely open
  is narrower than "is it wired up": whether Tether's own Federation Router
  type should eventually be retired in favor of go-messaging's built-in
  equivalent, now that both exist side by side. Flag that narrower question
  for T13 or a dedicated follow-up rather than assuming either answer.
- **`go-messaging`'s `mailbox` subpackage** (T01 §2.11, tuple addressing, not
  URNs): not adopted anywhere in this epic. If a future task considers it,
  treat it as a re-addressing decision requiring its own review, exactly as
  T01 flagged.

This epic's own migrations are `0019` (T02), `0020` (T03), and `0021` (T04) —
none of the three drops or renames a pre-existing column or table; all are
additive. (Migrations `0015`–`0018` predate or are unrelated to this epic —
see §2 — and are not characterized here; `0016` in particular does perform a
real `DROP TABLE`/`RENAME`, but as part of the unrelated v060-05 group
messaging sprint, not this one.) `e2e/upgrade_test.go` proves a real
pre-migrations (v0.0.1-schema) database boots cleanly through the current
binary with the legacy row intact — the safest possible signal for a
consumer's own upgrade path.

## 7. Fixed during this epic's own review passes (relevant to any consumer relying on prior behavior)

Three adversarial/fact-check review passes (T09's, T11's, and this task's own
follow-up closeout pass, prompted by a further independent review of this very
document) found and fixed real, shipped behavior a consumer should know
changed:

- **`GET /whoami`** used to return a fully unredacted registry profile
  (`Callback`, `HostAddress`, `KindMeta`, `ExternalIDs`) for any URN a caller
  named. As of T11, `Profile` and `Groups` in the whoami response are redacted
  the same way `GET /registry/{kind}` already was; `ExternalIDs` (top-level)
  and `Binding` are deliberately still full-fidelity (T08's own explicit
  self-discovery mandate). **If a consumer was reading `Callback`/`HostAddress`/`KindMeta`
  off `whoami`'s embedded profile, that field is now absent, not present-but-empty.**
- **`GET /messages/subscribe`** now requires `?as=` matching `?to=`, matching
  every sibling mailbox-read endpoint. A subscribe call without `?as=` that
  used to work now gets `400 invalid_request`.
- **`POST /sessions/bootstrap`** is now safe under genuine concurrent calls
  for the same `session_id` (previously a narrow race could surface a raw
  `500` instead of the endpoint's own documented idempotent `{created:false}`
  contract). No consumer-visible behavior change for the non-racing case.
- **Daemon double-start**: `mux daemon run` against an already-live state root
  now fails fast, before touching the database, instead of potentially
  mutating a live daemon's session state. Relevant if any consumer's own
  process supervisor (or agent-setup's launch scripts) ever raced a restart
  against a still-running daemon.
- **`Consume` could permanently strand a delivery** (CW-20260907-0033,
  previously backlog, now fixed): if the daemon process crashed/failed
  between committing `messages.consumed_at` and recording the corresponding
  delivery-core receipts, the receipt recording could never be retried (the
  message was already "consumed" from the caller's perspective, so every
  future call took the idempotent no-op path) — the delivery stayed
  non-terminal forever, perpetually re-attempted by delivery-core's own
  retry/wake logic even though the recipient had already consumed the
  message. Fixed: receipt recording is now attempted on every `Consume`
  call that has a `delivery_id`, not only the transitioning one, making a
  caller's own retry-after-failure self-healing. See
  `TestDeliveryBackedConsume_RecoversReceiptsAfterCrashBeforeAck`
  (`internal/store/delivery_store_test.go`) for the regression test, which
  fails against the pre-fix code. **This was not the whole story** — two
  further, more subtle instances of the same underlying issue were found
  and fixed in follow-up review passes, described next.
- **`Consume` could also get stuck behind Tether's own still-open wake
  lease** (a second instance of CW-20260907-0033, found by review after
  the fix above landed): `Consume`'s receipt recording always attempted a
  brand-new delivery-core `Claim`, which is correctly refused whenever
  the delivery already has ANY unexpired active lease — including one
  Tether's own wake pump deliberately left open "awaiting consumption."
  Fixed by reusing the delivery's current active lease instead of
  claiming fresh — but a following independent review found that first
  version unsafe: it could reuse (hijack) a lease held by a completely
  independent external process via T07's published-local bridge surface
  (`POST /messages/{id}/claim|ack|nack`), since that endpoint defaults
  its holder to the same asserted recipient URN `Consume` itself uses.
  Fixed properly via migration `0022`: an explicit ownership marker on
  `messages`, written only by `attemptWake` (right after its own claim)
  and by `Consume`'s own fallback-claim path — never by the T07 bridge —
  so `Consume` only ever reuses a lease it can prove is its own. A
  further review found a marker-write failure itself needed to be
  fatal-to-the-wake-attempt (Nack and retry), not best-effort, or the
  exact same stranding could reopen with no crash at all. See
  `TestDeliveryBackedConsume_DoesNotHijackAnIndependentlyClaimedLease`
  and `TestAttemptWake_PendingReceiptMarkerWriteFails_NacksInsteadOfProceeding`.
- **A late `Nack` could reverse an already-completed `Consume`** (a third
  instance found by yet another independent review, reproducing with no
  crash at all): `attemptWake`'s own lease can legitimately be completed
  by a *concurrent* `Consume` call while `attemptWake` is still mid-flight
  (between its `host_accepted` Ack and its busy/offline/send-error check).
  When `attemptWake` then observes a failure and Nacks that same,
  by-then-stale lease, `go-messaging`'s `Nack` incorrectly let the late
  Nack through against the already-`Consumed` attempt, reversing an
  already-`Delivered` delivery back to `retry_scheduled` — causing the
  next wake sweep to redeliver already-consumed content. **This fix
  belongs in, and landed in, the shared `go-messaging` library itself**,
  not Tether: released as `v0.5.1` (tagged and pushed to
  `github.com/hollis-labs/go-messaging`'s `main`), `Nack` now treats an
  already-`StageConsumed` attempt the same as `Failed`/`DeadLettered` — a
  harmless no-op — fixed identically in both the SQLite and in-memory
  backends, with a new permanent shared contract-suite test covering
  both. This repo's `go.mod` is now pinned to that release; see
  `TestAttemptWake_ConcurrentConsumeDuringBusyCheckStaysDelivered`
  (`internal/app/wake_test.go`) for the consumer-side regression test,
  confirmed to fail against the previously-pinned `v0.5.0`.
- **`POST /groups/{urn}/messages` could report unqualified success after a
  total fanout failure**: if the delivery-core `Enqueue` call failed
  entirely (not just the bookkeeping `delivery_message_id` mapping write —
  that narrower gap remains open, CW-20260907-0034), the room post still
  succeeded and the response gave the sender no way to know that **zero**
  group members received a durable delivery obligation for it. Fixed
  (response-transparency, no schema change): the room post still always
  succeeds (never rolled back), but the response now carries a
  `fanout_error` field (HTTP: `sendGroupResponse.fanout_error`; Go client:
  `SendGroupResult.FanoutError`; CLI: a stderr warning line; MCP:
  `tether_group_post`'s result gains a `fanout_error` key) whenever fanout
  was attempted and failed. Empty/absent means success, "not attempted"
  (no other members, or fanout disabled), or simply not known on a later
  read — never a delivery guarantee. See
  `TestGroupFanout_EnqueueFailureIsSurfacedNotSwallowed`
  (`internal/registry/group_fanout_test.go`).
- **`go-tether-client`'s `Get`/`Inbox`/`Thread`/`Subscribe` omitted `?as=`
  entirely**, so every one of them was rejected (`400 invalid_request`) by
  the current daemon — found during this task's own adoption-readiness
  review (§9's "public Go client" gap; see below). Fixed in
  `go-tether-client` commit `a8fb61c` (separate repository, committed to its
  local `main`, **not yet pushed or tagged**): `Inbox`/`Subscribe` now assert
  `?as=<the `to` address they were already called with>`; `Get`/`Thread`
  require a new `WithSelfURN` client option (they have no address parameter
  of their own to derive the claim from) and return the new
  `ErrSelfURNRequired` sentinel client-side if it isn't configured. See that
  repository's own `CHANGELOG.md` `## Unreleased` section for the full
  description.

## 8. Consumer adoption checklists

Each of these is scoped to **exactly** what that consumer's own dependent
task (C01/C02/C03) needs — none require a broad workflow or materializer
redesign on the consumer side, per this task's acceptance criterion.

### 8.1 agent-setup (C01, CW-20260906-0044) — canonical messaging bootstrap, registration, trace propagation

**Working example**: `e2e/consumer_agentsetup_test.go`'s
`TestConsumer_AgentSetup_BootstrapAtLaunchBoundaryIsIdempotent` — bootstraps a
preassigned `SESSION` at the launch/host boundary, proves a retried call is a
no-op, then sends/consumes a real message addressed to the resulting session
URN.

**Required seams**:
- `go-tether-client`'s `ResolveSessionBootstrap` (or the raw `POST
  /sessions/bootstrap` if not using the Go client) at the exact point
  agent-setup currently determines/exports the `SESSION` env var. `intent`
  must be one of `preassigned|resume|compact|fresh|fork` — use `preassigned`
  for agent-setup's normal launch case.
- No registration step is required before bootstrap succeeds — a session
  bootstraps into existence with `state="external"` and is immediately
  addressable at `msg://session/agent-mux/<session_id>`.
- If agent-setup wants trace propagation (linking a launched session back to
  whatever task/run triggered it), thread that through
  `ProviderMappings`/`LogicalAgentID` on the bootstrap call, then use `mux
  messages trace` from the consuming side to confirm delivery.

**Not required**: no group membership, no A2A configuration, no binding
lease call (bootstrap is sufficient for a plain launched session's identity).

### 8.2 Nanite (C02, CW-20260906-0045) — reliable mailbox, durable identity, scoped team publication

**Working example**: `e2e/consumer_nanite_test.go`'s
`TestConsumer_Nanite_DurableActorSurvivesReconnectAndKeepsMessaging` — two
durable actors register once, lease/revoke/re-lease bindings across a
simulated host reconnect, and keep exchanging real messages under the same
stable URNs throughout.

**Required seams**:
- `POST /registry/agents` (once, at actor creation) for a stable URN that
  outlives any one process.
- `POST /registry/bindings` (lease, `capabilities: ["pull-only"]`) at every
  new host-session start; `POST /registry/bindings/{id}/revoke` on clean
  shutdown/handoff (not required for crash recovery — a stale binding is
  simply superseded by the next lease).
- Plain `POST /messages` / `GET /messages/inbox` (or `list` for a
  non-destructive read) for actor-to-actor traffic.
- For "scoped team publication": `internal/registry`'s scoped-bindings
  surface (`mux registry scoped-bindings set|resolve|revisions`,
  `tether_registry_scoped_binding_*` MCP tools) — a role/slot → URN mapping
  with full revision history, if Nanite's "team" concept maps to role
  assignment rather than group membership. If it maps to group membership
  instead, use §8's group surface (§3.3) — pick whichever matches Nanite's
  own team model rather than forcing one onto the other.

**Not required**: A2A configuration (unless Nanite specifically wants
external-protocol reachability for a team member, which is a separate,
optional decision).

### 8.3 Torque (C03, CW-20260904-0103) — canonical session identity, reliable standalone/Tether messaging

**Working example**: `e2e/consumer_torque_test.go`'s
`TestConsumer_Torque_TaskAssignmentRequestResponseLifecycleIsTraceable` — a
`request`-kind assignment message, a runner consuming it, a `status_update`
reply threaded via `in_reply_to`, and both legs confirmed durable via `GET
/messages/{id}/trace`.

**Required seams**:
- Canonical session identity: same bootstrap seam as §8.1 if Torque launches
  sessions directly, or plain `POST /registry/agents` if Torque's "task
  runner" concept maps to a durable registered agent instead of an ephemeral
  session.
- `POST /messages` with `kind: "request"` for task assignment, `kind:
  "status_update"` for progress/completion, `in_reply_to` set to the
  assignment's message ID for correlation.
- `GET /messages/{id}/trace` as Torque's own polling/tracking mechanism to
  confirm durable delivery and consumption — this is precisely the surface
  T09 built for exactly this kind of external tracker.

**Not required**: no group surface needed unless Torque's task model
involves broadcasting the SAME task to every member of a fixed roster at
once (in which case, use §3.3's group fanout — but note it is NOT a "first
runner claims it" mechanism: fanout gives each of the N group members their
OWN independent, guaranteed delivery of the post, not one shared job N
runners compete over). For "assign this task to whichever of a pool of
runners claims it first," model the runners as pulling from one SHARED
recipient address instead (all runners consume the same mailbox) and use
the concurrent-claim primitive (§3.4/§9) over that single mailbox's
messages — a genuinely different addressing shape than group membership.

## 9. Known gaps carried forward (backlog, not blockers)

Filed during T10/T11/this task's own closeout as findings; none block this
handoff. **CW-20260907-0033 was filed as backlog and is no longer open** —
this task's own follow-up closeout fixed it (§7) rather than deferring it, so
it no longer appears below.

| Torque ID | Gap | Why not fixed here |
|---|---|---|
| CW-20260907-0028 | A2A outbound (Tether as an A2A *client*) has no implementation | Explicitly out of scope by design decision during T10 — server-only relay was chosen. |
| CW-20260907-0029 | `/a2a/` shares a listener/mux with every same-host-trust route; no network-boundary separation for genuinely external A2A traffic | Architectural question bigger than one feature; needs its own design pass. |
| CW-20260907-0031 | The repo's own `api-stub` test provider fails to launch through the real daemon HTTP path (`agentlaunch/compile: matrix: unknown provider id`) | Unrelated subsystem (launch-plan compilation), likely bitrot in a v0.0.2-era fixture; found while building an e2e private-local-binding test, not investigated further to avoid disproportionate effort. |
| CW-20260907-0032 | `registry.VisibilityTetherHosted` is a defined enum value with **zero** code path that ever creates it | Needs an actual design decision (what "hosted" means, distinct from "private-local"), not a bug fix. |
| CW-20260907-0034 | Bundled minor items: `GET /registry/bindings?target_urn=` has no ownership check; group-fanout's `delivery_message_id` mapping write isn't atomic with fanout (currently latent, nothing reads that column yet); `e2e/`'s crash tests only kill at safe operation boundaries, not mid-write | All low-severity/should-fix per T11's independent review; none block this handoff. |
| CW-20260907-0038 | `POST /messages/{id}/claim|ack|nack` (durable claim primitives, §3.4) have no typed Go client wrapper in `go-tether-client` — only raw HTTP today | Explicitly deferred during this task's own closeout when fixing that client's `?as=` gap (§7) — the narrower "client-level self-identity" fix was chosen over "full parity," so claim/ack/nack wrappers remain a separate follow-up. |

## 10. Rollout / rollback

**Rollout** (once Chrispian authorizes it): standard `mux daemon start`
against a catalog that has been prepared per §4. No special migration step —
the daemon's own startup path (`store.Migrate`) applies any pending
migrations idempotently on boot, proven safe by `e2e/upgrade_test.go` and the
now-hardened double-start guard (§7) that prevents two daemons from racing
that same startup path against one database.

**Rollback**: every schema change this epic itself introduced (migrations
`0019`-`0021`, §6) is additive — rolling
back to a pre-messaging-vnext Tether binary against a post-migration database
is not supported (older code doesn't know the new tables/columns exist, but
won't be broken by their mere presence, since nothing in this epic altered or
removed an existing column). If a rollback is ever needed, restore a database
snapshot taken before this epic's migrations ran, alongside the older binary
— do not attempt to run new-schema data against old code.

## 11. Cutover checklist

Use this as the literal go/no-go list. Items marked **[Chrispian]** are
explicitly not something any task in this epic is authorized to do
unilaterally.

- [x] Platform implemented, tested, and reviewed (T01–T11, this document).
- [x] `make check` green on the landed baseline.
- [x] Delivery/durability/security adversarial review complete, all
      high-severity findings fixed (T11).
- [ ] **[Chrispian]** Bump Tether onto `go-messaging`'s existing `v0.5.0` tag,
      and cut `go-tether-client`/`tether` release tags, per §2's safe order,
      if/when moving off `main`/pseudo-versions is desired.
- [ ] **[Chrispian]** Authorize C01 (agent-setup), C02 (Nanite), C03 (Torque)
      to begin their own independent adoption work, each against this
      handoff.
- [ ] C01/C02/C03 land in their own repositories, each producing their own
      versioned, reproducible evidence (per T13's acceptance criteria, not
      this task's).
- [ ] **[Chrispian]** Authorize local integration runs once all three land.
- [ ] T13 (CW-20260906-0046) runs, verifying real (not fixture-only) consumer
      wiring across all three, plus restart recovery and cross-consumer
      review resolution.
- [ ] **[Chrispian]** Any live daemon reload, hook activation, or production
      database exposure to this epic's changes — no task in this epic,
      including this one, is authorized to perform this.

**Platform-ready ≠ rollout-complete.** This document and T11's evidence
establish the former. Only T13's own sign-off, after real (not fixture)
consumer wiring is verified, establishes the latter — do not conflate the two
in any status report derived from this document.

## 12. Review record

This document itself was checked for accuracy against the live `main` branch
(commit `858dda7`) rather than written from memory of the plan: every HTTP
route table in §3 was cross-checked against `internal/api`'s actual
`mux.Handle`/`mux.HandleFunc` registrations; every MCP tool name against
`internal/mcpadapter`'s actual `mcp.NewTool` registrations; every CLI command
group against `cmd/mux`'s actual `cobra.Command` definitions and
`rootCmd.AddCommand` wiring; the version/SHA table in §2 against each
repository's actual `git log`/`go.mod`/`CHANGELOG.md` at the time of writing,
not the plan's original assumptions. The known-gaps table in §9 lists the
seven backlog Torque tasks filed during T10/T11's own work plus one
additional untracked gap surfaced by this task's own review pass, with no
paraphrasing of severity beyond what each task's own filing already states.

A second, independent fact-check review pass (distinct from the drafting
pass above) was then run against this document specifically, and found seven
real inaccuracies, each independently re-verified against live code/git
state before correction (not merely trusted from the review's own wording):
a broken `mux registry register` copy-paste example (§4.1, real flags
verified via `--help`); a stale go-messaging release-order claim (§2/§11 —
`v0.5.0` already exists past Tether's pinned commit, verified via
`git fetch --tags` + `git merge-base --is-ancestor` + `git log` — see the
third paragraph below for a correction to exactly which commit the tag
itself resolves to); a stale Federation Router
claim (§6 — confirmed live-wired via T05's `newFederatedMessageStore`/
`federation.BuildRouter`, read directly in `cmd/mux/daemon.go` and
`internal/app/service.go`); a migration-range over-attribution (§2/§6/§10 —
only `0019`-`0021` belong to this epic, confirmed by reading each migration
file's own header comment); a missing acknowledgment that T01 §3.3 assigned
this task the `/broker/*` + `logical_agents.claude_session_id` retirement
timeline (confirmed by reading `T01-compatibility-contract.md` directly, and
that the write path is still live by reading
`internal/store/logical_agents.go`'s `SetClaudeSessionID`) — that timeline is
still **not delivered** by this document, only now explicitly flagged as
such rather than silently re-deferred; an incomplete `delivery.write` scope
note (§3.5 — `mux_message_redrive` requires it too, not just purge); and a
dangling "see §9" cross-reference for the claim/ack/nack client-wrapper gap
(§3.4 — now a real row in §9's table). All seven are corrected in this
version of the document.

A third pass — this time Chrispian's own direct review of the platform,
not a delegated fact-check — found three further substantive gaps and two
further document inaccuracies, all independently re-verified against live
code/git state and then acted on (fixed, with tests) rather than only
documented:

1. `Consume` could permanently strand a delivery on a narrow crash window
   (CW-20260907-0033, previously filed as backlog needing design care) —
   confirmed by tracing `consumeAndReportDeliveryID`/`recordConsumedReceipts`
   in `internal/store`, then **fixed**: receipt recording now retries on
   every idempotent replay, not only the transitioning call. Regression
   test: `TestDeliveryBackedConsume_RecoversReceiptsAfterCrashBeforeAck`
   (fails against the pre-fix code).
2. `go-tether-client`'s `Get`/`Inbox`/`Thread`/`Subscribe` genuinely omitted
   `?as=` and would be rejected by the current daemon — confirmed by
   reading the client's source directly, then **fixed** in that repository
   (commit `a8fb61c`, not yet pushed), with claim/ack/nack wrappers
   explicitly scoped out and filed as CW-20260907-0038. Regression tests:
   `TestHTTPStore_GetAndThread_RequireSelfURN`,
   `TestHTTPStore_SendsAsQueryParam`.
3. Group fanout could report unqualified send success after a total
   delivery-core Enqueue failure, leaving zero recipients with a delivery
   obligation and no signal to the sender — confirmed by reading
   `group_fanout.go`/`service.go` directly, then **fixed**
   (response-transparency: a new `fanout_error` field surfaced through the
   HTTP response, the Go client, the CLI, and the MCP tool — no schema
   change, per the chosen fix scope). Regression test:
   `TestGroupFanout_EnqueueFailureIsSurfacedNotSwallowed`.

A fourth pass (an independent review of item 1's fix above, dispatched by
the executor before considering it closed) found item 1 was incomplete:
`Consume` could still get stuck behind Tether's own still-open wake lease
(commit `64a981f`), fixed by reusing the delivery's current active lease —
but a further independent review of THAT fix found reusing any active
lease indiscriminately could hijack an independent external claimant's
lease via T07's published-local bridge surface, fixed via a new ownership
marker (migration `0022`) that only `Consume`/`attemptWake` ever write —
and a further review of THAT found a marker-write failure needed to be
treated as fatal to the wake attempt (Nack and retry), not best-effort, or
the same stranding could reopen with no crash at all. See §7 for the full
narrative and regression tests; each of the three iterations here was
independently confirmed to fail against the version it replaced before
being accepted.

A fifth pass (Chrispian's own further direct review) found one more real
gap in the now-hijack-safe `Consume` fix: a late `Nack` racing a
concurrent, legitimate `Consume` could still reverse an already-completed
delivery back to `retry_scheduled`, reproducing with no crash at all. This
one was a bug in the shared `go-messaging` library itself, not Tether —
fixed there (both SQLite and in-memory backends, plus a new shared
contract-suite test), released as `go-messaging v0.5.1` (tagged, pushed to
`origin/main` on GitHub), and this repo's `go.mod` bumped to that release
(commit `75f80e8`) alongside a consumer-side regression test
(`TestAttemptWake_ConcurrentConsumeDuringBusyCheckStaysDelivered`),
confirmed to fail against the previously-pinned `v0.5.0` and pass again
once restored to `v0.5.1`.
4. The go-messaging `v0.5.0` tag was mischaracterized as sitting at commit
   `70770fb` with a further untagged commit on top — corrected after
   re-running `git rev-parse v0.5.0^{commit}` directly: the tag is an
   annotated tag that dereferences to `9789d8f`, exactly `origin/main`'s
   tip, so no further commit sits un-tagged past it (§2).
5. §8.3's suggestion that group fanout suits "whichever of N runners claims
   it first" was a category error — fanout gives each member their OWN
   guaranteed delivery, not one shared job N runners compete over —
   corrected to point a first-claim use case at the concurrent-claim
   primitive over a shared mailbox instead (§8.3).

All five of the third pass's findings are reflected in this version of the
document (§2, §6, §7, §8.3, §9), and the fourth and fifth passes' fixes are
reflected too (§7, §2, §12 above). Every code fix across all three passes
carries its own regression test, independently confirmed to fail against
the code/release it replaced. `make check` passes clean in the tether repo
as of the final commit in this chain (`75f80e8`): 1673 tests (race-enabled),
0 lint issues, 0 govulncheck vulnerabilities, 66.9% coverage. `go-tether-client`'s
own fix passes `go build`/`go vet`/`go test ./...` clean in that repository
(still local-only, not pushed). `go-messaging`'s fix passes that
repository's own `make check` (fmt/vet/lint/test-race/vuln) and is live on
GitHub as the pushed, tagged `v0.5.1` release.
