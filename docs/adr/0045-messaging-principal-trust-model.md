# ADR 0045: Messaging Principal Trust Model — Explicit Same-Host Trust, Not Universal Authentication

**Status:** Accepted — 2026-09-06
**Context:** Sprint messaging-vnext-20260906, T05 (CW-20260906-0036), plan CW-20260906-0023
**Extends:** ADR-0002 (Daemon Transport), ADR-0023 (Message Routing Contract), ADR-0040 (Messaging Federation)

## Context

T05's acceptance criteria require that "trusted per-user mode is explicit,
not assumed universal authentication," and that claimed sender/recipient
identity be bound to "authenticated local or opt-in external principals."
Before this ADR, that trust boundary existed only as scattered code comments
(`internal/api/groups.go`: *"Caller-identity surrogate. v060-05 has no token
auth (lands in v060-03)... replaced by v060-03"*) referencing a token-auth
milestone that a direct search of the codebase confirms was never built.

A dedicated investigation for this task (read-only, T05 design research)
established the following facts precisely, and they are the basis for this
decision:

- **No peer-credential mechanism exists anywhere in this codebase.** No
  `SO_PEERCRED`, `ucred`, `getpeereid`, or equivalent syscall-level identity
  extraction exists for Unix domain socket connections. Tether has no way to
  determine which OS process or user is on the other end of any connection.
- **The daemon's transport is not UDS-exclusive.** `internal/daemon/listener.go`
  supports both `unix:` (the default, `~/.tether/run/muxd.sock`) and a real,
  working `tcp:host:port` listener, per ADR-0002 — which itself explicitly
  deferred the auth model: *"auth model is out of scope here."* Nothing
  restricts the `tcp:` option to loopback-only at the code level; every
  documented example happens to use `127.0.0.1`, but that is convention, not
  enforcement.
- **Every messaging/broker/group HTTP handler trusts a self-asserted
  identity.** `from`/`to` on `POST /messages`, `Sender`/`Recipient` on
  `POST /broker/envelopes`, and `by`/`member`/`?as=` across `/groups/*` are
  all read directly from the request body or query string with no
  verification step. `GET /broker/envelopes/{id}`, the `recipient`-scoped
  branch of `GET /broker/envelopes`, and — found by a distinct review pass
  on this task's own diff — `GET /messages/{id}`, `GET /messages/inbox`,
  `GET /messages/list`, and `GET /messages/thread/{id}` additionally
  required **no identity claim of any kind** before this task (not even a
  self-asserted one) — a strictly weaker case than the rest of the surface,
  closed by this task (see Consequences). The `/messages/*` gap was
  particularly material because this same task rewires
  `mux_message_get/inbox/list/thread` (the MCP tools) to call exactly these
  endpoints.
- **The daemon composes a single OS-user's session/message state.** There
  is no per-tenant isolation model, no user database, and no concept of
  "another local user's Tether instance" — the entire daemon process,
  SQLite file, and every catalog/session/message row belong to one
  filesystem-level owner.

Given these facts, two paths were available: (a) build a real principal/
token system — session-scoped bearer tokens minted at launch time,
propagated through the launch-bootstrap chain, and verified on every
transport — or (b) make the actual, currently-achievable trust boundary
**explicit** and close the concrete gaps that make it weaker than intended,
while deferring a genuine multi-principal capability system to the task
that owns launch-time identity propagation.

## Decision

**Tether's messaging surface uses an explicit same-host, single-user trust
model, not per-message cryptographic authentication, as of this ADR.**

1. **The trust boundary is "whoever can reach the daemon's configured
   transport."** For the default `unix:` configuration, that is enforced by
   filesystem permissions on `~/.tether/run/` (created `0750` — owner
   rwx, group r-x, other: no access at all) rather than by anything in the
   HTTP handler layer. For an operator-opted-into `tcp:` configuration, the
   operator is choosing to widen that boundary to "anyone who can reach the
   configured host:port" — this ADR does not add network-level
   authentication to that path; operators who enable `tcp:` on a
   non-loopback address are responsible for their own network isolation,
   exactly as ADR-0002 already implied.
2. **Self-asserted `from`/`to`/`as`/`by` identity claims are accepted as-is**
   across `/messages/*`, `/broker/*`, and `/groups/*`. This is a deliberate,
   reviewed continuation of existing behavior, not a newly discovered gap
   left unaddressed — `TestMessageSend_SenderIdentityIsSelfAssertedByDesign`
   (internal/api/t05_security_test.go) locks this in as tested, intentional
   behavior so a future change to it is a reviewed decision, not a silently
   noticed "oh, that was never checked."
3. **Every identity-bearing operation still requires an explicit claim.**
   Before this task, `GET /broker/envelopes/{id}`, the recipient-scoped
   branch of `GET /broker/envelopes`, and the entire `/messages/*` read
   surface (`GET /messages/{id}`, `/messages/inbox`, `/messages/list`,
   `/messages/thread/{id}`) required no claim at all — worse than the
   write side of the same surface (`read`/`archive`/`unarchive`/`consume`),
   which already required `?as=`. All of these now require `?as=`:
   `GET /messages/{id}` and `/messages/thread/{id}` check it against each
   message's actual sender/recipient (thread results are scoped to the
   turns the caller is actually party to, since a thread is a shared
   request/reply chain, not a single mailbox); `/messages/inbox` and
   `/messages/list` require it to equal the `?to=` mailbox being read.
   This closes a concrete "zero assertion" hole without pretending to add
   cryptographic verification it doesn't have.
