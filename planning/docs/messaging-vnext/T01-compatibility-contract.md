# T01 — Landed messaging baseline and Tether compatibility/cutover contract

Torque: CW-20260906-0033 (plan CW-20260906-0023, "Messaging vNext"). Architecture
approved by Chrispian 2026-09-06, session session-20260906-8a2916eb. Gate
CW-20260906-0032 (G09) and CW-20260906-0031 (G08) are both `done` in Torque as
of this writing.

This document is T01's deliverable: (1) the baseline/version matrix, (2) the
current-path inventory, (3) the migration/compatibility contract, (4) the
pre-migration test/build evidence. It does not implement anything beyond the
two mechanical fixes noted in §4 — implementation of the contract itself is
T02–T12.

## 1. Baseline/version matrix

Root `go.mod` (committed, unchanged by this task) already pins the *published*
versions from the unrelated dependency-sweep task CW-20260905-0054 (PR #43,
merged 2026-09-05, commit `5406114` on `main`):

| Module | Tether's committed `go.mod` version | Note |
| --- | --- | --- |
| `github.com/hollis-labs/agentkit` | v0.5.1 | Published; predates this vNext stream's G02 work. |
| `github.com/hollis-labs/go-messaging` | v0.4.0 | Published; predates this vNext stream's G01/G03-G06 work. |
| `github.com/hollis-labs/go-providers` | v0.25.0 | Published, unchanged by vNext. |
| `github.com/hollis-labs/go-runner` | v0.6.0 | Published, unchanged by vNext. |
| `github.com/hollis-labs/go-otel` | v0.6.1 | Published, unrelated to messaging. |
| `github.com/hollis-labs/go-sandbox` | v0.2.1 | Published, unchanged by vNext. |

**The vNext work is not published.** It exists as unreleased commits on top of
those tags in the local `libs/` checkouts, landed there per the recorded
handoff at
`/Users/chrispian/dev/agent-os/workspaces/drafts/shared-materialization-20260906/messaging-vnext-local-handoff.md`.
Verified directly against the actual checkouts (all clean, matching the
handoff exactly):

| Module | Checkout | Branch | HEAD SHA |
| --- | --- | --- | --- |
| go-messaging | `/Users/chrispian/dev/hollis-labs/libs/go-messaging` | `codex/messaging-vnext-g01-session-20260906-a538428d` | `2a0132bb3e2cace893fb321c1ba3835eeeafe822` |
| agentkit | `/Users/chrispian/dev/hollis-labs/libs/agentkit` | `feat/parity-stale-provenance` | `afade3a9b2e47f24519bd48d33b3e75aae671c61` |
| go-agent-wrapper | `/Users/chrispian/dev/hollis-labs/libs/go-agent-wrapper` | `main` | `215d0e7e32bb0eb0901b435e09719765bff9fe91` |
| go-providers | `/Users/chrispian/dev/hollis-labs/libs/go-providers` | `main` | `da49e247f307ff2b6d9a84d6221b650e56f99965` |
| go-sandbox | `/Users/chrispian/dev/hollis-labs/libs/go-sandbox` | `main` | `19b76c2a66f116eb1b83c39c06354607d7c50766` |
| go-runner | `/Users/chrispian/dev/hollis-labs/libs/go-runner` | `main` | `1e90842b43c56b174a7765c7b53e6cc8d9f39b8c` |

Reference (not required by Tether directly today — see §2.14):

| Module | Checkout | HEAD SHA |
| --- | --- | --- |
| go-tether-client | `/Users/chrispian/dev/hollis-labs/libs/go-tether-client` | `67d06cc07c732d4ac2586a4b98362d99f92e7e25` (still `v0.1.0`, pinning `go-messaging v0.2.0`, `go 1.22` — see §2.14, §4.3; T08 scope) |

**Consumption rule for T01-T12:** development against the vNext content uses
an uncommitted, test-only `go.work` (not checked into either repo):

```
/private/tmp/claude-501/-Users-chrispian-dev-hollis-labs-apps-tether/401261e3-e568-46ad-b7aa-26532b94739b/scratchpad/tether-messaging-vnext.go.work
```

```go
go 1.26.7

use (
	/Users/chrispian/dev/hollis-labs/apps/tether
	/Users/chrispian/dev/hollis-labs/apps/tether/apps/sysop
	/Users/chrispian/dev/hollis-labs/libs/go-tether-client
	/Users/chrispian/dev/hollis-labs/libs/go-providers
	/Users/chrispian/dev/hollis-labs/libs/go-sandbox
	/Users/chrispian/dev/hollis-labs/libs/go-runner
	/Users/chrispian/dev/hollis-labs/libs/agentkit
	/Users/chrispian/dev/hollis-labs/libs/go-agent-wrapper
	/Users/chrispian/dev/hollis-labs/libs/go-messaging
)
```

`go.mod` `require` lines are **not** bumped to a fictional new version number
for the unreleased content — there isn't one yet. Root `go.mod` keeps naming
v0.4.0/v0.5.1 (the real, already-published versions) until go-messaging/agentkit
actually cut a release containing this work; until then, `GOWORK` is how this
stream's own dev/test loop sees the new code, matching TETHER-PLAN's "use
temporary test-only Go workspace wiring... never commit... machine-specific
go.work files."

Verified builds under that workspace (2026-09-06): `go build ./...` clean for
Tether root, `apps/sysop`, and `go-tether-client`. `go-tether-client`'s test
suite has one **pre-existing, workspace-independent** flake — see §4.3.

## 2. Current-path inventory

Verified by a dedicated read-only pass over every messaging/identity-touching
package (2026-09-06). All file:line references below are against the SHAs in
§1 (Tether `main` @ `5406114`).

### 2.1 Flat end-to-end call-path inventory (acceptance #1's core deliverable)

Eleven traced chains, each confirmed by reading the actual code, not inferred:

