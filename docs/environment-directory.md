# Environment directory

The optional `environment_directory` module stores a hub's explicit environment
bindings in its own daemon database. An explicit hub profile enables it; a worker
or legacy profile leaves it disabled unless configured. Directory visibility
and deployment ownership confer no worker permissions.

An operator registers a previously authorized expected UUIDv4, immutable messaging
authority, exact paired device ID, ordered durable HTTP(S) routes, and an explicit
private `file:///absolute/path` credential reference. The daemon uses the existing
current-user private-file checks. It never chooses a UUID from an anonymous
advertisement, discovers a token from ambient settings, mints a grant, or stores
credential bytes. Each route must first advertise the expected UUID and compatible
protocol before the credential resolver runs. A protected read then establishes
connectivity; the descriptor alone is not authenticated enrollment.

Registration, label changes and retirement require the verified operator over the
local Unix socket. A device with `admin`, a session, an anonymous local caller, or
an operator over TCP cannot mutate the directory. Remote reads require the
existing `read` scope. HTTP, CLI and MCP return redacted credential references.

With the daemon running, the CLI exposes:

```text
tether env list
tether env get UUID
tether env register < registration.json
tether env rename UUID LABEL
tether env remove UUID
```

The registration document has this shape (all identity/route/ref values below are
illustrative; obtain the actual expected values through the authorized enrollment
process):

```json
{
  "environmentId": "a6a2102f-e465-454a-9f4f-527a76be51ad",
  "authority": "worker-example",
  "label": "Example worker",
  "routes": [{"baseURL": "https://worker.example"}],
  "credentialReference": "file:///private/device.token",
  "deviceId": "exact-paired-device-id",
  "ownership": "external",
  "managementMode": "INDEPENDENT",
  "homes": [{
    "urn": "msg://agent/worker-example/example-agent",
    "authority": "environment",
    "managementMode": "INDEPENDENT"
  }]
}
```

Routes are explicit durable reachability declarations. An invocation-only SSH
forward URL must not become a durable route. This feature neither starts tunnels
nor infers an installer, transport, administrator or management grant.

`ownership` is `managed` or `external` deployment metadata. Separately, DEC102
management mode defaults to `INDEPENDENT`. Explicit per-agent and launch settings
have increasing precedence over the instance setting. Missing settings never
mean hub control. Home authority is `environment` by default or explicitly `hub`;
it is inert metadata in this MVP. Mode and home declarations are immutable after
registration. They do not change worker admission, identity, mailbox location or
grants, and cannot switch authority at runtime.

A UUID owns one unique authority; an agent URN has one durable home. Repeating
an identical binding can refresh observations. Rename changes only the display
label. Changing bindings, relocating homes and recycling retired identities or
authorities are refused. Static federation configuration must agree on the
primary route and credential reference, and may not claim a directory authority
as local. Directory routing reads committed records before static peer/local
fallback; a retirement tombstone always refuses delivery. Each actual remote
operation uses the identity-checked mesh client and is not replayed on another
route after an uncertain mutation. Already in-flight operations are not global
fencing or proof of stopped computation.

Records retain capabilities, protocol, server version, state and last successful
observation time across daemon restarts. A failed observation preserves lastSeen;
a failed observation cannot revive a retired record. The connection-manager
scheduler consumes the directory service's observation seam separately.

Removal first commits `state: retired` and `revocationPending: true`. Only a
successful explicitly authorized worker revoke can clear pending state. The
current composition has no supported remote revoke port and therefore preserves
pending state; it never broadens a read/operate device into an administrator.
Unavailable, non-admin or uncertain revocation is also pending. Repeating remove
or restarting does not replay an uncertain revoke. Retirement does not revoke
independent provider/platform authority or declare a worker dead.

The HTTP surface is `GET/POST /environments` and
`GET/PATCH/DELETE /environments/{UUID}`; PATCH accepts only `label`. The MCP tools
are `tether_environment_list`, `tether_environment_get`,
`tether_environment_register`, `tether_environment_rename` and
`tether_environment_remove`. Both CLI and MCP use the daemon client and refuse
an unavailable daemon rather than opening SQLite directly.

Primary-owner enrollment and real-worker acceptance remain separate operator
gates. Synthetic source tests establish no live deployment or enrollment claim.
