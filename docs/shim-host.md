# Opt-in shim hosting

The daemon can host Claude streaming-stdio and Codex app-server JSON-RPC
providers through a persistent shim. Direct execution remains
the default. Codex public output delivery has additional requirements described
below. The explicit
`tether shim-bridge --descriptor <path>` command connects stdio to an already
placed provider. `--attach` reconnects to that provider and journal using its
existing private checkpoint; it never places another provider.

## Placement and deployment

Detached placement starts a setsid child in a new OS session. It **stays in the
daemon's cgroup**. Under a systemd service with `KillMode=control-group`, it dies
with that service on stop or restart. It survives a controller process crash only
when the surrounding service or supervisor does not kill that cgroup. A
production deployment that needs providers to survive daemon restarts must use
**systemd-user**, which puts each host in a separate transient unit and cgroup.
That backend requires explicit enablement and an activation plan before any
production use. Configuration under
`catalog.defaults`:

```yaml
launch_host: direct        # direct (default) or shim
shim_host:
  journal_bytes: 268435456  # 256 MiB; minimum 2 MiB
  systemd_user: false      # explicit opt-in for the service backend
  unit_prefix: tether-shim- # names new transient units only
```

`TETHER_LAUNCH_HOST` overrides `catalog.defaults.launch_host` and is read on each
launch. Only the exact value `shim` enables hosting; any other non-empty value
selects `direct`, even when the catalog requests a shim. Doctor reports the
selection without creating a host. Unsupported runtimes, missing Linux peer
pidfds, and unavailable required confinement fall back to the existing direct
request with a diagnostic. A placement positively known not to have started a
child also falls back. Once a child may exist, an uncertain result retains the
placement and never launches a second child.

The descriptor carries the real provider's resolved command, sandbox and
resource-limit wrappers, and complete environment policy. The bridge is a
privileged control client; wrapping only it does not protect the provider. Its
environment excludes provider credentials. The host receives only the explicit
descriptor and a small infrastructure environment. Policies needing
daemon-owned loopback forwarding and resource wrappers that create an additional
service scope currently fall back before placement. Legacy workspace binds are
translated explicitly. A missing requested deny path yields `policy_path_missing`
before placement; a policy that cannot protect private state is refused.

Descriptors are owned 0600 files under 0700 directories. These permissions do
**not** hide them from a same-uid provider. While the flag is enabled, every Tether-sandboxed launch denies the entire
private shim root, including every per-session state directory and
the launch descriptor, journal and control socket. Without that denial the
provider could otherwise read its environment secrets and controller capability.
Unsandboxed sessions and arbitrary same-UID processes remain able to read these
files. The state parent is write-protected to prevent renaming the denied tree, and
user-service-manager access is denied so it cannot escape through a new unit. The
database and placement receipt contain paths and facts, never capability values.
Environment keys containing TOKEN, SECRET, KEY, PASSWORD or CREDENTIAL
(case-insensitive) are volatile by default. Callers add other volatile or secret
keys through `VolatileEnvKeys`; their values never enter the fingerprint. Stable
non-secret values still fence a changed launch request. A matching retry with
re-minted volatile values returns the original placement, whose descriptor and
provider environment retain the FIRST values; it does not refresh credentials.

Placement persists intent before submission and waits briefly for lock ownership.
An uncertain submission is inspected, never automatically launched again. A
journal pin is included in the client's hello, so an identity or journal mismatch
is refused before takeover changes the controller epoch. Inspect merges observed
facts while preserving the operation key, fingerprint and attempted-submit flag.

## Provider protocols

Antigravity direct providers remain tied to the daemon. This hosting path does
not preserve them across daemon death; Antigravity shim/ACP hosting is a separate
follow-up. Its restart-survival limit does not imply a failure in Claude or
Codex recovery.

Claude uses the stdio bridge described below. Hosted Codex uses a separate
durable protocol ledger, bound to the original session, instance, generation,
placement operation, submission attempt and journal. Its protocol checkpoint is
initialized before placement; reattachment must use that ledger and the exact
observed host. A missing or mismatched checkpoint remains an unknown outcome,
and never falls back to creating a fresh direct Codex thread. Uncertain input
effects are retained rather than submitted again.

## Codex delivery and recovery