1. **CLI send** — `mux messages send` (`cmd/mux/messages.go:54,77`) → `internal/client.Client.MessageSend` (`client.go:933`, `POST /messages`) → `internal/api/messages.go:303 handleMessageSend` → `internal/store/messaging_store.go:59 (*messagingStore).Send` → `INSERT INTO messages` + in-memory `fanOut`.
2. **CLI inbox (destructive pull)** — `messages.go:185,194` → `client.go:869 MessageInbox` (`GET /messages/inbox`) → `internal/api/messages.go:350` → `messaging_store.go:132 Inbox` — tx-scoped `SELECT ... WHERE delivered_at IS NULL` + `UPDATE ... SET delivered_at=?`.
3. **CLI list (non-destructive)** — `messages.go:206,215` → `client.go:886 MessageList` → `internal/api/messages.go:397` → `internal/store/messaging_inbox.go:100 List` (no lifecycle mutation).
4. **CLI consume/ack** — `messages.go:251,263` → `client.go:986` → `internal/api/messages.go:524` → `messaging_store.go:256 Consume` — `UPDATE ... SET consumed_at=? WHERE ... consumed_at IS NULL`.
5. **MCP send** — `mux_message_send` → `internal/mcpadapter/messages.go:115` → **directly** `a.svc.Store.MessagingStore().Send` — same tail as #1, but see §2.7: this runs inside a separate `mux mcp` OS subprocess with its own SQLite connection to the same file, not through the daemon.
6. **MCP inbox/list** — `internal/mcpadapter/messages.go:226,309` → directly `.Inbox`/`.List` — same subprocess bypass as #5.
7. **HTTP send** — `POST /messages` → `internal/api/messages.go:303` → `messaging_store.go:59 Send` — identical tail to #1/#5, but this is the actual daemon-owned instance with live in-memory fan-out.
8. **HTTP list/read** — `GET /messages/list` → `internal/api/messages.go:397` → `messaging_inbox.go:100`; `GET /messages/{id}` → `internal/api/messages.go:336` → `messaging_store.go:109 Get`.
9. **Group post** — CLI `mux group post` (`cmd/mux/group.go:300,333`) → `internal/client/groups_client.go:296` (`POST /groups/{urn}/messages`) → `internal/api/groups.go:444,460` → `internal/registry/service.go:637 SendToGroup` → `internal/registry/storage.go:1468 InsertGroupMessage` → **raw `INSERT INTO messages (...) ` at storage.go:1498-1507 — bypasses `internal/store/messaging_store.go` entirely** (see §2.5); mentions fire `internal/store`'s `Send` separately for notice envelopes only. MCP parallel: `tether_group_post` → `group_tools.go:485,516` → same `Service.SendToGroup`.
10. **Group read** — CLI `mux group read` (`group.go:366,375`) → `groups_client.go:346` (`GET /groups/{urn}/messages`) → `internal/api/groups.go:483,504` → `internal/registry/service.go:694 ListGroupMessages` → `internal/registry/storage.go:1529` → **raw `SELECT ... FROM messages WHERE group_urn=?`** — again bypasses `messaging_store.go`'s `Get`/`List`/`Thread`. MCP parallel: `tether_group_read` → `group_tools.go:527,540`.
11. **Federation inbound delivery** — no federation-specific inbound path exists: a peer's `httpPeerStore.Send` (`internal/federation/peerstore.go:97-123`) is a plain `POST` against this daemon's ordinary `/messages` endpoint — identical tail to #7. Outbound routing through `federation.Router` is fully implemented and tested but **never invoked** by the running daemon (§2.6).

### 2.2 Registry URNs and messaging URNs are the same identifier space

ADR-0041 D3: registry identities mint as `msg://agent/agent-mux/<id>` (or
`msg://project/...`) — the **same URN scheme and scheme prefix** ADR-0023
defines for message addressing, not a separate ID space needing a lookup
table. A registered agent's registry URN *is* its messaging address. This
simplifies §3.1: "registry URN maps onto `ActorRef`" is not a translation
layer, it's the same string. What's still genuinely separate is
`logical_agents.id` (Tether's internal durable-actor row key, pre-dates the
registry and is never itself a `msg://` URN) and canonical `SESSION`
(agentkit, per-session, newer still) — three name-spaces, two of which
(registry, messaging) already coincide.

### 2.3 Known schema facts (verified directly against migrations)

Tether's messaging schema already carries real migration history, not a
single monolithic table:

| Migration | Adds | Status |
| --- | --- | --- |
| `0005_broker_envelopes.sql` | `broker_envelopes` (v0.0.2 mailbox table: id, sender, recipient, workflow_id, correlation_id, message_type, priority, payload, created_at, delivered_at, consumed_at, audit_json) | Live-write, dead-lifecycle — see §2.10. `broker_envelopes` still receives inserts and has a live HTTP surface, but `delivered_at`/`consumed_at` are never written by any code path. |
| `0010_messages.sql` | `messages` (id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type, metadata, created_at, delivered_at, consumed_at, canceled_at) | Live — the `go-messaging`-native table aligned 1:1 with `messaging.Envelope`, used by `/messages/*` routes **and**, via a separate raw-SQL path, by group messages (§2.5). |
| `0013_messages_inbox_state.sql` | `messages.read_at`, `messages.archived_at` | Live — confirmed non-destructive attention state, fully independent of `delivered_at`/`consumed_at` in the actual query logic (§2.4.2). |
| `0014_messages_read_index.sql` | index only | Live. |
| `0016_group_messaging.sql` | `registry_entries.kind` extended to include `'group'`; `group_members` (grp_urn, member_urn, role, joined_at, last_read_seq); `messages.group_urn`, `messages.group_seq` | Live — Sprint v060-05 "Group Messaging" (ADR-0042) is a complete, shipped implementation, not schema-only (§2.5). |
| `0015_registry.sql`, `0017_registry_external_ids.sql` | `registry_entries`/`registry_capabilities`/`registry_skills`/`registry_links`/`registry_external_ids` | Live — the *separate* v0.6 federation-directory epic (agent/project/group identity), unrelated in origin to this messaging-vNext stream but the identity substrate group rooms now sit on. |
| `0003_logical_agents.sql`, `0007_logical_agent_claude_session_id.sql`, `0009_session_groups.sql` | `logical_agents` (durable actor row), `sessions.logical_agent_id` FK, `logical_agents.claude_session_id`, `session_groups` | Live — Tether's pre-existing actor/session model, predates agentkit's canonical `SESSION` vocabulary. Confirmed fully disjoint from it today — §2.12/§3.1. |