4. **Capabilities that are NOT "permission to send" are checked
   independently, even under this trust model.** `POST /messages/notify`'s
   `session_id` override previously let a caller address a message to one
   URN while waking a *different, unrelated* running session by ID —
   conflating "I can send a message" with "I can wake any session,"
   which the architecture explicitly separates. `resolveNotifySession` now
   requires the explicit override to actually correspond to the message's
   resolved recipient (exact session match for `msg://session/...`,
   matching `LogicalAgentID` for `msg://agent/...`) — see
   `TestMessageNotify_RejectsUnrelatedSessionOverride` and
   `TestMessageNotify_ExplicitSessionIDMustMatchSessionKindRecipient`.
   Group send/read remain gated by real, pre-existing service-layer
   membership checks (`SendToGroup`, `ListGroupMessages`) — a caller can
   claim to be any URN, but the service still requires that URN to
   legitimately be a member of the specific group being addressed
   (`TestGroups_Send_ImpersonatingExistingDifferentMemberStillForbidden`).
5. **Federation does not widen this trust model.** `internal/federation.Router`
   (now actually wired into the live HTTP handler as of this task — see
   `cmd/mux/daemon.go`'s `newFederatedMessageStore`) routes by URN authority
   to a peer's own `Store`; the peer hop itself is a separate, already-
   documented-as-deferred concern (ADR-0040: cross-host transport
   authentication is "program task M2," out of scope here too).

## What this ADR does NOT do

- It does not add cryptographic or token-based authentication. No bearer
  token, no session credential, no signature scheme.
- It does not add peer-credential (`SO_PEERCRED`) extraction. That would
  only cover the `unix:` transport, not `tcp:`, and was judged a
  disproportionate addition for the value it would provide given the
  single-user deployment model this trust boundary already assumes.
- It does not change who can reach the daemon. The `unix:`/`tcp:` transport
  choice and its filesystem/network exposure remain exactly as ADR-0002
  left them.

## Where real multi-principal authentication belongs

The architecture doc's "Bind sender, reader, subscription, group-management,
and wake permissions to authenticated principals" is a real, larger
capability this ADR does not close — it requires session-scoped credentials
minted at launch time and propagated through the launch/bootstrap chain,
which is T08's explicit scope ("provider-neutral local bootstrap/
registration helper that agent-setup can invoke at the launch/host
boundary... accept preassigned SESSION and idempotent fallback"). Building
that infrastructure inside T05 would have meant inventing launch-time
credential propagation ahead of the task that actually owns the launch
boundary — exactly the kind of "new host redesign" the execution contract
warns against. This ADR's job is narrower and complete on its own terms:
make the CURRENT trust boundary explicit, tested, and free of the specific
concrete holes (broker's zero-assertion reads, the notify session-override
gap) that were weaker than the boundary itself required.

## Consequences

- `internal/api/broker.go`'s `GetEnvelope` and the recipient-scoped branch
  of `ListEnvelopes` now require `?as=` matching the envelope's sender or
  recipient (previously: no claim required at all).
- `internal/api/messages.go`'s `handleMessageGet`, `handleMessagesInbox`,
  `handleMessagesList`, and `handleMessagesThread` now require `?as=`
  (previously: no claim required at all on any of the four). The
  `internal/client.Client` and `mux messages get|thread` CLI/`mux_message_get`/
  `mux_message_thread` MCP tools were updated to supply it; `mux_message_inbox`/
  `mux_message_list`/`mux messages inbox|list` needed no interface change
  since their existing `to` argument already serves as the claim (the
  server requires `as == to` for a mailbox read).
- `internal/api/messages.go`'s `resolveNotifySession` now verifies an
  explicit `session_id` override actually corresponds to the message's
  resolved recipient before waking it.
- `internal/federation.Router` is now wired into the live `MessageStore`
  seam (`cmd/mux/daemon.go`), closing the gap ADR-0040 itself documented
  ("the Router is composed onto `Service.Federation` but the MCP/HTTP
  message front-ends still call `Store.MessagingStore()` directly"). This
  wiring covers `Send`/`Inbox`/`Subscribe`/`Consume` only — `List`,
  `MarkRead`, `Archive`, and `Unarchive` still always resolve against the
  local store even when the recipient's authority is a registered peer
  (`TestNewFederatedMessageStore_ListMarkReadStayLocalEvenWhenFederated`
  documents this as a known, tested limitation, not a silent gap). Routing
  those four operations to a peer's store is deferred along with the rest
  of cross-host read/write forwarding — it needs the same peer-transport
  authentication design ADR-0040 already deferred as "program task M2,"
  not a small wiring change like the other four methods.
- `mux mcp`'s message tools now route through the daemon's HTTP client
  rather than a second, separate in-process SQLite connection — closing a
  distinct authorization-bypass surface (an MCP-originated send previously
  never passed through whatever the daemon's HTTP layer enforced, and
  never triggered its in-memory SSE fan-out either).
- Nothing in this ADR requires a database migration, a config change, or
  operator action — every behavior change is either a bug fix (notify
  override, broker zero-assertion reads) or a wiring fix (federation, MCP
  routing) within the existing trust model, not an expansion of it.

## References

- ADR-0002 — Daemon Transport (`auth model is out of scope here`)
- ADR-0023 — Message Routing Contract
- ADR-0040 — Messaging Federation — Authority-Routing Peer Configuration
- `planning/docs/messaging-vnext/T01-compatibility-contract.md` §2.9 (no
  authentication on either message surface, found during T01) and §3.4
  (T05 prep notes)
- T05 design research (session-internal): daemon transport/socket model,
  `mux mcp` composition, existing (absent) auth middleware
