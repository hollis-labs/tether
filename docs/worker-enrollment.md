# SSH worker enrollment

`tether env add` enrolls a dedicated Linux worker account using an existing SSH
host alias or `user@host`. Obtain an exact release containing this command and
the worker service, together with its release `checksums.txt`, on the hub. The
worker needs no GitHub access or private module credentials.

```sh
tether env add worker-alias --authority worker-one --version 0.8.0 \
  --archive /hub/releases/tether_0.8.0_linux_amd64.tar.gz \
  --checksums /hub/releases/checksums.txt --provider codex \
  --scope read,operate
```

The version illustrates the archive layout; select an actual release containing
the enrollment helper. Use the archive matching the worker's architecture, even
when the hub has a different architecture. Only Linux amd64 and arm64 workers
are supported. Provider CLIs and their login credentials must already be
prepared on the worker. Enrollment never copies provider credentials or performs
provider login. Device scopes are independent; the default is `read` alone.
Choose `operate`, `terminal`, `maintain` or `admin` explicitly when needed.
Enrollment requires `read` to verify the resulting device with a protected read.

Preflight runs in a non-interactive login shell. It checks the OS and architecture,
git, usable bubblewrap, the systemd user manager, linger, the selected provider
CLIs on PATH, and writable state under the dedicated user's home. An existing
catalog, daemon receipt, ownership marker or worker unit is refused when there
is no matching enrollment operation. A failing preflight stops before worker
installation or hub receipt creation. Missing linger requires a separate
administrator action, for example `sudo loginctl enable-linger worker`; enrollment
never runs sudo or changes account settings.

SSH uses BatchMode, ExitOnForwardFailure, ServerAliveInterval/CountMax and a
connection deadline. ControlMaster, ControlPath and ControlPersist are disabled.
There are no password prompts, supplied SSH command fragments, nested SSH hops or
automatic host-key bypasses. Prepare host keys and SSH authentication separately.
`--timeout` bounds the whole operation (10m default, at most 30m); individual
commands, uploads, forward establishment and HTTP requests have shorter bounds.

The hub verifies and snapshots the archive against its exact checksums entry,
then pushes that snapshot, matching checksums and the archive's bootstrap binary.
It rejects archive escapes, duplicate paths, links, special files, oversized
members and incomplete gzip trailers before changing the worker. The worker
uses the existing staged, checksummed immutable runtime installer and
[managed user service](worker-service.md), with its unit/runtime/catalog/PID
provenance checks. It initializes only a new owned catalog, configures an
enforced loopback listener and the local authority, and selects independently
hosted systemd shim units for agent survival across daemon restarts. Existing
external supervisors and unrelated units are never adopted or stopped.

An invocation-owned SSH local forward reaches the public environment descriptor.
Its UUID must match the worker's identity proved through the private SSH helper;
the protocol, exact release and platform must match before pairing. Requests
retain the configured worker Host authority while using the local forward.
The forward is closed and its process joined when the command returns.

The worker creates a short-lived one-time grant through its existing local Unix
socket and operator credential. Its code returns only through a bounded private
capture pipe. The hub exchanges it over the verified forward, persists the
returned device token in a current-user-owned regular 0600 file and verifies a
protected read. No grant code or token enters command arguments, SSH diagnostics,
public output, catalog YAML or the enrollment receipt. There is no operator-token
fallback, public grant issuance, implicit admin grant or device-to-child scope
translation.

The hub retains a private receipt at
`~/.tether/environments/<authority>/receipt.json`; `--receipt-dir` selects an
explicit absolute 0700 directory. It records the operation UUID, SSH target,
worker UUID, authority, release digest, remote port, scopes, phase, grant/device
identifiers and a `file:///absolute/path` credential reference. The token is in
the adjacent `device.token`, separately from receipt metadata. The preflight
inventory is retained privately for later capability reporting. Public command
output reports the completed or partial operation and contains no token or code.

Retry the same command with the same receipt and release bytes. Completed
enrollment verifies the original worker and device without reinstalling or
issuing another grant. A changed target, authority, release, port or scope set
refuses reuse of that receipt. Known partial installation can resume the exact
operation; unknown or changed catalog/service provenance is retained for local
inspection instead of overwritten.

The receipt is persisted before the pairing POST. If the response is lost or
cannot be persisted, it reports an uncertain exchange and refuses automatic
replay or replacement-device minting. Inspect devices through the worker's local
operator and explicitly reconcile that receipt. A successful retry does not
invent evidence that the previous exchange failed. A revoked device likewise
fails verification rather than silently replacing its credential.

```sh
tether env rollback worker-alias \
  --receipt-dir "$HOME/.tether/environments/worker-one"
```

Rollback controls only the exact verified managed worker unit. A known paired
device is revoked through the worker's local operator before stopping the unit.
Unreachable or refused revocation stops rollback and retains evidence. An
uncertain exchange with no known device ID remains an operator reconciliation
obligation. Runtimes, catalog, state, shim custody, private credentials and
receipts remain available; rollback does not erase data or claim unknown
credentials have been revoked.

Directory registration is separate work. The enrollment forward's temporary URL
is not a durable route. A directory consumer needs explicit durable routes and
the completed receipt. Deployment ownership (`managed` versus `external`) is
separate from an agent's INDEPENDENT or HUB-MANAGED mode and never changes its
home UUID, URN or authority. Enrollment performs no primary-owner hub enrollment.

Source tests use disposable stores, real daemon HTTP pairing handlers, synthetic
private credentials and disposable command/service stubs. They do not establish
clean-box SSH installation, logout survival, provider login or live-worker
acceptance; those remain separately authorized operational checks. Native HTTP409,
empty ordinary child scopes, current child expiry/revocation limitations, A2A's
inner binding refusal, caller-asserted message addresses, single-owner read access
and same-uid file/database access limitations remain unchanged.
