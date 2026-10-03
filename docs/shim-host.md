# Shim host foundation

The host and bridge packages provide a Claude streaming-stdio foundation. They
are not connected to the daemon's session launch path yet. The explicit
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
That backend requires explicit enablement. Configuration foundations under
`catalog.defaults`:

```yaml
shim_host:
  journal_bytes: 268435456  # 256 MiB; minimum 2 MiB
  systemd_user: false      # explicit opt-in for the service backend
```

The launch-host selector and daemon integration are separate work. The host
accepts the provider's resolved executable, argv, complete environment and pin
identity. Sandbox and resource limits must be applied to that provider before
placement. Wrapping only the bridge does not protect the provider.

Descriptors are owned 0600 files under 0700 directories. These permissions do
**not** hide them from a same-uid provider. The daemon integration MUST deny the
entire per-session state directory in the provider's sandbox policy, including
the launch descriptor, journal and control socket. Without that denial the
provider can read its environment secrets and controller capability. The
database and placement receipt contain paths and facts, never capability values.
Callers declare volatile and secret environment keys through `VolatileEnvKeys`;
the placement fingerprint includes their keys and excludes their values. Stable
non-secret values still fence a changed launch request.

Placement persists intent before submission and waits briefly for lock ownership.
An uncertain submission is inspected, never automatically launched again. A
journal pin is included in authenticated hello, so an identity or journal mismatch
is refused before takeover changes the controller epoch. Inspect merges observed
facts while preserving the operation key, fingerprint and attempted-submit flag.

## Delivery and recovery

The bridge writes each complete stdout line before atomically saving its source
cursor and remaining bytes. Reconnect drains saved complete lines and partial
carry before the journal tail, and replays the captured `system/init` once for a
fresh agentkit session. Each attach reports the journal, epoch and replayed-event
count on stderr. A missing checkpoint is refused: recovery cannot reset input
keys or silently replay the entire journal. An explicit journal conflicting with
the checkpoint is also refused. A committed terminal exit is saved with the
cursor, so a later attach returns that status without waiting for another event.

Two stdout crash windows remain. A line consumed downstream before its checkpoint
can be delivered again. Bytes written and committed while still unread in a dead
pipe can be lost. This does not promise exactly-once turn delivery. A later daemon
integration can suppress repeated result messages by their own identity. Stderr
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
and signals otherwise propagate unchanged. The daemon MUST consult authenticated
shim health to decide whether the provider ended, even when the bridge exits;
neither its exit code nor agentkit's bridge PID establishes provider liveness.

## Teardown

Bridge stdin EOF detaches and leaves the provider alive. Explicit user stop calls
host `Stop`: request the provider's bounded kill if it is still running, then
terminate the authenticated host through a pidfd, escalate from SIGTERM to
SIGKILL after the configured grace and wait within a total timeout. An already
exited provider is a successful kill outcome. Teardown refuses stale PID identity
and requires Linux with authenticated peer-pidfd support; other platforms return
`unsupported`, never signal a stored bare PID.

After verified host exit, a durable retired receipt is written and the
secret-bearing launch descriptor is removed. Repeated Stop is idempotent. The
journal and secret-free receipt remain as evidence until explicit retirement.
Placement and attach clear abandoned `.commit-*` files under the same lock used
by record writers, so a live atomic commit is never deleted.