Completed supported Codex output is projected from the retained native inbox
into `session.turn_output` and the selected message route. Final-answer items
supply final text when present; otherwise supported agent-message items supply
it. The producer commits its private retry journal before database publication.
A stable `output_id` binds the native completion source and body, so retry after
publication does not invent another public output.

Supported `commandExecution` starts, output deltas and completions are retained
as private, identity-checked turn obligations. Their original start/completion
item envelopes and output bytes survive checkpoint reopening. They do not become
agent final-answer text or certify a turn completion; a supported completed
agent reply still earns the public delivery receipt. Changed command identity,
foreign turn/item references and unsupported lifecycle shapes refuse rather
than discarding the private trace.

The delivery transaction then reinterprets the frozen native inbox, verifies the
actual public event, content identity and selected-route staging, and atomically
saves the projection with the inbox drain. Producer acceptance alone cannot
issue that delivery proof. A concurrent protocol change keeps the inbox pending
for retry; an ambiguous receipt commit refuses further input until recovery can
resolve it. Public channel attachment and terminal consumption remain separate
boundaries.

Native inbox message bytes are stored without JSON compaction or HTML escaping.
Older checkpoints that embedded messages as JSON retain an explicit legacy
marker. Before retained delivery, a trusted recovery reader can restore their
original spelling only from the canonical journal, checking exact stdout spans,
cursors, partial bytes, current receipt and unchanged checkpoint. The ordinary
conditional protocol commit preserves current execution and old-write fences.
Missing or changed evidence refuses; restoration does not drain the inbox,
advance replay, settle operations or issue a delivery/replacement proof.
An older binary may refuse the new byte encoding while retaining obligations;
rollback must not be treated as a way to force their delivery.

Reattachment uses the exact original host and durable protocol ledger. Before
admitting replay records, a reconnect settles its existing frozen inbox under a
five-second delivery context so a batch at capacity can make room. Failure or
unsupported obligations refuse that controller while retaining the private
inbox; they do not acknowledge or discard unsupported output. Projection builds
one private batch and validates it before the checked delivery transaction.

Readiness requires replay through the observed journal high water with verified
delivery; a bridge process starting is not enough. A failed readiness check
closes the unsettled controller while retaining the provider and custody. It
never repeats initialization, starts a replacement thread or resubmits an
uncertain turn.

Unsupported output unions, stderr records, unanswered server callbacks and
uncertain input remain retained and may prevent readiness. An authenticated provider exit while
a native turn is still open remains unsupported rather than certifying a
completed turn. Protocol retention, controller attachment and provider exit
alone are not public completion evidence. The source path for a surviving hosted
Codex provider does not authorize replacement of gone custody with a fresh
provider.

A separate retained-team path can replace proven-lost Codex execution after a
matching retired canonical receipt, positive absence of both recorded host and
provider PIDs, and complete store-issued historical accounting. Startup checks
accounting before retiring positively absent custody; retirement itself does not
move enrollment or launch a child. The retained receipt owner revalidates the
opaque accounting proof in the transaction that commits the new session,
lineage and current-reference remapping under the original actor and binding.
Credential and sealed MCP ceilings remain in force. Unknown or pending effects,
changed evidence, revoked authority and missing process identity refuse.

The old raw protocol ledger, custody and accepted history remain frozen. The new
placement starts with a pristine protocol ledger; the old inbox, RPC counters,
delivery checkpoint and thread state are not copied. Normal protocol and delivery
operations, reattachment, stop and detach on the historical execution refuse;
lookup errors never fall through to process control. Historical accounting remains
available. This replacement does not resubmit an uncertain turn or establish
public completion of the old execution.

## Claude delivery and recovery

The shim-specific `session.shim_status` event with state `running` and reason
`reattached` is a readiness signal: the bridge's controller hello has completed
and its fresh controller epoch has been durably saved in `bridge.json`. An
observer may then send a turn or Stop without racing that starting bridge.
The generic Manager `running` event still means only that the bridge process
started. A Stop before shim readiness can return the typed, retryable
`controller_busy` refusal; epoch fencing makes either controller order safe.
Recovery waits at most ten seconds for durable readiness. On expiry it closes
only the unsettled bridge and reports `detached` with reason `handshake_pending`,
retaining the provider and placement for later reconciliation. It never reports
shim readiness on expiry. Bridge closure has a separate three-second bound.

