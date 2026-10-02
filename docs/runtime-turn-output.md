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

## Attaching staged output to a channel

The daemon installs `turnrouting.Router`; lightweight `app.New` and catalog-only
services do not install a worker. The live `session.turn_output` subscription
accelerates routing. Startup and periodic scans also page the durable stage queue,
including messages whose event failed to persist or was dropped from fanout.
Each scan handles at most eight pages of 128 IDs and resumes its cursor on the
next tick. Complete sweeps revisit failed IDs; retries back off from one second
to one minute. There is no permanent hold: normal 30-day retention can purge an
unattached stage. Expired stages cannot attach, even before their bodies are
purged; periodic scans exclude them and discard their retry records. Shutdown joins the worker
before closing the database.

The internal attach API is:

```go
channels.Service.AttachExisting(ctx, channels.ExistingMessage{
    MessageID: stagedID,
    Channel: route.Channel,
    SessionID: sessionID,
    Actor: routerAddress,
    LaunchDisplayName: displayName, // optional; defaults to the launch ID
}) (gomsg.Envelope, error)
```

`Store.AttachChannelMessage` implements the optional
`channels.ExistingMessageBackend`. It checks the persisted resolved route and
selected kind, and invokes the normal channel publication authorization hook
before opening the transaction. The router supplies the session URN as the
publisher principal and envelope sender; `msg://service/local/turn-router` is
the audit actor. The router does not synthesize token scopes. A future
scope-based authorization policy must explicitly authorize trusted daemon-internal
routing; identifying the session sender alone does not supply token grants.
A denial leaves the stage hidden for retry.

One transaction releases the existing body, addresses it to the channel, uses the
session ID as its thread, indexes the publication, and records durable
`session.turn_routed` audit with actor, publisher, session, turn, channel and
message IDs. Repeating the same attachment returns the original envelope without
another body, publication or audit. A conflicting destination or sender fails.
This path never calls ordinary send, mailbox fanout or delivery enqueue. The audit
is committed directly with publication and is available in durable event history,
not through live bus/SSE fanout. Channel history follows publication commit order;
if a turn is temporarily denied, a later turn can attach first. Session and turn
metadata identify the original outputs; channel order does not promise model-turn
order across retries.

The original turn metadata remains on the channel message, with `launch_id` and
`launch_display_name` added. The catalog launch currently has no separate display
name, so daemon routing uses the launch ID. Replies use the sender; this API adds
no reply-target setting.

`RoutingWiring()` reports installed producer kinds and the running worker;
`RoutingRuntimeKinds(runtimeID)` reports registered source detectors using primary
runtime IDs. The reducer consumes the synchronous feed. Claude and Codex register
question tools and permission denials; Antigravity registers denials, with no tool
ID available for its approval signal. Other current sources conservatively report
final and failure. A question or approval is classified only when the turn ends
on that signal. Approval text can contain the refused command line. Terminal
outputs are emitted for lifecycle accounting and are never a routable kind.

A logical-agent resume carries the checkpoint source session's persisted resolved
route, including an absent opt-in, into the new session. Catalog changes do not
replace an API/CLI route override on resume. Legacy checkpoints without a source
session retain catalog resolution. The daemon's close hook calls `Service.Close`
after draining sessions, so unfinished output is flushed before the router joins
and storage closes.

## Persistence and submission boundaries

The synchronous persistence attempt has one five-second overall budget, so it
cannot hold the turn-state lock for four successive operation budgets. Retry
workers give each metadata read, route reread, body stage and event publication
its own five-second budget within a one-minute retry age. Context cancellation
or deadline errors on reads/staging, and any event-publication error, defer output
off the reader. Other metadata-read errors log and continue with an empty
workstream; other route/staging errors log and fall back to the excerpt without a
message ID. A corrupt stored route is reread at subsequent output, without
inventing route defaults.

Retries back off from 100 milliseconds to five seconds. The volatile pool is
capped at 64 outputs / 16 MiB of text. Shutdown cancels in-flight attempts, gives
each pending output one final attempt with a five-second overall budget, then
joins workers before closing storage. Before staging, a crash, a failed final
shutdown attempt, a full retry pool or prolonged outage can still lose output,
with an explicit error log. Once staged, the router's durable scan can attach the
body even if its output event fails. Staging uses a deterministic ID from session,
turn and kind and preserves the original envelope on repeated calls; an already
staged message ID is also retained across event retries.

`session.turn_output` events follow successful event persistence order. A delayed
turn 1 can publish after turn 2 from the same session; this path provides no
per-session FIFO across retries. Consumers must use session/turn metadata to
identify outputs rather than treating event arrival order as turn order. Channel
publication has its own retry ordering, described above. Workstream metadata is
read at publication, so assignment changes apply to later turns. The legacy
`provider.permission_denied` event remains antigravity-only; the turn reducer
still runs for every runtime with its installed detectors.

Raw PTY input is a stream of keystrokes and does not prime a model-turn marker.
Semantic `SendTurn` and non-PTY `SendInput` publish provisional markers before
runtime entry. Concurrent steering shares that marker: a failed submission cannot
settle another accepted or still-pending submission. Reduced runtime events can
open and accept a marker before a submission returns.

Semantic submission and boot-entry gate acquisition honors the caller context.
Cancellation while waiting does not install a marker or leave a future gate
acquisition behind. The gate is released before the blocking runtime submission.

An accepted turn can end without text. If its terminal is repeated and the reducer
returns no Output while the host marker is still unbound, Tether closes that
marker with an internal completion marked `Synthetic` (final for Done/completed,
failure for Error/failed). It preserves any
terminal stop reason and publishes no event or durable body for that empty final.
A terminal arriving before submission returns is retained provisionally and
completed only after successful acceptance; rejected submissions record no
completion. Bound, non-empty turns still complete through the reducer. Old runtime
terminals with an already-completed turn ID cannot settle a successor.

An ID-less terminal retained during overlapping submissions is discarded because
it cannot safely be attributed to accepted steering. A rejected submission also
discards its provisional terminal; a real bound output or session exit will settle
any remaining marker. The ambiguity flag stays set until that settlement. Two
overlapping successful submissions followed only by repeated empty terminals can
therefore leave the marker open; `CancelTurnAndWait` can exhaust its default
30-second terminal wait despite the runtime having ended the turn. This is a
conservative attribution trade-off pending an upstream begin hint or stable turn
ID. Settlement clears the flag so a later empty turn can complete normally.
A duplicate ID-less terminal arriving after a new submission
is accepted remains indistinguishable from that submission's empty response and
can settle it synthetically. This preserves repeated empty-turn completion;
removing the ambiguity needs an upstream begin hint or stable runtime turn ID.
The reducer can suppress repeated identical failures: Tether retains failure in
the internal completion, but cannot reconstruct a missing durable failure output.
A repeated identical Error suppressed by the reducer can produce neither a
`session.turn_output` failure event nor a routed failure message, even though the
host marker records a synthetic failure completion.