`internal/registry/` (Go package) is the **new v0.6 federation directory
service** — CLAUDE.md: "the pre-v060-01 `internal/registry/` package was
renamed [to] `internal/launchresolve/`... to free the `registry` name." It is
not the old launch-catalog resolver. This matters because "registry URNs" in
the messaging-vNext architecture doc and "registry URNs" in the v0.6 epic are
the same identity substrate, not two systems.

### 2.4 `internal/store` messaging persistence: column semantics confirmed

Verified by reading every write/read site (`internal/store/messaging_store.go`,
`messaging_inbox.go`, `messaging_list.go`) against the `messages` schema:

| Column | Written by | Read/filtered by |
| --- | --- | --- |
| `id`,`kind`,`channel`,`from_urn`,`to_urn`,`thread_id`,`in_reply_to`,`payload`,`content_type`,`metadata`,`created_at` | `Send` (`messaging_store.go:59-98`) | `Get`, `Inbox`, `Thread`, `List` |
| `delivered_at` | **only** `Inbox` (`:194-201`, atomic SELECT+UPDATE in one tx) | `Inbox`'s own `WHERE delivered_at IS NULL` (`:140`) |
| `consumed_at` | **only** `Consume` (`:258-260`) — does **not** require `delivered_at` to be set first | `Consume`'s own idempotency check |
| `canceled_at` | `Cancel` (`:288-289`) | `Inbox`'s WHERE excludes canceled rows |
| `read_at` | `MarkRead` → `stampRecipientField` (`messaging_inbox.go:170-201`) | `List`'s `UnreadOnly` filter |
| `archived_at` | `Archive`/`Unarchive` → `stampRecipientField` (`:175-213`) | `List`'s default `archived_at IS NULL` exclusion |

**2.4.1 — Delivered vs. consumed are genuinely distinct, but there is a crash-recovery gap.** They're independent columns set by independent methods; `Consume` never checks `delivered_at`. But there is **no automatic redelivery**: if a consumer crashes after `Inbox()` sets `delivered_at` but before calling `Consume`, that message is permanently excluded from future `Inbox()` calls (`WHERE delivered_at IS NULL`) with no retry/expiry mechanism today. This is exactly the gap the new `delivery` package's lease+attempt model (§1, go-messaging vNext) closes — T03's primary functional win, not just a storage-engine swap.

**2.4.2 — Read/archived attention state is cleanly independent of delivery state, confirmed with no conflation anywhere.** `stampRecipientField` only ever touches `read_at`/`archived_at`; `Inbox`/`Consume`/`Cancel` never touch them; `List`'s WHERE clause never references `delivered_at`/`consumed_at`. T03 can layer new delivery-receipt columns alongside these without behavior change to List/MarkRead/Archive.

**2.4.3 — Race coverage already exists.** `messaging_race_test.go` covers concurrent `Inbox` (exactly-once delivery via tx-scoped SELECT+UPDATE), concurrent `Consume` (idempotent), cancel-vs-consume, send-vs-subscribe, and dispatcher request/reply concurrency — all with `-race`. `messaging_store_test.go` already runs go-messaging's own `messagingtest.RunContract` against this SQLite store — **it is already a fully contract-conformant `messaging.Store` today**, which materially lowers T03's risk: the swap is additive (adopt `delivery.Store` alongside/under the existing conformant `Store`), not a from-scratch reimplementation.

### 2.5 Group messaging is fully built, but writes through a second, undocumented-until-now path into the shared `messages` table

This is a material correction to how T04 should be scoped. ADR-0042 (Sprint
v060-05, Accepted 2026-05-21) documents a **complete, shipped** implementation,
not a stub:

- `msg://group/<authority>/<grp_id>` addressing (3-segment, server-minted
  `grp_`-prefixed IDs). `internal/registry/id.go:84-87` and
  `internal/federation/README.md:41-44` both still say group URNs must route
  around `go-messaging.ParseURN` because "`go-messaging v0.2.1`'s closed
  `AddressKind` enum doesn't include `group`." **That premise is stale: the
  currently-pinned `go-messaging v0.4.0` already ships `KindGroup` (added at
  v0.3.0) and already accepts group-kind addresses in `ParseURN`.** Nothing
  in `internal/registry` has been updated to use it — see the structural
  finding below.
- Mailbox-pull delivery (one row in `messages` + `group_urn`/`group_seq`,
  not fan-out-per-member) with non-destructive per-member `last_read_seq`
  cursors on `group_members`.
- Owner/moderator/member roles, open-post, moderator-gated invite/kick/
  archive, archive-not-delete (`ErrGroupArchived` HTTP 423), an
  owner-must-transfer-before-leaving constraint.
- A two-phase `@`-mention parser (`internal/messaging/mentions.go`,
  pre-commit `Parse`/post-commit `Dispatch`) emitting pointer-only `notice`
  envelopes to the mentioned agent's personal inbox — capped at 32
  mentions/message, ambiguous short-form mentions abort the send (HTTP 400)
  pre-commit. Sent from a synthetic system URN `msg://agent/agent-mux/agt_mxsysmnt00`
  (deliberately faked as `agent`-kind so `go-messaging.ParseURN` accepts it,
  chosen precisely because `group`-kind sender addresses weren't safe to use
  at write time — another artifact of the stale-version assumption above).
- `internal/registry.Service.SendToGroup`/`ListGroupMessages`/membership
  methods are fully implemented and tested (`service_test.go`, `storage_test.go`),
  wired to both `internal/api/groups.go` and `internal/mcpadapter/group_tools.go` —
  **real, working code, confirmed not schema-only.**