The bridge writes each complete stdout line before atomically saving its source
cursor and remaining bytes. Reconnect drains saved complete lines and partial
carry before the journal tail, and replays the captured `system/init` once for a
fresh agentkit session. Each attach reports the journal, epoch and replayed-event
count on stderr. A missing checkpoint is refused: recovery cannot reset input
keys or silently replay the entire journal. An explicit journal conflicting with
the checkpoint is also refused. A committed terminal exit is saved with the
cursor, so a later bridge run returns that status without waiting for another event,
with or without `--attach`.

Two stdout crash windows remain. A line consumed downstream before its checkpoint
can be delivered again. A replayed Claude `result` is suppressed when its own UUID
matches any durably published turn output's `provider_result_id` for that session. Equal
text alone never establishes identity; results without a UUID retain the
at-least-once window. Bytes written and committed while still unread in a dead
pipe can be lost. This does not promise exactly-once turn delivery. Result
deduplication does not remove the pipe-consumption window. Stderr
has no line framing or durable carry; a crash after a stderr write but before its
checkpoint can duplicate those bytes. Journal order is preserved between stdout
and stderr events, but cannot reconstruct the provider's original ordering across
its two independent pipes.

Input chunks are at most 64 KiB. A unique counter is saved before each effect and
each chunk waits for a receipt. A crash after reserving a key but before sending
loses that input; a crash or lost receipt after sending leaves its outcome unknown.
Partial writes and uncertain outcomes are fatal and never retried. The shim
bounds pipe writes at two seconds. Frames are limited to 1 MiB and stdout lines
to 64 MiB; larger lines report `line_too_long`. Final stdout without a newline is
flushed before the provider's exit status is returned.

`journal_unavailable` means journal exhaustion or write failure. Other output
gaps, including `descendant_holds_pipe`, are stderr diagnostics; the bridge
continues to flush the suffix and propagate the exit. The journal retains
acknowledged history without compaction.

Bridge infrastructure failures use reserved exit code **93**. Provider status
and signals otherwise propagate unchanged; a provider that itself exits 93 has
the same bridge status. The daemon MUST consult shim health through the checked
same-uid connection to decide whether the provider ended, even when the bridge
exits;
neither its exit code nor agentkit's bridge PID establishes provider liveness.

## Teardown

Bridge stdin EOF detaches and leaves the provider alive. Graceful daemon shutdown
marks hosted sessions `detached`, closes only their bridges and keeps their
placements unretired. This does not override a supervisor's cgroup kill policy.
Explicit user/API stop calls
host `Stop`: request the provider's bounded kill if it is still running, then
terminate the checked peer through a pidfd, escalate from SIGTERM to
SIGKILL after the configured grace and wait within a total timeout. An already
exited provider is a successful kill outcome. Teardown refuses stale PID identity
and requires Linux with peer-pidfd support to signal a live host. Other platforms
return `unsupported` for live teardown and never signal a stored bare PID. An
owned child proven exited by its waiter can be retired without a pidfd. Before
sending takeover hello,
Stop checks the same-uid socket peer credentials, equality with the recorded
`HostPID` and Linux process start time when available, and acquires that socket's
pidfd. The secret-free private receipt persists the process start time as an
identity witness. If Stop adopts a previously missing start time from the
checked pidfd, it commits that witness atomically with fsync before sending
provider control or host signals. Unsupported kernels
refuse before changing the epoch or journal. The pinned hello checks session,
instance, generation and journal. The client does not verify that the server
knows the secret: these checks do not provide mutual secret authentication or
protect against a malicious same-uid peer outside the provider sandbox.

