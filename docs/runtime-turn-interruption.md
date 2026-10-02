# Runtime turn interruption

`Service.CancelTurnAndWait(ctx, sessionID, actor)` cancels the submission that
was open when the call began and waits for its bound reducer Output. A terminal
or failure Output both mean the turn ended. The result carries the stable Tether
`TurnID` and the bound `OutputTurnID`; ACP can use a different runtime output ID.
The method does not deliver a reply. The reply surface supplies a verified caller
identity as `actor` and submits its next turn after this method succeeds.

The method snapshots the intended turn before taking the per-session submission
gate, checks the current ID and acceptance again under the gate, and holds the
gate through runtime cancellation. It releases the gate before waiting. A runtime
acknowledgement alone is insufficient: the captured completion channel must close
and `CompletedTurn` must confirm the intended marker's binding. Failed submission
or exit before any Output does not qualify as successful interruption. Completion
records retain 64 markers; an absent record fails closed.

`TurnInterruptRefusal` is available through `errors.As`:

| Reason | Meaning |
| --- | --- |
| `no_turn_in_progress` | No turn is open at entry or at the gated check. No runtime cancellation is sent. |
| `turn_superseded` | The captured turn changed before cancellation, or settlement has no matching Output. A successor is never cancelled. |
| `turn_not_yet_started` | The submission is provisional. It has neither returned successfully nor produced a reduced body/terminal event. Cancellation is refused immediately. |
| `unsupported` | The actual session or adapter cannot cancel its turn. Also matches `agentsessions.ErrInterruptUnsupported` through `errors.Is`. |

Missing sessions preserve `ErrSessionNotRunning`; provider refusals and caller
cancellation propagate. The reply caller decides how to handle the typed refusal;
no-turn and superseded results permit ordinary next-turn delivery. Waits use the
caller context. Call this method from the request goroutine, never the runtime's
synchronous reader callback.

Native sessions remain owned by agentkit. `Manager.InterruptTurn` forwards the
session's real `TurnInterrupter`; `NewPlanScopedAdapter` preserves the inner
adapter's optional stream/RPC interruption interfaces without adding them to
unsupported adapters. ACP sessions forward the wrapper's advertised delivery
capabilities and existing `session/cancel` path. Capability queries use this same
scoped-adapter constructor and require `RoutingInterruptWired(providerID)` as well
as the descriptor's `cancel_turn` advertisement.

`session.turn_interrupt_requested` is persisted before the runtime call, and
`session.turn_interrupt_completed` records the result. Both identify actor,
session, and intended turn; successful completion also names the bound output.
An unavailable intent audit prevents cancellation. Outcome audit failures are
returned to the caller. Payloads contain no reply body.

Captured Codex replay exercises interruption and the next turn on the same
process; scripted ACP exercises `session/cancel` and a reusable next prompt.
Deterministic fake-runtime tests cover provisional submission, stale snapshots,
blocked accepted input, gate release before the terminal wait, context cancellation,
and completion without an Output. Default tests launch no installed model CLI.

This primitive and its audit contract are CW-20261002-0067 / ADR 0049. The HTTP
reply-to-sender integration is supplied separately by CW-20261002-0065. The marker
contract is in [runtime-turn-output.md](runtime-turn-output.md).
