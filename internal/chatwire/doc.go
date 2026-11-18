// Package chatwire is the chat wire the Tether daemon serves for its own seats (CW-20261118-0011): the
// libs/agent/chat handler (chat-attach-wire.md, draft 7) mounted by internal/api at /chat/v1 when
// daemon.chat_wire.enabled is on (default off).
//
// What the host supplies, and where it lives:
//
//   - Runner (runner.go): resolves a conversation's current session through internal/conversation (never through
//     chathost) and runs one internal/chatbackend backend per session, picked by runtime: Claude PTY → claudepty,
//     Claude streaming-stdio → claudestdio. Codex app-server is a typed *UnsupportedRuntimeError until
//     CW-20261118-0009. Info: Deltas per backend (stdio true, PTY false), Approvals false, RunnerRef
//     "<session>/<attempt>". The backends' observed runs (typed into the terminal, mail wakes) go to Service.Observe.
//   - Control (control.go): the input lease is chathost's writer lease, so chat attach and terminal attach share it.
//     It is not a chat.Bumper.
//   - Log (db.go): libs/agent/chat/sqlitestore in StateDir/chat/chat.db, with the Control's rows in the same file.
//   - Authorize (authorize.go): from the caller's internal/identity principal; every conversation is owned by
//     identity.OperatorID.
//
// # Generations: one number
//
// The authority is the Tether conversations row (internal/store, migration 0107), raised by exactly one in the
// transaction that writes a relaunched, resumed or replacing session. After that transaction commits the store calls
// Store.OnConversationMoved; the Wire then has the Control reconcile (chat record → the row's generation, lease and
// active run cleared; chathost binding stepped to the same generation and attempt) and the Runner rebind to the new
// session and send the chat conversation on Replaced. The Service re-reads the generation from Control.Get, which
// reconciles too, so a missed signal (a crash between commit and hook) heals on the next read. chathost's binding for
// a conversation is HostBinding(owner, agent, seat, target): generation and attempt are the Tether row's.
//
// The chat conversation's ID is the Tether conversation's for a seat's first chat conversation. A reset opens a new
// chat conversation (nc.ID) in the same Tether conversation; it starts at the Tether generation, not at 1, because
// there is one number.
//
// # Gaps in chathost (libs/agent/sdk/chathost at bc1013d0655e)
//
// The Control keeps the chat-side state chathost cannot express in its own table and uses chathost for exclusion
// only. Precisely:
//
//  1. Lease time: chathost leases last a fixed 30 s on its own wall clock (Acquire, Takeover, Renew); chat.Control
//     takes now and ttl from the caller (default 90 s). The chat lease's expiry is therefore the Control's record;
//     the chathost lease is taken for the holder's chathost attachment and re-taken on the next Holds/BeginRun when
//     it lapsed and nobody else took it. Between a chathost lapse (30 s) and the chat holder's next call, a terminal
//     writer can take the lease; the chat holder then loses it (403 lease_required). Conversely a chat lease the
//     Control let expire keeps its chathost lease up to 30 s; the next chat acquire revokes it, a terminal waits.
//  2. No read of the current lease: chathost has no getter for the writer lease or its holder. A lease held by a
//     terminal writer is reported as a *chat.LeaseHeldError naming TerminalHolder, not the terminal's attachment.
//  3. Attachment IDs: chathost issues its own; chat.Control.Attach takes the caller's. The Control maps one to the
//     other, and keeps role, stream mode and generation, which chathost cannot be asked for by ID.
//  4. No active run, runner ref, reset or successor in chathost: kept in the Control's table.
//  5. Generation steps: chathost's CompareAndSwap advances one generation at a time with a fresh attempt per step and
//     Create starts at 1, so a conversation chathost first sees at generation N (or that moved twice while the daemon
//     was down) is stepped through placeholder attempts "<conversation>.catchup.<g>".
//  6. One transaction: Tether's store (SQLite) and chathost (a JSON file under flock) cannot commit together, so the
//     "one transaction" of the wire doc is: Tether row first (the authority), chathost after the commit, reconciled on
//     every Control call.
//  7. Agent seat: chathost registers one seat per operative; the Control uses the chat agent (the seat) as the
//     operative, so a team's per-member seats each get their own chathost agent.
//
// The resolver rule stands: a terminal attachment's TargetResolver (CW-20261118-0012) runs under chathost's lock and
// must use conversation.Resolver, never this Control, which takes chathost's lock itself.
package chatwire
