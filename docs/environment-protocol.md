# Environment identity, protocol and role profiles

The daemon owns one UUIDv4 identity per selected state directory. Startup uses
the directory of the opened state database, rather than a separate default
directory. Private `environment-id` and `environment-id.recovery` files retain
the first published identity across restarts and interrupted initialization.
Keep both with state backups. Restoring this state restores its identity;
cloning it also clones the identity. Corrupt or conflicting identity files fail
startup instead of silently generating a replacement.

Optional settings in `global.yaml`:

```yaml
environment:
  label: worker-1
  authority: worker-1
role: worker
modules:
  teams: false
  llm_gateway: false
```

The label defaults to the hostname. An authority name contains 1–32 lowercase
letters, digits or hyphens. Once configured it is pinned to the local UUID in
private state files; a different name requires an explicit future rename or
enrollment operation. This binding performs no hub enrollment or directory
collision check.

## Public descriptor

`GET /.well-known/tether/environment` is public in every identity mode. It is
cached at startup, supports `HEAD` and conditional `If-None-Match` reads, and
contains only identity, label, platform, version and capability metadata:

```json
{
  "environmentId": "4d1cb455-d82c-4c70-b622-144ad62a9583",
  "label": "worker-1",
  "platform": {"os": "linux", "arch": "amd64"},
  "serverVersion": "dev",
  "protocol": 1,
  "capabilities": {},
  "updateCapability": "foreground"
}
```

An absent capability group means unsupported. This foundation advertises no
versioned remote groups; later API compositions must add only groups they
actually implement. It exposes no catalog paths, provider configuration or
credentials. `updateCapability: foreground` describes the current daemon
composition: managed service updates are a later lifecycle slice. `/health`
also reports `environmentId`, `serverVersion` and `protocol` and remains public.

## Protocol agreement

The protocol integer changes for breaking remote API changes, independently of
the application release version. Clients decode unknown JSON fields, capability
groups and string variants additively. Established field names and types have a
protocol-1 conformance baseline.

Send `Tether-Protocol: 1`, or `?protocol=1` when headers are unavailable. A
supplied value must match exactly; duplicate values or disagreeing header/query
values fail with HTTP 409:

```json
{
  "error": {
    "code": "protocol_mismatch",
    "message": "client and environment protocols must match",
    "required_protocol": 1,
    "update_hint": "Fetch /.well-known/tether/environment and update the client or environment to the same protocol."
  }
}
```

Existing local API callers may omit the value. New versioned stream routes
require it when installed; future remote listeners must require it for their
protected routes. Health and the descriptor remain bootstrap-readable even
with a mismatched value. Protocol agreement grants no authentication or scopes.

## Worker and hub modules

Omitting `role` preserves the existing combined composition: the old
`teams.enabled` and AI provider opt-ins still decide those modules. An explicit
`role: worker` enables worker features and defaults teams, the LLM gateway,
environment directory and connection manager off. `role: hub` includes worker
features and selects those hub modules. A worker skips LLM construction before
model discovery or helper credential resolution, even if legacy AI providers
remain configured.

The `modules` map overrides individual choices. Valid names are `session_core`,
`local_mcp`, `messaging`, `credential_broker`, `remote_listener`, `stream_api`,
`lifecycle`, `environment_directory`, `connection_manager`, `teams` and
`llm_gateway`. Unknown names or roles fail validation. `tether doctor` reports
the role and each module; configured future directory and connection manager
modules explicitly report that their implementation is not installed.

Module switches select new exposed API requests. Disabled routes return 404
after caller authentication. Disabling lifecycle prevents session mutations and
boot recovery scheduling; disabling session core also prevents that scheduling.
Already accepted durable deliveries retain their existing recovery and drain
semantics. These switches do not move or revoke authority over existing sessions.
The credential broker switch controls its API surface, not the internal safety
checks that protect launches.

The local MCP switch controls the daemon gateway and locally composed stdio
adapter. Enabling it retains the separate `daemon.mcp_endpoint.enabled` opt-in;
it does not expose the endpoint automatically. A forwarded adapter inherits the
destination daemon's module policy. Disabling `remote_listener` refuses a TCP
listener; enabling it does not install the later per-listener remote policy,
device authentication or pairing. Stream, directory and connection manager
switches do not create absent implementations.

## Enrollment authority metadata

The native enrollment kernel stores and shows an `authority` value of
`environment` (default) or `hub` on its actor record and owner-scoped directory
projection. Retirement preserves it. Legacy records with no value display as
`environment`; no bulk migration runs. The value is metadata only: authorization,
publication, binding leases and launch admission retain their existing checks.
There is no new production enrollment route, global fencing or migration.
