# Runtime turn interruption

`Service.CancelTurnAndWait(ctx, sessionID, actor)` cancels the submission that
was open when the call began and waits for its bound reducer Output. A terminal
or failure Output both mean the turn ended. The result carries `OutputKind` and `StopReason`, the stable Tether
`TurnID`, and the bound `OutputTurnID`; ACP can use a different runtime output ID.
The method does not deliver a reply. The reply surface supplies a verified caller
identity as `actor` and submits its next turn after this method succeeds.

A successful result requires a runtime-acknowledged interrupt (or cancelled
terminal) followed by a non-flush terminal. An exit-flushed partial Output
returns `session_ended`, wrapping `ErrSessionNotRunning`, even after an ACK.
Claude can end an acknowledged interrupt with a failure Output.

The method snapshots the intended turn before taking the per-session submission
gate, checks the current ID and acceptance again under the gate, and holds the
gate through runtime cancellation. Gate acquisition honors context; gate wait,
readiness retries and the cancel RPC share a two-second bound by default. It
releases the gate before waiting, then bounds the terminal wait to thirty seconds.
`Service.InterruptCancelTimeout` and `InterruptDoneTimeout` can override those
defaults when configured before use. The runtime cancel runs separately: at the bound, the Service releases the gate
even if a non-cooperative CLI call remains blocked; its late return cannot touch
gate or marker state. The blocked runtime call itself remains until the runtime
unblocks or exits (tracked as an upstream follow-up). A caller deadline takes precedence. An
internal deadline returns typed `interrupt_timeout`. A successor observed during
or after the cancel call returns `turn_superseded`. A runtime
acknowledgement alone is insufficient: the captured completion channel must close
and `CompletedTurn` must confirm the intended marker's binding. Failed submission
or exit before any Output does not qualify as successful interruption. Completion
records retain 64 markers; an absent record fails closed.

`TurnInterruptRefusal` is available through `errors.As`:

| Reason | Meaning |
| --- | --- |
| `no_turn_in_progress` | No turn is open at entry or at the gated check. No runtime cancellation is sent. |
| `turn_superseded` | The captured turn changed before cancellation, or settlement has no matching Output. A successor is never cancelled. |
| `turn_not_yet_started` | The submission is provisional. It has neither returned successfully nor produced a reduced body/terminal event. Cancellation is refused immediately. An accepted RPC submission whose runtime handle remains absent after a bounded retry also returns this reason. |
| `session_ended` | The session exited and flushed partial output; wraps `ErrSessionNotRunning`. |
| `interrupt_timeout` | The internal gate/cancel or terminal deadline elapsed; wraps `context.DeadlineExceeded`. |
| `unsupported` | The actual session or adapter cannot cancel its turn. Also matches `agentsessions.ErrInterruptUnsupported` through `errors.Is`. |

Missing sessions preserve `ErrSessionNotRunning`; provider refusals and caller
cancellation propagate. The reply caller decides how to handle the typed refusal;
no-turn and superseded results permit ordinary next-turn delivery. Waits use the
caller context. Call this method from the request goroutine, never the runtime's
synchronous reader callback.

Native sessions remain owned by agentkit. `Manager.InterruptTurn` forwards the
session's real `TurnInterrupter`; its optional readiness query returns
`ErrTurnNotStarted` while the RPC start notification is missing. The Service
retries that error for at most two seconds, checking the intended marker before
each attempt with context-aware ten-millisecond intervals. Exhaustion returns
a typed refusal without waiting for natural completion.
 `NewPlanScopedAdapter` preserves the inner
adapter's optional stream/RPC interruption interfaces without adding them to
unsupported adapters. ACP sessions forward the wrapper's advertised delivery
capabilities and existing `session/cancel` path. Capability queries use this same
scoped-adapter constructor and require `RoutingInterruptWired(providerID)` as well
as the descriptor's `cancel_turn` advertisement.

`session.turn_interrupt_requested` is persisted before gate acquisition and the runtime call, and
`session.turn_interrupt_completed` records the result. Both identify actor,
session, and intended turn when known; early invalid-actor/missing-session returns
also record their outcome. Successful completions include kind and stop reason; successful completion also names the bound output.
An unavailable intent audit prevents cancellation. Outcome audit failures are
returned to the caller. Payloads contain no reply body.

Captured Codex replay exercises interruption and the next turn on the same
process; scripted ACP exercises `session/cancel` and a reusable next prompt.
Deterministic fake-runtime tests cover provisional submission, stale snapshots,
blocked accepted input, gate release before the terminal wait, context cancellation,
completion without an Output, and delayed runtime handles with an injected clock.
The captured transcript cannot hold its response-to-notification gap open; the
agentkit scripted replay holds it with an explicit release frame. Default tests launch no installed model CLI.

This primitive and its audit contract are CW-20261002-0067 / ADR 0049. The HTTP
reply-to-sender integration is supplied separately by CW-20261002-0065. The marker
contract is in [runtime-turn-output.md](runtime-turn-output.md).