After verified host exit, a durable retired receipt is written and the
secret-bearing launch descriptor is removed. Typed peer and hello refusals are
decisive and never permit an absence fallback. A connection-level refusal or
missing socket permits retirement only when the recorded process identity is
shown gone: a recorded start time identifies an absent or replaced process, or
the owned child's waiter proves it exited (with the recorded start time
rechecked on Linux). Systemd-user also requires the named
unit to be absent. An unreaped Linux child is proven exited only when one
stat read of the same PID contains the recorded start time and state `Z` or `X`;
a different start time, a live matching process or an unreadable stat is retained.
Peer-pidfd ESRCH requires that positive identity evidence too.
An unreachable socket, a timeout, or bare ESRCH on a stale PID is unknown and
retains the capability files; a host might have restarted under another PID.
The host Stop operation after verified retirement is idempotent,
including after a host crash or a timeout after SIGKILL. The session API refuses
Stop on completed, failed, killed or orphaned sessions with `session_not_running`;
it preserves the original state and exit code. A collected systemd unit
is a successful stop outcome after its absence is verified. The journal and
secret-free receipt remain as evidence. Retired placements are terminal: there
is no retirement/re-place API, and a new placement requires a NEW session ID.
For ordinary retirement, a same-key Place returns `outcome_unknown`; a changed
key returns `idempotency_conflict`. An exec Start failure proves no child was
created: placement is retired, its capability removed, and the initial and
same-key Place return the terminal receipt with `placement_failed`. Stop of
that receipt succeeds idempotently.

A dead host whose PID or start time was never recorded stays retained with
`outcome_unknown: host PID was never recorded; explicit retirement is unavailable`;
repeated Stop and matching Place cannot resolve it. The session stays detached
until an explicit operator retirement operation exists. This must be resolved
before the shim path is enabled by default. This
package has no operator recovery or explicit retirement operation for that
wedge. A wrapper ShimCommand that forks its host and exits also fails closed:
the child owning the socket differs from the recorded submitted process.

The daemon loads the canonical `placement.json` receipt for Inspect, Reattach and
Stop: the database placement row does not contain the process start time.
Inspect and Reattach preserve typed refusals and never merge Gone results.
Systemd teardown retains a loaded unit after stop failure and retries on the
next Stop; retirement requires verified unit absence.

Startup recovery reattaches a running host with the recorded journal and bridge
checkpoint. Ordinary positive host absence becomes `orphaned`, revoking session
principals and binding generations in one transaction. Eligible retained-team
recovery instead preserves enrollment and binding authority: confirmed-retired
non-Codex custody is archived with current fences; Codex custody needs the
historical accounting path above and keeps its old ledger/custody. Subsequent
team replacement commits a new execution before launch. A timeout, typed refusal, missing
receipt or unknown outcome stays `detached`. Reconciliation runs once at daemon
startup; it does not retry automatically during the daemon's lifetime. Inspection
is bounded to ten seconds per placement and a shared thirty-second inspection/
attachment budget, plus up to three seconds to close a pending bridge.
Placements beyond that budget remain detached with `startup_budget_exhausted`.
A launching intent with no descriptor, no receipt and zero recorded process IDs
is failed as `shim_not_submitted` after checking absence under the placement lock.
Positive host absence retires its descriptor before revoking authority. Doctor,
runtime health and `session.shim_status` report the typed reason and placement
key, backend, unit and socket. They never expose the capability or environment.
Disabling the flag retains detached placements with `shim_reconcile_disabled`
and leaves their children alone. Orphaned sessions cannot mint new principals.

Operators can inspect the named unit and restore connectivity or an original
verified private receipt, then arrange a daemon restart to retry reconciliation. They must not synthesize a
PID/start-time witness or delete retained capability files. A dead host with no
recorded identity has no recovery operation in this release; explicit retirement
is a follow-up. A loaded unit after failed stop similarly remains retained until
its absence is positively established. A new launch uses a new session ID;
ordinary same-key placement after retirement remains refused.

Placement and attach clear abandoned `.commit-*` files under the same lock used
by record writers, so a live atomic commit is never deleted.

Retirement removes the secret-bearing descriptor. The secret-free placement
receipt, bridge checkpoint, host log and journal remain as recovery evidence;
the journal defaults to a 256 MiB cap per session. No automatic retention sweep
or operator cleanup command is wired. Internal retirement and bounded retention
seams require complete controller exclusion, submission drain and descendant
containment proof; the production proof capability currently returns
`unsupported`. A submission fence or an absent unit alone does not authorize
removing retained artifacts.
Sandbox temporary directories are remembered only by the current daemon and can
remain after detach followed by a daemon restart; reattachment cannot recover
that cleanup handle. Explicit retirement of an unknown placement and cleanup of
its retained artifacts remain unavailable; retained evidence can accumulate
across daemon restarts.

The session API credential expires after seven days. It is copied into the
immutable provider environment: neither reattachment nor a matching placement
retry refreshes it. A hosted provider lasting beyond that limit loses API access.
Credential refresh is a separate follow-up.
