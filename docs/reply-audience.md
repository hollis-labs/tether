# Reply audience and triggering input (CW-20261021-0010)

Status: slice 1 (recording, binding, metadata). The consumer contract and fixtures
with the Tachyon session follow as slice 2; nothing here is a consumer contract yet.

Every routed agent output records **who asked for its turn**. A consumer can then
show, for example, only the replies addressed to the operator.

## What is recorded

Each input handed to a session is stored in `turn_inputs` (migration 0097) **before**
the text reaches the runtime: the session, a per-session sequence, the route it
arrived by (`source`), the sender URN, the **sender class** and the evidence for it
(`basis`), a reference (message id, reply id), and a state: `submitting`, then
`accepted` once the runtime took it, or `failed`.

| `source` | How it arrives |
|---|---|
| `chat_send` | a chat send into a running session, or the text that starts a new chat session |
| `session_turn` | `POST /sessions/{id}/turn` |
| `mcp_send_turn` | the MCP `tether_session_send_turn` tool in the local service |
| `mail_wake` | mail injected into a session by the wake path |
| `routing_reply` | a routed reply delivered as the session's next turn |
| `system` | an injection Tether makes itself (the codex prime) |

There is no separate ACP source: an ACP prompt reaches the daemon as `/turn` and is classified by its authenticated principal like any other input. A runtime with no turn feed (a PTY) produces no turn outputs to bind, so nothing is labeled for it.

## Sender class (a DEV policy, to revisit at release posture)

The class comes from **authenticated facts only**: a verified principal, or the sender
the daemon stamped on a stored message. Prose, timestamps and the copied owner-inbox
destination are never evidence. **Any doubt resolves to `unknown`, never to `operator`.**

| Evidence | Class |
|---|---|
| the **shared operator credential** (the operator token: the human CLI and local agents on the operator-authorized connection both hold it) | `unknown` (basis `shared_operator_credential`) |
| a paired device holding an interactive scope (`operate`, or `tether.operate`) | `operator` (the only source of `operator`) |
| a paired device holding only the host-credential `worker` scope (worker devices, event relays) | `system` |
| any other device (read-only, admin-only), a principal of unknown kind | `unknown` |
| a session or agent principal | `agent` |
| a service principal | `system` |
| mail or a reply stored with a daemon stamp: the **verified principal that wrote it** is a session or agent principal | `agent` |
| ... a service principal | `system` |
| ... the **shared operator credential** (the operator token, including the operator-authorized MCP connection agents use) | `unknown` (basis `shared_operator_credential`) |
| ... a device principal (its scopes are not in the stamp) | `unknown` |
| no verified principal (anonymous, identity off, observe-mode `?as=`), an unstamped or `unverified` message | `unknown` |
| a Tether-made injection | `system` |

### Why mail is classified by the verified principal, not by its From

A message's `From` is a claim. A principal holding `*` or `message.send_as` (the operator
credential does) may author **any** From, and the daemon then stamps the message
`verified`: that proves only that the credential was *allowed* to claim the address, not
who wrote the mail. An agent that sends mail through the shared operator-authorized
connection with an agent `From` (or, equally, with a user `From`) is stamped
`verified` with the operator as the verified principal. So the class comes from the
verified principal recorded with the message (`from_principal`), and the shared
operator credential is `unknown`, never `operator` and never `agent`: nothing
distinguishes a human from an agent behind it. Operator audience therefore comes only
from a paired device with an interactive scope (see the table above).

**Cost.** The same rule applies to direct routes: a turn typed with the operator token
(`/turn`, MCP send-turn) is `unknown`, visible under "All" but not under "Replies to me".
The real fix is per-agent / human-distinct credentials (CW-20261020-0047).

## Binding an input to an output

When a turn's output is published, the **oldest unbound, not-failed input** of the
session is bound to that turn, by the order Tether observed itself. A failed or
never-accepted input never binds to a later turn. The audience is the input's class
only when the binding is **proven**: the input is accepted, it was not queued behind
another unbound input, and no other input is waiting. Otherwise the audience is
`unknown` and the reason is the basis:

| `audience_basis` | Meaning |
|---|---|
| `authenticated_principal`, `stamped_message_sender`, `system_injection` | proven; the class is the input's class |
| `none` | the input's sender is unknown |
| `shared_operator_credential` | mail written through the shared operator credential, which a human or an agent may hold |
| `no_input` | no recorded input (history, a provider-initiated turn) |
| `submission_unsettled` | the output arrived before the runtime confirmed the input |
| `queued_input` | the input was queued behind another |
| `queued_inputs_present` | another input is waiting, so coalescing cannot be excluded |
| `binding_unavailable` | the binding could not complete within the output deadline |

`mixed` is part of the vocabulary but slice 1 never emits it: whether a runtime merges
queued inputs into one turn is not proven, so a possibly merged turn is `unknown`.
Questions, approvals and failures of a turn share the turn's audience. A replay
republishes the journaled audience unchanged. Historical outputs have no `turn_inputs`
row and no audience field, which reads as `unknown`.

## What an output carries

Routed-message metadata and the `session.turn_output` payload gain `audience`,
`audience_basis`, `input_ids` (opaque ids) and `input_classes` (the sorted classes of
a proven binding; empty whenever the audience is `unknown`, so a consumer that filters
on it never shows a guess).

## Privacy and retention

- **Sender URNs never leave the hub database.** They are stored in `turn_inputs` only;
  an output carries the class and opaque ids, never the URN.
- `session.turn_output` is not in the `view=projection` allowlist of the managed-session
  contract, so none of this reaches the fleet feed. Adding any audience field to that
  view is a contract change and repeats the field audit.
- `turn_inputs` follows the **events retention window** (the daily sweep deletes rows
  older than the window together with the events). A consumer that needs an old
  classification reads it from the output, which carries the class itself.
- Additive and forward-only, no backfill. An older binary ignores the table.

## Consumer contract (PROPOSED: to be agreed with the Tachyon session before slice 1 merges)

Where it is read: the routed output message (its `metadata`) in Tether's message store,
and the `session.turn_output` event payload. Both carry the same four fields. None of
them is on the environment projection view.

| Field | Type | Meaning |
|---|---|---|
| `audience` | `operator` \| `agent` \| `system` \| `unknown` | who asked for the turn that produced this output |
| `audience_basis` | string | the evidence, or the reason it is unknown (table above) |
| `input_ids` | opaque ids (comma-joined in message metadata, an array in the event) | the input bound to the turn; empty when none |
| `input_classes` | classes (comma-joined / array) | the class of a PROVEN binding; **empty whenever `audience` is `unknown`** |

Invariants (enforced by the fixture generator on every case):

1. `audience` is exactly one of the four values; a consumer ignores any other value as
   `unknown` (the vocabulary may grow, e.g. `mixed`).
2. `audience` in {`operator`, `agent`, `system`} implies `input_classes` is exactly
   `[audience]` and one `input_id`.
3. `audience` = `unknown` implies `input_classes` is empty. A consumer must filter on
   `input_classes`/`audience`, never on `audience_basis`, and never on anything else.
4. A missing `audience` field (an output older than this change) is `unknown`.
5. The event and the routed-message metadata always agree.
6. No sender identity is ever present: only the class and opaque ids.

Toggle semantics: "Replies to me" shows an output iff `operator` is in `input_classes`
(equivalently `audience` = `operator`); "All replies" shows every output; an output with
`audience` = `unknown` appears only under "All replies". A consumer must not infer the
audience from the text of the reply, from nearby timestamps, or from the copied
`owner-inbox` destination (every routed output has it).

What `unknown` costs in practice: a reply to an input that was queued behind another, an
output that arrived before the runtime confirmed its input, a bind that missed its
deadline, a provider-initiated turn, and every historical output. Chat sends are
refused while a turn is in flight (409), and mail wakes are refused while the session is
processing, so the common operator send-and-wait path is proven.

Fixtures: `docs/fixtures/reply-audience/*.json`, one per audience and per `unknown`
reason, **generated by the real output path** (`TestReplyAudienceFixtures`; regenerate with
`UPDATE_FIXTURES=1 go test ./internal/app -run ReplyAudienceFixtures`). Ids are replaced
by stable placeholders.
