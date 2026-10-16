# Linux worker service

`tether service` manages an independent systemd user service without Cerberus.
It is for a worker account prepared for this purpose. Keep an existing
Cerberus-managed or manually started daemon under its current supervisor:
Tether refuses to adopt, update, restart or stop an externally owned daemon or
unit. These commands do not enroll workers, issue credentials or change
account settings.

## Prepare and install

Prepare the worker catalog with `tether init`, or copy an operator-prepared
catalog. Provider CLIs and their credentials remain local. For agent survival
across daemon restarts, explicitly configure the existing
[systemd-user shim backend](shim-host.md):

```yaml
catalog:
  defaults:
    launch_host: shim
    shim_host:
      systemd_user: true
```

Direct execution remains the default. Detached shims in the daemon's cgroup
do not establish restart survival; independently hosted shim units are required.
The service runs the existing foreground `tether serve`, including its
enforced loopback listener, exact Host/Origin guard, remote operator rejection,
protocol gate and MCP exclusion. Configure forwarded Host/Origin authorities
in the catalog as described in [the remote protocol guide](environment-protocol.md).

Obtain an **exact** release archive and its release `checksums.txt` on the hub,
verify the distribution source, and transfer both to the worker. Workers need
no GitHub access. Archive names must match the version and native architecture,
for example `tether_0.8.0_linux_amd64.tar.gz`. Tags (`latest`, `v0.8.0`),
version ranges and partial versions are refused.

Use a release containing the service and remote-protocol commands. The version
numbers below illustrate the layout; replace them with that release's exact
version.

```sh
tether --catalog "$HOME/.tether/catalog" service install 0.8.0 \
  --archive "$HOME/releases/tether_0.8.0_linux_amd64.tar.gz" \
  --checksums "$HOME/releases/checksums.txt"
tether service status
tether service status --json
```

The command snapshots the installing shell's PATH into the unit so provider
executables resolve. It does not invoke a login shell, shell hooks or providers,
or copy the shell's other environment variables or secrets into the unit.
Prepare any required provider-specific service environment independently,
using local configuration and secret references; existing provider credential
handling is unchanged.

Linger is checked before installation. If disabled, the command fails with
`linger-disabled` and prints the exact administrator command for the selected
user, such as:

```sh
sudo loginctl enable-linger 'worker'
```

An administrator runs that one-time command separately. Tether never runs
sudo or enables linger, and never installs the service as root.

The owned unit is `$XDG_CONFIG_HOME/systemd/user/tether-worker.service`
(`~/.config/systemd/user/` when XDG_CONFIG_HOME is unset). It uses
`Restart=on-failure`. It does not control `tether.service`, Cerberus units or
shim units. A verified live daemon in the selected catalog that is not this
unit's main process causes `external-install` refusal.

## Runtime layout and ownership

Runtimes live under `~/.tether/runtime/versions/<exact-semver>/`. Installation
copies the local archive into an owned `.staging-*` directory, verifies its
SHA-256 entry in `checksums.txt`, rejects archive escapes, duplicate paths,
symlinks, hardlinks and special file types, extracts, records archive and
executable digests, writes `.install-complete`, then publishes by rename.
Incomplete or changed existing runtimes are refused rather than overwritten.
Only staging created by the failed invocation is removed.

A directory lock records PID, boot identity, process start identity and an
ownership nonce. Kernel serialization protects lock creation, reclaim and
release. A proven dead incarnation can be reclaimed; a live incarnation,
unknown identity or unreadable/missing identity stays refused. Age or a bare
PID cannot authorize reclaim. Inspect an unknown stale lock explicitly rather
than treating it as permission to remove another process's work.

`current` and `previous` are atomically replaced relative symlinks into the
versions directory. The `ownership` marker is `managed` or `external`.
Managed control also requires the recorded unit digest, expected launcher,
selected catalog and systemd fragment provenance. A marker alone grants no
control. The public descriptor advertises `updateCapability: service` only
when its selected runtime executable and installed unit provenance verify;
an unverified managed-launch hint reports `none`. Ordinary foreground startup
keeps `foreground`. No host inventory or private paths enter the descriptor.

## Update and switch back

Updates are manual. Transfer the second exact archive and checksums, then:

```sh
tether service update 0.8.1 \
  --archive "$HOME/releases/tether_0.8.1_linux_amd64.tar.gz" \
  --checksums "$HOME/releases/checksums.txt"
tether service switch-back
# Or select any complete retained exact version:
tether service switch-back 0.8.0
tether service restart
```

Checksum failure preserves the active runtime. A failed restart retains the
new selection and the previous complete runtime for manual switch-back; there
is no automatic trial/commit or database rollback. Restart uses the existing
daemon shutdown and shim reconnect path. The service does not stop independent
shim units. Live shim compatibility still requires the existing custody and
wire negotiation; selecting an older binary does not override retained
obligations or force unsupported provider state to recover.

Remote clients must speak the server's protocol integer. Fetch
`/.well-known/tether/environment`; a protected request with a missing or
mismatched protocol returns `protocol_mismatch`, `required_protocol` and an
update hint. Change the client or environment to the same protocol. SemVer
alone is not a compatibility promise. Pairing remains separate work.

## Resuming a session on a worker

A worker-placed session can be resumed on the same worker, with the provider's own
conversation, instead of starting fresh. Two halves have to agree:

- **Hub (opt-in, off by default).** `Service.WorkerResume` (#350) allows resume of a worker-placed source.
  It fails closed unless that worker is connected and advertises `worker-resume`, the source is a claude
  session, the request is not native-only, and a provider session id is recorded. The hub then sends the
  new launch with `LaunchSpec.resume_session_id`. Nothing in this repo sets `WorkerResume` from
  configuration yet, so it stays off.
- **Worker (this version and later).** The worker advertises `worker-resume` in its hello and honors
  `resume_session_id`: it finds the claude transcript `<claude config dir>/projects/*/<id>.jsonl`
  (`CLAUDE_CONFIG_DIR`, else `~/.claude`), reads the directory the session ran in, and starts
  `claude --resume <id>` there. Any other resume or continue flag in the hub's args is dropped, so the
  conversation is exactly the one the hub approved.

The worker refuses, with `launch_failed` carrying `resume_unavailable: <reason>` and no fresh start, when the
id is malformed, the provider is not claude, the transcript is not on this worker, the recorded directory is
outside the worker's `workspaces/` tree, or that workspace no longer exists. A refused resume creates no
workspace. The hub decides what to do next; it must not fall back to another worker.

Required worker version: the release that includes CW-20261014-0113. Older workers do not advertise the
feature, so the hub keeps refusing resume on them. It takes effect only after a worker swap
(see [Update and switch back](#update-and-switch-back)).

## Status and removal

```sh
journalctl --user -u tether-worker.service
systemctl --user status tether-worker.service
tether service uninstall
```

Status explains missing/changed provenance, incomplete runtime,
`external-install`, `user-manager-unavailable`, `linger-unavailable`,
`linger-disabled`, `service-disabled`, `service-stopped` and `service-failed`.
Uninstall disables and stops **only** a verified managed worker unit, then
removes that unit and its service ownership record. It retains runtime versions,
the catalog, state, provider credentials and shim custody.

Source fixtures cover checksummed local archives, refusal cases, independent
ownership, manual update/switch-back and separate disposable process custody.
They do not prove a clean Linux account survives logout or that a real
systemd-hosted provider survives restart. Those operator acceptance checks
require a separately authorized owned account/window. No live agent-os unit,
daemon, catalog, credentials or linger settings were changed by this source
implementation.

## Reference capture on worker sessions

The hub sends the resolved `extract_refs` policy in `LaunchSpec`; it is off by
default. Enabling it requires a worker advertising `extract-refs` and a session
credential. The worker currently plants this relay for Claude only; other
providers reject an enabled policy. Otherwise the hub refuses the launch instead of silently dropping
capture. Older workers still accept default-off launches.

The credential relay plants the canonical `--session` and an explicit
`--extract-refs=true|false` after the imported template's flags. The hub records
`ref_attribution` after a capable worker acknowledges launch; old workers retain
unknown attribution. This records capture capability, not evidence that a call
happened or that a stored source is authenticated. Calls through separate direct
MCP entries still bypass the Tether gateway.

Changing the catalog does not reconfigure already planted sessions. Enablement
also depends on authenticated proxy provenance (CW-20261016-0157); propagation
alone does not supply an issuer-minted proxy credential. Keep extraction off
until that path is ready. No existing settings are changed by this support.
