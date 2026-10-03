# Shim host foundation

The host and bridge packages provide a Claude streaming-stdio foundation. They
are not connected to Tether's session launch path yet; existing launches keep
their current behavior. The explicit `tether shim-bridge --descriptor <path>`
command connects stdio to a shim already placed by a host. `--attach` reconnects
to that same child and journal, using its private checkpoint. It does not start
another provider. A changed identity or journal is refused before input effects.

The host defaults to detached placement in a new OS session. A systemd-user
backend requires explicit enablement and uses a separate transient service with
an instance-specific name. Configuration foundations under `catalog.defaults`:

```yaml
shim_host:
  journal_bytes: 268435456  # 256 MiB; minimum 2 MiB
  systemd_user: false      # explicit opt-in for the service backend
```

The launch-host selector and daemon integration are separate work. The host
accepts the real provider's resolved executable, argv, complete environment and
pin identity. Sandbox and resource limits belong on that provider command,
before placement. Wrapping only the bridge does not protect the provider.
Descriptor capabilities stay in owned 0600 files under private 0700 directories;
the database holds paths, identity, cursors and process IDs, never capabilities.

## Delivery through a crash

The bridge writes each complete stdout line before atomically saving its source
cursor and remaining bytes. At a committed cursor, reconnect continues with the
saved partial line and journal tail. It replays the captured `system/init` once
so a fresh agentkit session reports the provider session ID. Each attach emits a
`shim.attach` diagnostic on stderr with its journal, controller epoch and number
of replayed events. Bootstrap init replay is intentional.

Two crash windows remain. A line already read downstream but not yet committed
can be delivered again. Bytes written and committed while still unread in the
pipe can be lost when that reader dies. This does not guarantee exactly-once
turn delivery. Checkpoints occur after every line, including when one journal
chunk holds multiple lines; already committed lines in that chunk are not
replayed. A later daemon integration can suppress repeated result messages by
their own identity. Stdout and stderr remain separate; journal ordering cannot
recover the provider's original ordering between independent output pipes.

Input uses chunks no larger than 64 KiB and saves a unique operation counter
before each effect. Each chunk waits for its receipt. A partial write, timeout or
lost receipt is fatal `outcome_unknown` and is never retried. The shim bounds
input pipe writes at two seconds. Frame size is limited to 1 MiB. Stdout lines
are limited to 64 MiB; an overlong line is a typed `line_too_long` failure. The
last suffix is flushed when the provider exits, even without a trailing newline.

Bridge stdin EOF detaches the controller and leaves the provider alive. An
explicit user stop must call the host's `Stop`, which kills and waits for the
provider and tears down its host. Inspection uses authenticated shim health;
agentkit's `Health().PID` identifies the bridge, not the provider.

The journal retains acknowledged history and has no compaction. Exhaustion or
write failure ends the child and reports `journal_unavailable` (the pinned shim
masks output-side `journal_full` as that code). Placement records intent before
submission: if a submission might have happened, inspect the same operation
rather than launch it again. Proven-gone placements require explicit recovery.
