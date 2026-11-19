# Conversations

A **conversation** is the durable identity a chat client attaches to. A
**session** is the PTY/process that runs it right now. The daemon keeps the
mapping between them (CW-20261118-0008) so a conversation survives the session
that runs it being relaunched or replaced.

## Tables (migration 0107)

`conversations` holds one row per conversation, its current state:

| Column | Meaning |
|---|---|
| `conversation_id` | Opaque ULID minted by the daemon. Never derived from a session ID; clients must not parse it. |
| `owner` | The owner. Today always `msg://user/local/operator` (`identity.OperatorID`; one daemon, one operator). |
| `agent` | The operative: the catalog agent (`launch.Plan.CatalogAgentID` for team members, else the logical agent). |
| `seat` | The seat the conversation is durable for: the session's `logical_agent_id`. |
| `kind` | `stable` (durable for a seat). `embedded` is reserved. |
| `generation` | Starts at 1; rises by exactly 1 each time the conversation moves to another session. |
| `session_id` | The session running it now. |
| `attempt_id` | Opaque ULID minted per generation. Not the runtime binding's attempt (that one can be a secret). |
| `worker_id` | The worker the session runs on; empty means this daemon. |

`(owner, agent, seat)` is unique for `kind = 'stable'`: one conversation per
seat.

`conversation_generations` is append-only: one row per `(conversation_id,
generation)` with the session, attempt, worker and `reason` (`create`,
`launch`, `resume`, `replace`). It lets a superseded session still resolve to
its conversation and know the generation it held.

## When a row is written

Every write happens inside the transaction that writes the session, so the
session and its conversation commit or roll back together.

| Path | Code | Effect |
|---|---|---|
| First session for a seat | `Store.CreateSessionKeyed` (`internal/store/sqlite.go`) | Insert at generation 1, reason `create`. |
| Fresh launch for a seat that has a conversation (hub or worker, team members included) | `Store.CreateSessionKeyed` via `Service.createContinued` (from `createSessionFromPlan`, `createWorkerSessionWith`) | Move, generation +1, reason `launch`. |
| Relaunch (`Continuity: relaunch`) or checkpoint resume (`intent = resume`, `parent_session_id` set) | `Store.CreateSessionKeyed` via `Service.createContinued` / `Service.resumeLogicalAgent` | Move, generation +1, reason `resume`. |
| `/sessions/bootstrap` | `Store.CreateSession` (from `handleSessionBootstrap`, unchanged) | As above, by intent. |
| Team replacement of a lost execution | `Store.prepareTeamReplacementTx` (`internal/store/session_replacement.go`) | Move, generation +1, reason `replace`, beside `session_replacements.binding_generation`. |

A session with no logical agent has no conversation. Writing the session that
is already current is a no-op, so a retried write never bumps twice. The move
is a compare-and-set on the generation read in the same transaction.

Existing seats are not backfilled: a seat gets its row on its next session.

## Bump rule

The conversation always follows the newest session written for its seat, and
each move raises `generation` by exactly 1. A bump invalidates every
attachment and writer lease held at the older generation (see the chat attach
wire); the conversation ID and its history do not change.

The chat host keeps its own binding generation. After the transaction that
moved a conversation commits, the store calls `Store.OnConversationMoved`
(a team replacement reports on `EventsCommitted`, after its caller's commit).
With `daemon.chat_wire.enabled` on, `internal/chatwire` then moves the chat
host binding to the same generation and attempt and tells the chat Service to
re-read the generation (CW-20261118-0011); every chat Control call reconciles
again, so a missed signal heals on the next read. `/terminal-attachments`
(`daemon.terminal_attachments.enabled`, CW-20261118-0012) derives its binding
with `chatwire.HostBinding` on the same chat host, and brings the chat host to
the conversation's generation before it resolves a target, so either flag can be
on alone.

## Resolver

`internal/conversation.Resolver` returns `{conversation, owner, agent, seat,
kind, generation, session, attempt, worker}`:

- `ByConversation(ctx, id)` returns the current target.
- `BySession(ctx, sessionID)` returns the current target of the conversation
  that session ever belonged to, plus `current` (false when the session has
  been superseded).
- `Resolve(ctx, target)` takes a conversation or a session ID: a superseded
  session is `ErrStale`. Terminal attachments use it as their target resolver.

It reads only the Tether store. It must never import or call
`libs/agent/sdk/chathost`, because terminal attachments call their target
resolver while the chat host's lease lock is held;
`internal/conversation/resolver_deps_test.go` enforces that with
`go list -deps`. `BySeat(ctx, seat)` serves the chat wire, which names an
agent by its seat. The chat wire (`internal/chatwire`, behind
`daemon.chat_wire.enabled`) is the first reader; see its package doc for how it
uses the mapping and what chathost cannot express.