**Critical structural finding: group messages bypass `internal/store`'s
`messaging.Store` entirely, via a second raw-SQL write/read path.**
`internal/registry/storage.go:1468 InsertGroupMessage` does its own
`INSERT INTO messages (id, kind, from_urn, to_urn, thread_id, payload,
content_type, created_at, group_urn, group_seq) VALUES (...)` (`:1498-1507`)
directly against the *same* `messages` table `messaging_store.go`'s `Send`
writes to (`:82-98`) — with its own UUIDv7 generation and its own
`group_seq` counter, **never calling `messagingStore.Send`.** Symmetrically,
`ListGroupMessages` (`storage.go:1529`) does its own raw
`SELECT ... FROM messages WHERE group_urn=? AND group_seq>?`, never calling
`Get`/`List`/`Thread`. Consequences, all confirmed by reading the code:
(a) group posts get **no in-memory fan-out** — `messaging_store.go`'s
`Subscribe`/SSE stream never sees them; (b) `to_urn` on a group row is the
group URN itself, which (at the *time this ADR-0042 code was written*)
`go-messaging.ParseURN` would reject, so `/messages/list`, `/messages/inbox`,
`GET /messages/{id}` are all unable to see group rows even by accident —
group messages are readable *only* through `registry.Storage.ListGroupMessages`.
This is exactly the "do not shadow-write the same logical message into two
independent authorities" anti-pattern go-messaging's own compatibility guide
warns against (§1) — except here it's two *tables paths onto the same table*,
not two tables. **Now that `go-messaging v0.4.0` already has `KindGroup`
(above), this dual-path is no longer structurally required** — reconciling
`internal/registry`'s group read/write path onto `messaging_store.go`'s
`Store` (or the new `delivery.Store`) directly is live, actionable T03/T04
work, not blocked on any future library change.

**Implication for T04:** "Preserve group rooms" is a *verification and
reconciliation* task (confirm the above still behaves correctly once posts
go through the shared delivery core instead of a parallel raw-SQL path — this
closes the fan-out/notify gap as a side effect), not new room construction.
What T04 actually adds net-new is architecture's **scoped-address role/slot
binding** primitive ("`reviewer`/`engineer` within a team/run", resolution
provenance, single-vs-fanout, frozen recipient sets on accepted delivery) —
a distinct concept from ADR-0042's owner/moderator/member *group-permission*
roles, and from `registry_links`, which ADR-0042 explicitly ruled out for
group membership because it can't carry per-member state. The "durable
fanout" half of T04's title is the other real new-work item: today's group
delivery has no per-recipient delivery-obligation/receipt tracking at all
(ADR-0042: "No event emission on group writes... Consumer cache invalidation
happens via re-pull") — T04 is where per-recipient receipts (architecture:
"attach lightweight per-recipient delivery receipts" to "one canonical body")
get added without duplicating the room body or turning it into a destructive
queue.

### 2.6 Federation Router is fully implemented, fully tested, and never actually invoked at runtime

