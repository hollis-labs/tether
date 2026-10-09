# AGY shim restart acceptance

CW-20261009-0015 supplies AGY hosting for the affected restart cases in
CW-20261003-0006 and CW-20261003-0161. Harness owns unit operations and live
provider input. This matrix is a test plan, not a live result.

With shim hosting enabled and an eligible Antigravity subprocess-per-turn
launch, Tether places `tether shim-agy` under the existing canonical shim host.
The worker invokes the published resolved AGY turn template for each input,
using `--conversation` for the native conversation observed on its first turn.
Its children inherit the resolved environment, sandbox, limits, working
directory and hosted process group. The receipt's provider PID identifies the
worker; individual AGY turn children are transient descendants. Systemd user
placement keeps the host and worker outside the daemon unit's control group.
The daemon owns only the authenticated bridge.

The worker uses AGY's supported per-turn stream-json output. It does not use
AGY's persistent stdin mode, whose result boundary is unreliable in the
published adapter, or assume an ACP invocation. Each actual turn gets a stable
worker UUID in the canonical raw journal; native conversation IDs remain
separate. Results publish only after the child exits with a valid terminal
result. A lost/replaced native conversation, authentication failure, nonzero
exit or incomplete output fails explicitly. Active-turn interruption remains
unsupported; accepted subsequent inputs run sequentially.

| Affected case | Required observations |
| --- | --- |
| Fresh owned AGY launch | Actual backend is hosted, canonical receipt/session/actor/generation matches, worker PID is alive in its own shim unit, resolved provider settings remain unchanged. A `direct_fallback` receipt is not a hosting pass. |
| Mid-turn daemon restart | Record the owned AGY start marker and native conversation; restart only the daemon unit with its normal control-group policy. Host/worker identity survives, finish marker appears, and the canonical journal produces the complete final output after reattachment. |
| Queued steer across restart | Queue a correction during the active turn; it remains queued rather than being reported as an active interrupt. The next actual turn uses the same native conversation and produces its own terminal result identity. |
| Next turn after reattachment | Same canonical Tether session, host/worker receipt and native conversation; no second host placement or fresh native conversation. Equal final text on separate turns still produces separate outputs. |
| Replay/delivery | An exact already-published result UUID does not republish a completion. Observe actual durable public output/routing receipts; raw journal presence or input acknowledgment alone does not prove delivery. Preserve unacknowledged notify obligations. |
| Failure/refusal | Missing/mismatched native identity, incomplete result, auth failure or failed child must not claim successful continuation or silently start a fresh conversation. Uncertain/live custody retains the existing shim fences. |

The retained isolated catalog already contains the genuine AGY provider and
owned disposable launch. No global catalog change, credential change, sandbox
weakening or KillMode change is part of this source slice. The operator verifies
the actual catalog and eligible launch before the affected campaign. Existing
Claude, Codex, workspace and strict-native design-kit results remain separate.

Source fixtures exercise real per-turn subprocesses plus mid-turn bridge
detachment, service reconstruction, canonical journal replay, queued input and
next-turn conversation continuity under a detached owned shim. The systemd
daemon-restart matrix above requires the separate Harness live campaign.
