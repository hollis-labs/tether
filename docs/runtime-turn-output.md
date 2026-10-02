# Runtime turn output and interruption state

Tether feeds one `turnoutput.Reducer` per session from the synchronous native
`TypedEventCallback` or the ACP wrapper's raw Activity observer. These paths
precede rendering and do not depend on lossy event fanout. Session termination
calls `Flush("process_exited")`; shutdown flushes before closing storage.
The public event and staged-message contract is documented in
[the API reference](api/README.md#sessionturn_output).

`Service.SessionTurnOutputState(sessionID)` exposes an internal `TurnOutputState`
for the reply interruption service. It invokes no runtime or HTTP operation.

| Method | Contract |
| --- | --- |
| `CurrentTurn()` | Stable Tether submission ID and completion channel; empty ID means no turn in progress. |
| `LockSubmission()` | Acquires the per-session submission gate and returns its unlock function. |
| `TurnAccepted(id)` | True only if that marker is still current and the runtime has accepted it. |
| `CompletedTurn(markerID)` | Returns the bound reducer Output turn ID, only after that marker completes through an Output. |

A provisional marker is installed before calling runtime input, so it is visible
while submission is blocked and cannot miss synchronous completion. Successful
submission or the first event that opens a reduced turn marks it accepted. A
successful return cannot resurrect a marker completed during the call. Steering
keeps the existing marker. A rejected submission settles only a marker created by
that submission; it cannot settle an existing or later turn.

The submission gate protects marker installation and is released before calling
blocking runtime input. The cancellation caller snapshots its intended turn,
acquires the gate, takes a fresh snapshot, and checks both ID and acceptance before
calling runtime cancellation. It releases the gate before waiting on completion.
A changed ID must not cancel a successor; a provisional marker must not cancel a
runtime turn that has not started. The cancellation service owns those typed
refusals.

The reducer binds its turn to the submission marker. Native feeds use the marker
ID; ACP keeps its runtime-provided turn ID. Completion is recorded only for an
Output matching that binding, after persistence and publication attempts, before
the synchronous callback returns. The captured channel then closes and remains
valid across subsequent turns and session removal. Empty final output also
completes the marker even though no event is published.

A failed submission or process exit before any reduced Output closes the channel
without recording completion. Cancellation must check `CompletedTurn`, rather
than treating a closed channel alone as successful interruption. Completion
records retain the latest 64 markers; a missing record fails closed.

Selected routed text is stored once in a hidden staged message. The channel
router attaches that existing ID rather than calling ordinary message send to
create a second body or enqueue a mailbox delivery. Unattached stages remain
subject to the documented retention window.