ADR-0040 (Accepted) ships `internal/federation.Router` (authority-routing
`messaging.Store` decorator) and its own text already flagged the risk:
*"the Router is composed onto `Service.Federation` but the MCP/HTTP message
front-ends still call `Store.MessagingStore()` directly."* **Confirmed still
true today, precisely.** `internal/app/service.go:58,176` constructs
`Service.Federation *federation.Router`, but a repo-wide grep for
`.Federation` (excluding tests) finds only its two construction sites — no
other code ever reads it. `internal/daemon.Server` has no `Federation` field
at all. `cmd/mux/daemon.go:205` wires `MessageStore: svc.Store.MessagingStore()`
— the bare local store — into every HTTP handler; the mention parser is
wired the same bare way (`daemon.go:196-198`). **Net effect: even with
`federation.enabled: true` configured, no Send/Inbox/Consume call in the
running daemon goes through the Router today.** It is fully implemented and
covered by `router_test.go`/`peerstore_test.go`/`federation_test.go`, but
dead weight at runtime. This is exactly TETHER-PLAN's own words for T05:
"connect actual federation hooks rather than unused router construction" —
now an evidenced fact, not a hypothesis to rediscover. "Federation inbound
delivery" today is nothing more than an ordinary unauthenticated `POST
/messages` from a peer's `httpPeerStore.Send` landing on the same
`handleMessageSend` as any local client (§2.1 item 11) — there is no
federation-aware inbound handling of any kind.

ADR-0040 also flags that `internal/federation.Router` "intentionally
duplicates the decorator promoted into `go-messaging`" and calls swapping to
the library version "a drop-in once `go-messaging` is retagged." **Confirmed:
go-messaging v0.4.0 (Tether's current pin) already ships that
`Router`/`Register`/`Unregister`/`Authorities`/`IsLocal`/`WithStrictRouting`
API (added v0.3.0) — `internal/federation.Router` is today a literal,
already-swappable duplicate of code already in Tether's dependency tree.**
Retiring `internal/federation.Router` in favor of `messaging.Router` and
actually wiring `Service.Federation` into the HTTP/MCP front-ends are both
now unblocked, evidenced T05 line items rather than open questions.

### 2.7 MCP tools run in a separate OS process with their own SQLite connection — the real "unify transports" problem

This is the single most important finding for T05 and was not visible from
schema/ADR review alone. `mux mcp` (`cmd/mux/mcp.go:97`) constructs its
**own** `app.New(...)` — its own OS process, its own `*store.Store`/SQLite
connection to the same `~/.tether/state/tether.db` file, entirely separate
from the running daemon's in-process store instance. All ten message tools
except `mux_message_notify` (`internal/mcpadapter/messages.go:29-111`) call
`a.svc.Store.MessagingStore()` **directly** — a direct SQLite access from a
different process, not merely an in-process shortcut around HTTP. Consequence:
because `Subscribe`/fan-out is in-process/in-memory (`messaging_store.go:309-352`),
**an MCP-tool-originated `Send` never triggers the daemon's SSE
`/messages/subscribe` fan-out** — durably persisted, but invisible in
real time to anything subscribed to the daemon. `mux_message_notify` is the
sole exception, routed through `a.client.MessageNotify` to the daemon because
it needs the daemon's live `SendTurn` capability. Today there are
effectively **three** independent write surfaces onto the same SQLite file
for ordinary messages (daemon HTTP, the `mux mcp` subprocess, and — per §2.5
— `internal/registry`'s raw-SQL group path); CLI is the one surface
confirmed clean (always via the HTTP/UDS client, §2.8). T05's "route
canonical sends... through one daemon-owned service" acceptance criterion is
this exact problem, now precisely located rather than assumed.

### 2.8 CLI surface — confirmed no messaging bypass

`cmd/mux/messages.go`, `group.go`, and `registry.go` all route exclusively
through `internal/client`'s HTTP/UDS client — confirmed by their own header
comments ("no filesystem reads bypass the daemon", "no in-process daemon
dependency") and by reading every subcommand. The one direct-store bypass in
the CLI is `cmd/mux/sessions.go`'s deliberate read-only degraded-mode
fallback (`listSessionsDaemonOrStore`/`openStoreReadOnly`,
`cmd/mux/daemon.go:821`) for `mux sessions list`/`get` when the daemon is
unreachable — unrelated to messages/broker/groups/registry, none of which
have an analogous fallback.

### 2.9 No authentication on either message surface today

`internal/api/messages.go`: `from`/`to` on send are body-supplied with no
verification (`:118-128`); the recipient identity for read/archive/consume/
mark-read is a caller-supplied `?as=<urn>` query parameter
(`recipientAction`, `:452-477`) with **zero verification the caller actually
is that URN** — same-host-UDS-trust by explicit design (matches ADR
group-messaging's documented D7 same-host-trust decision, but the message
surface's own ADR-0023 doesn't call this out as explicitly). `internal/api/broker.go`:
same shape, sender/recipient read straight from the request body
(`:172-183`), no auth at all. This is exactly what T05's acceptance criteria
mean by "bind claimed sender/recipient to authenticated... principals" and
"prevent body/query URNs from granting authority" — today, nothing prevents
it.

### 2.10 `internal/broker` / `broker_envelopes` — live writes, dead lifecycle tracking

Confirmed: `broker_envelopes` still receives inserts via
`internal/store/broker_envelopes.go`'s `CreateEnvelope`, and still has a live,
unauthenticated HTTP surface (`internal/api/broker.go:87-93`: `/broker/envelopes`,
`/broker/envelopes/{id}`, `/broker/envelopes/{id}/reply`, `/broker/requests`),
wired end-to-end (`internal/app/service.go:52,175`, `cmd/mux/daemon.go:200`).
It serves the **multiplexor** inter-session-mailbox use case (ADR-0018;
`examples/demos/multiplexor/`), a narrower job than ADR-0023's general
`/messages/*` routing — not simply dead legacy to delete. But **no Go code
anywhere ever executes `UPDATE broker_envelopes SET delivered_at=…` or
`consumed_at=…`** — confirmed by grepping every reference to the table.
`ListEnvelopesByRecipient`'s `WHERE delivered_at IS NULL` filter (intended
as "undelivered") is therefore vestigial: every envelope ever sent to a
recipient is returned forever, since nothing ever marks one delivered. The
in-memory request/reply correlation (`broker.Dispatcher`,
`dispatcher.go:16-59`) is unrelated to these DB columns and is lost on daemon
restart. Net: the broker has a real, live write+read+HTTP surface, but zero
enforced delivery-lifecycle guarantee — closer to an unbounded, never-expiring,
unauthenticated log than a mailbox. `migrations/0010_messages.sql:6-9`'s own
comment — "`broker_envelopes` and `/broker/*` routes remain for backwards
compat until a future migration consolidates them" — confirms this
consolidation was always intended and simply hasn't happened; T03/T05 is
where it happens.

### 2.11 go-messaging's optional `mailbox` subpackage uses tuple addressing, not URNs — adopting it is a re-addressing decision, not a drop-in

go-messaging v0.4.0 (Tether's current pin) already ships an optional
`mailbox` subpackage: a durable service addressed by `(session_id, agent_id)`
**tuples**, not `msg://` URNs, with its own unread/read/resolved lifecycle,
handoff coordination, and notification/wake hooks against a host-owned
`agent_messages` table. This maps closely in *intent* onto Tether's
hand-rolled `read_at`/`archived_at` state and session-wake logic
(`internal/api/messages.go:210-216`) — but because its addressing model is
tuple-based and Tether's `messages` table is URN-based throughout, adopting
`mailbox` is a genuine re-addressing decision for T03, not a drop-in swap.
The `delivery` package (URN/`Address`-based, §1) is the closer structural
fit for Tether's existing schema; `mailbox`'s compat helpers (`LegacyTupleAddress`
etc., §1) exist precisely to bridge tuple-shaped legacy consumers like Nanite
into the URN world, which is a different problem than Tether's own migration.

### 2.12 Session identity: agentkit's canonical `SESSION` vocabulary is entirely unadopted, confirmed by direct grep

Repo-wide grep results (excluding tests), settling §3.1 definitively:
`ProviderSessionMapping` — zero hits. Literal canonical `SESSION` env-key —
zero hits. `ParentSessionID`, `LegacySessionIDs`, `SessionBootstrap` — zero
hits. `internal/registry/*.go` (excluding tests) — zero hits for
`SessionID|ProviderSession|LogicalAgent|ClaudeSession` beyond one unrelated
naming-convention comment; **registry and Tether's session/actor tables are
fully disjoint identity systems today, with zero code-level coupling in
either direction.** Tether does import `agentkit/agentlaunch` and
`agentkit/agentsessions` extensively, but only for `Manager`/`Runtime`/`Plan`
— none of the newer `SessionBootstrap` canonical-session types. `LogicalAgentID`
itself is threaded through 93 non-test files (`internal/store/sqlite.go:85`
`SessionRow.LogicalAgentID`; `internal/store/logical_agents.go:11`
`LogicalAgentRow`; `internal/launch/plan.go:11`; `internal/agent/policy.go`;
`apps/sysop`'s dashboard DTOs; and, the one point where `LogicalAgentID`
already intersects messaging, `internal/api/messages.go:201`'s
`resolveNotifySession`, which matches a running session's `LogicalAgentID`
against a notify recipient's URN ID segment). **Bottom line: a migration
touching messaging identity designs the URN↔session mapping from scratch —
nothing in the current codebase anticipates it in either direction.**

### 2.13 Documentation vs. code mismatches

- `docs/api/README.md`'s `/messages` table lists 8 routes; the live code
  registers 13 (`internal/api/messages.go:29-41`) — missing from the doc:
  `POST /messages/request`, `GET /messages/subscribe`,
  `GET /messages/thread/{id}`, `POST /messages/{id}/cancel`,
  `POST /messages/{id}/unarchive`. T12's documentation deliverable should
  correct this rather than inherit the gap.
- The "Known v1 limitations" note in `docs/api/README.md:1254-1256`, and the
  code comments in `internal/registry/id.go`/`storage.go` and
  `internal/federation/README.md`, all state Tether is pinned to
  `go-messaging v0.2.1` and describe the group-URN/`ParseURN` and
  Router-duplication workarounds as therefore required. **Both are stale**:
  `go.mod` already pins v0.4.0, which already has `KindGroup` and `Router`
  (§2.5, §2.6). These aren't just doc typos — the *code* still behaves as if
  the old constraint holds, which is the real T03/T04/T05 work, not merely a
  docs fix.
- `docs/groups/symbols.md` and `docs/registry/overview.md` — checked against
  code (mention-symbol vocabulary, `callback.go`'s `file://`/`cli://`
  resolvers) and found consistent; no mismatches there.

### 2.14 go-tether-client dependency currency (T08-relevant, noted here for the baseline matrix)

`libs/go-tether-client/go.mod` requires `go-messaging v0.2.0` and declares
`go 1.22` — three go-messaging minors behind Tether root's v0.4.0, let alone
the unreleased vNext content. Not a T01 blocker (T08 owns go-tether-client),
but material to record in the baseline matrix since T08 depends on T07 which
depends on everything else — by the time T08 starts, go-tether-client's own
compatibility gap will already be several tasks stale.

## 3. Migration/compatibility contract

### 3.1 Identity mapping: registry URN vs. LogicalAgentID vs. SESSION vs. provider ID

Four distinct identity vocabularies are now in play and must be reconciled
without merging by display name or profile (architecture doc, "Registration...
is authenticated and idempotent against an owner-scoped external key, not
display-name matching"):

1. **`internal/registry` URN** (v0.6 directory service) — a durable
   participant/publication record: `agent`, `project`, or `group` kind,
   owner-scoped, survives offline. This is the architecture's "durable
   participant/publication record" lifetime.
2. **`logical_agents.id` (`LogicalAgentID`)** — Tether's pre-existing durable
   actor row. `sessions.logical_agent_id` FKs to it. Pre-dates the registry
   and pre-dates canonical `SESSION`.
3. **Canonical `SESSION`** (agentkit `agentlaunch.SessionBootstrap`) — the new
   provider-neutral continuity-episode identity: `SessionID`, `Intent`
   (`preassigned|resume|compact|fresh|fork`), `ParentSessionID`, `ActorRef`
   (optional durable actor, separate from `DefinitionRef`),
   `ProviderMappings []ProviderSessionMapping{Owner,Provider,NativeSessionID}`,
   `LegacySessionIDs`, `Publication` (`private-local|published-local|tether-hosted`).
4. **Provider-native session ID** — e.g. Claude CLI's `session_id`. Tether
   currently stores exactly **one** such ID *per logical agent*
   (`logical_agents.claude_session_id`, migration 0007), documented as
   "each logical agent holds at most one active claude conversation... v0.0.4
   (latest-only resume)". This is precisely the anti-pattern the architecture
   calls out to retire: "never select whichever matching process happens to
   look newest" — a provider ID is a *per-session* mapping
   (`ProviderSessionMapping`), not a *per-actor* singleton, so two concurrent
   or historical sessions for the same logical agent cannot both be
   represented today.

**Contract:** T02 introduces canonical `SESSION` as a new, additive identity
scoped to `sessions` rows (not `logical_agents`), carrying
`ProviderSessionMapping[]` (owner=`tether`, provider=e.g. `claude-code`,
native_session_id=the existing `claude_session_id` value) as an explicit
mapping rather than a bare column. `logical_agents.id` continues to serve as
the durable-`ActorRef` when `Actor.Durable=true`; a fresh one-off boot gets a
`SESSION` with no `ActorRef`, matching "a one-off session need not manufacture
a permanent actor record merely to send a message." `registry` URNs map
1:1 onto `ActorRef`/`DefinitionRef` for anything explicitly published;
private/local sessions never touch the registry. No existing `logical_agents.id`
or `registry_entries.urn` value changes on migration — this is purely an
additive join, preserving "old URNs remain resolvable with explicit
provenance" (T02 acceptance).

`logical_agents.claude_session_id` becomes a read-only compatibility mirror
of the *current* session's provider mapping during the migration window
(existing single-active-conversation callers keep working unmodified) and is
formally superseded once T02's `sessions`-scoped provider-mapping table is
the write path — do not drop the column in T01/T02; schedule its retirement
for T12's "legacy endpoint disposition" per the execution contract ("no
silent loss").

### 3.2 Message storage: which authority, and how legacy rows migrate

Per go-messaging's own compatibility guide
(`libs/go-messaging/docs/messaging-vnext-compatibility.md`, §"Authoritative
storage"): **do not shadow-write the same logical message into two
independent authorities.** Applied to Tether's now-confirmed reality (§2.3-§2.5):

- `broker_envelopes` (0005) — live-write, dead-lifecycle (§2.10). T03 does
  not migrate it into `delivery` (it serves the distinct multiplexor use
  case, not general agent messaging); it is dead-lettered from any new
  delivery-lifecycle semantics and either gets its `delivered_at`/`consumed_at`
  columns wired up properly as part of the `/broker/*` consolidation
  `0010_messages.sql` already promised, or is explicitly retired at T12 —
  either way not silently left half-alive.
- `messages` (0010, 0013, 0014, 0016) — the live go-messaging-aligned table,
  and the real migration target, **including its group rows** (§2.5:
  `internal/registry`'s raw-SQL group write/read path is not a separate table,
  it's a second code path onto this same table and must be reconciled in the
  same T03 pass, not deferred as a T04-only concern). Tether never adopted
  the `mailbox` subpackage (confirmed, §2.11) and `mailbox`'s tuple addressing
  is a poor structural fit for Tether's URN-based schema anyway — T03 targets
  the URN/`Address`-based `delivery` package instead. T03 writes a
  Tether-specific projection (`internal/store`/`internal/registry` rows →
  `delivery.EnqueueRequest`-equivalents) following the *policy*, not the
  literal function signature, of `delivery.MigrateLegacyMailbox`: default
  `LegacyMailboxHoldAmbiguousUnread`-equivalent — unread rows with no clear
  delivery-obligation history import as dead-lettered pending authorized
  redrive, not blindly replayed; read/archived rows import as completed
  history without fabricating `host_accepted`/`turn_submitted`/`consumed`
  receipts they never earned. Because `messaging_store.go` is already a
  fully contract-conformant `messaging.Store` (§2.4.3), this is an additive
  adoption of `delivery.Store` alongside the existing store, not a
  from-scratch rebuild.
- `messages.read_at`/`archived_at` (attention) stay independent of the new
  `delivery` receipt stages (`persisted`/`lease_acquired`/`host_accepted`/
  `turn_submitted`/`consumed`/`failed`/`dead_lettered`/`canceled`), exactly as
  CONTRACTS.md requires. Confirmed no conflation exists today (§2.4.2) — only
  a new receipt-stage column set is added alongside them, no behavior change
  to existing List/MarkRead/Archive.
- `group_urn`/`group_seq` (0016) — once T03/T04 route group posts through the
  shared delivery core instead of `internal/registry`'s raw-SQL path, group
  fanout gets the same per-recipient receipt tracking as ordinary messages
  for free; T04 freezes the resolved recipient set for an *accepted* delivery
  (architecture: "Freeze resolved recipients for an accepted fanout
  delivery... later rebinding must not move a retry to a different actor");
  the existing `group_seq` per-group counter is a reasonable ordering
  primitive to keep as-is.

### 3.3 Public endpoint versioning and legacy disposition

- New canonical HTTP/CLI/MCP surface versions as `v1` of the reliable-delivery
  contract (receipt-stage-aware read/send/claim/ack). Existing `/messages/*`
  (destructive-`Inbox`-shaped) and `/broker/*` (confirmed live, §2.10) are
  retained as explicitly-labeled compatibility endpoints per TETHER-PLAN T05
  ("Retain explicitly versioned legacy entry points with safe compatibility
  behavior"), not silently removed in this task or in T03-T05.
- Final retirement timeline for `/broker/*` and for
  `logical_agents.claude_session_id` as a write path is T12's "legacy
  endpoint disposition" deliverable — T01 does not retire anything.

### 3.4 Process/transport unification (T05) and authorization (T05) — now precisely scoped

Two structural gaps confirmed in §2.6/§2.7/§2.9 are T05's actual acceptance
criteria, not abstract goals:

1. **Three independent write surfaces onto the same SQLite file exist today**:
   the daemon's HTTP handlers, the separate `mux mcp` OS subprocess (its own
   store connection, §2.7), and `internal/registry`'s raw-SQL group path
   (§2.5). T05's "one daemon-owned service" means literally routing all three
   through the same in-process `Service`/`Store` instance — the MCP adapter
   either becomes an in-process component of the daemon rather than a
   separate `app.New()`-constructing subprocess, or every MCP write goes
   through the daemon's HTTP/UDS client instead of a direct store handle
   (mirroring how the CLI already does it correctly, §2.8). Either fix also
   closes the MCP-send-invisible-to-SSE-subscribers gap (§2.7) as a side
   effect.
2. **No sender/recipient authentication exists on `/messages/*` or
   `/broker/*` today** (§2.9). T05 binds claimed sender/recipient to an
   authenticated local principal (same-host UDS trust is an acceptable v1
   answer per existing ADR precedent for groups' D7, but it must be an
   explicit, applied rule on every message/broker endpoint, not merely true
   by accident of same-host deployment) — body-supplied `from`/`to` and
   query-supplied `?as=` must stop being sufficient on their own once a
   principal concept exists.
3. **`internal/federation.Router` is fully redundant with `go-messaging.Router`
   (already available at Tether's current v0.4.0 pin) and is never actually
   invoked** (§2.6). Retire the Tether-owned copy and wire `Service.Federation`
   into the unified service from item 1 — this was blocked on nothing but is
   currently just unconnected code.

### 3.5 Rollback

Mirrors go-messaging's own package-level rollback shape (§"Rollback" in the
compatibility guide), applied at the Tether layer:

1. Stop calling the new delivery-projection path in `internal/store`/`internal/api`.
2. Continue serving `/messages/*` (and `/broker/*` if still live) from the
   existing `messages`/`broker_envelopes` tables — those code paths are not
   deleted until T12 confirms migration success.
3. Leave any new `messaging_*` delivery tables in place for audit, or drop
   them under a host-owned migration after backup — never as an automatic
   side effect of a failed rollout.
4. All rollback drills run against **copied fixture DBs**, never
   `~/.tether/state/tether.db` (execution-contract requirement; also see
   AGENTS.md/CLAUDE.md canonical state root).

## 4. Pre-migration build/test evidence

### 4.1 Root Tether `make check` (published deps, no workspace) — baseline

Ran clean on `main` @ `5406114` before any change in this task:

```
$ make check
...
DONE 1378 tests, 6 skipped in 2.768s
govulncheck ./...
No vulnerabilities found. (0 reachable of 3 total findings in required modules)
Coverage (aggregate, cross-package): 65.1%
```

The 6 skips are pre-existing/expected (`SkipEventFanout=true` compliance
cases; `BinarySkip=true` cases needing a provider CLI binary not installed in
this environment) — not induced by this task.

### 4.2 Fixed as routine pre-migration hygiene: `apps/sysop` nested-module drift

`apps/sysop/go.mod` (a separate Go module, not covered by root `make check`
or by CW-20260905-0054's sweep) was still pinned to `agentkit v0.2.0`,
`go-messaging v0.2.1`, `go-otel v0.1.0`, `go-providers v0.23.0`,
`go-runner v0.5.0` — and failed to build (`go: updates to go.mod needed`)
against its own committed `go.sum`. Ran `go mod tidy` in `apps/sysop`
(published deps only, no workspace) to catch it up to the same published
versions root already uses. After the tidy:

```
$ go -C apps/sysop build ./...   # exit 0
$ go -C apps/sysop vet ./...     # exit 0
$ go -C apps/sysop test ./...    # ok  .../tether/apps/sysop/cmd/tether_sysop  0.334s
```

This is in scope for T01 because every downstream task (T02+) edits packages
`apps/sysop` imports (`internal/registry`, `internal/store`, `internal/api`,
`internal/mcpadapter`) — leaving it broken would have silently regressed
`apps/sysop` with every subsequent task's own build being green. Diff is
`go.mod`/`go.sum` only, no source changes, not committed yet (bundling with
T01's actual commit once this document's review pass closes).

### 4.3 Known pre-existing flake, confirmed unrelated to go-messaging

`go-tether-client`'s `TestAIChatStreamParsesEvents` failed once under the
temporary workspace (`stream err = <nil>`). Confirmed by 40 consecutive runs
**without** the workspace (i.e. against go-tether-client's own currently
pinned `go-messaging v0.2.0`, no vNext code involved at all): **18/40 failed**
with the identical signature. This is the same closed-`errCh`-read race
already identified and fixed in Tether root's own `ai_client` by
CW-20260905-0054 ("Also fixed: Pre-existing flake in TestClientAIChatStream...
both read a closed errCh as a stream error, and errCh closes on clean
completion") — go-tether-client has its own independent copy of that stream
code and never received the equivalent fix. It does not touch go-messaging
and is not a library defect; it is an unrelated pre-existing go-tether-client
bug. Recorded here per the execution contract ("tests establish pre-migration
behavior") and flagged for T08 (the task that already owns go-tether-client
changes) rather than fixed under T01.

### 4.4 Workspace build verification (unreleased vNext content)

Under the temporary `GOWORK` (§1):

```
go build ./...                                    # tether root: exit 0
go -C apps/sysop build ./...                       # exit 0
go -C libs/go-tether-client build ./...            # exit 0
go -C libs/go-tether-client test ./...             # 1 known pre-existing flake (§4.3); no new failures
```

No shared `go.mod`/`go.sum` files were committed with workspace-only
resolution; the workspace file itself lives under this session's scratchpad,
not the repo.

## 5. Open items carried to later tasks (not T01 blockers)

All items below are now confirmed facts (§2), not open questions — listed
here as a punch list for whoever picks up each downstream task, so they
start from evidence instead of rediscovering it:

- **T03**: reconcile `internal/registry`'s raw-SQL group write/read path
  (§2.5) onto the shared delivery core in the same pass as the general
  `messages` migration — it's a second code path on one table, not a
  separate migration target. Also decide `broker_envelopes`' fate (§2.10,
  §3.2): wire up real delivery-lifecycle tracking or retire at T12, but stop
  leaving `delivered_at`/`consumed_at` permanently unset.
- **T04**: group rooms/roles/mentions/archive are already shipped and tested
  (§2.5, ADR-0042) — read ADR-0042 in full before scoping. Real net-new work
  is the scoped role/slot-binding primitive plus per-recipient durable-fanout
  receipts, not room construction.
- **T05**: three concrete, evidenced fixes, detailed in §3.4 — (1) unify the
  daemon HTTP path, the separate `mux mcp` subprocess's direct SQLite access
  (§2.7), and `internal/registry`'s raw-SQL group path (§2.5) onto one
  in-process service; (2) add sender/recipient authentication to
  `/messages/*` and `/broker/*`, both currently wide open (§2.9); (3) retire
  `internal/federation.Router` for the already-available `go-messaging.Router`
  and actually wire `Service.Federation` in, since it's built, tested, and
  simply never called today (§2.6).
- **T03/T04/T05, low-effort but real**: `internal/registry`'s and
  `internal/federation`'s code (and `docs/api/README.md`) still assume
  `go-messaging v0.2.1`'s constraints (no `KindGroup`, no library `Router`).
  Both are already available at Tether's current v0.4.0 pin (§2.5, §2.6,
  §2.13) — the version bump already happened via the unrelated
  CW-20260905-0054 sweep; only the code catching up to it remains.
- **T12**: correct `docs/api/README.md`'s `/messages` route table (5 of 13
  routes missing, §2.13) and the stale go-messaging-version prose alongside
  it, as part of the documentation deliverable.
- **T08**: `libs/go-tether-client`'s three-minor go-messaging lag and
  `go 1.22` pin (§2.14).
- CW-20260904-0091 ("Modernize Tether onto current agentkit and go-messaging
  foundations") remains `todo` in Torque but its stated acceptance criteria
  (current agentkit/go-messaging/go-providers versions, no regression) are
  already satisfied by the now-`done` CW-20260905-0054. Per TETHER-PLAN T01
  scope ("Do not wait on stale broad modernization task CW-20260904-0091
  when its relevant work has demonstrably landed"), this task is not treated
  as a blocker here. Its own status is not mine to change; noted for whoever
  owns cleanup of that record.
