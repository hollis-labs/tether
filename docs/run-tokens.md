# Launcher execution tokens and proxy_events contract

CW-20261120-0004 implements design rev4 sections 5 and 6.2. Torque comment
27481 posted the field contract before implementation; amendment 27552 preserves
the Torque identifiers introduced by PR #631. Contract review: 27483 and 27553.

## Two run identifiers

`attribution.run_id`, `sessions.run_id`, event run correlation and
`TETHER_RUN_ID` retain their existing meaning: a launcher-asserted Torque
`runs.id`. Unknown remains unknown, including an absent environment variable.

Every local/worker launch additionally mints a fresh execution UUID into
`TETHER_EXECUTION_RUN_ID`. A signed `TETHER_RUN_TOKEN` is minted only when the
execution host supports nonpersistent handoff. Local support is probed; workers
advertise `execution-run-token-v1` only when supported. Older/non-Linux workers
and unsupported local hosts retain legacy authenticated operation without a
verified execution. Reserved provider/ambient token values are removed before
launch; no token is sent to an incapable worker.
The signed claim `run_id` means that execution UUID; it is never substituted for
the Torque field. `run_verified` validates the signed execution context, not the
Torque identifier, identity flag, or durable writer provenance.

## Wire and verification

`X-Run-Token` is separate from the Authorization bearer. The compact JWS header
is exactly `{"alg":"EdDSA","typ":"tether-run+jwt"}`; claims are
`{urn, run_id, session, task, workstream, parent, exp, jti, iss, aud}`.
Issuer is `tether-launcher`, audience `tether-mcp-run`; expiry uses the normal
session-token TTL. A shorter bearer lifetime still applies independently.
Optional task/workstream/parent stay empty when unknown. URN is the session's
logical agent binding target, or its session principal address without an agent.
Parent refers only to a parent execution UUID from verified launching context.

A single nonempty header of at most 4096 bytes is required when supplied. Missing
headers preserve legacy admission, with execution fields absent. Malformed,
forged, expired, wrong-type/issuer/audience and wrong-subject/session/URN tokens
fail admission. The subject must be `msg://session/local/<session>`, authenticated
as a session credential or bound agent JWT. Signed task/workstream must exactly
match the current row; reassignment invalidates retained signed context instead
of overwriting server-resolved fields. Snapshot resolution also rechecks those
values so reassignment between admission and observation cannot stamp stale
execution attribution.

The run token is pinned to its MCP view, including jti. Adding, changing or
removing it requires a fresh MCP session. Every HTTP request, method admission
and monitor rechecks it. Expiry cancels retained calls/streams; existing bearer
and session revocation still apply. Jti is correlation, not single-use replay
prevention. Verification has no Registry/Tesseract calls.

## Enforced signing-key custody

The key never enters SQLite or DB/WAL/SHM sidecars. It lives separately at
`<state-db-path>.run-keys/signing.key`: a checked owned 0700 directory containing
an owned regular 0600, single-link, 64-byte Ed25519 key file. Every use walks and
checks opened directory/file inodes with no-follow protection. Unsafe shared
parents outside a private ownership boundary, symlinks, special files, hardlinks,
wrong owners, permissions or malformed material are refused. Existing permissions
are never repaired implicitly. Creation is exclusive and synced; a missing key
in retained custody fails closed rather than silently rotating it.

Backups must retain the private key directory alongside the database; a SQLite-only
backup does not contain it. No key cache bypasses later custody checks. Restart
reads the same private file.
Explicit key replacement invalidates outstanding signatures on re-admission and
monitoring; there is no automatic rotation or token refresh. No live key or
permission changes are performed by this task.

## Runtime-only token handoff

The local shim placer and worker executor strip the raw token before writing
`launch.json`. They put it into a sealed Linux memfd: anonymous kernel memory,
not a filesystem credential/config file. Descriptors contain only a numeric
`/proc/<launcher-pid>/fd/<fd>` reference and a fresh binding id. The shim entrypoint
accepts only owned 0600, zero-link, sealed anonymous memory, checks the binding id
and session, then restores the token into the provider's in-memory environment.
The launcher retains the memfd until the shim is ready, then closes it. If submission may have succeeded but the caller cancels or readiness times out, an independent observer retains it until authenticated readiness, verified process absence, or verified retirement. Caller cancellation alone never revokes a submitted runtime’s first read. A launcher process death before that first read closes the memfd and fails closed; recovery requires a fresh launch, rather than persisting a replayable token. Markers
are removed from the provider environment.

Inspection and controller recovery never restore tokens into persisted state.
Reattachment to an existing live shim preserves its runtime. Replaying a stale
handoff, recycled fd, different session or raw token-bearing descriptor fails
closed; a new launch is required. Current nonpersistent handoff support is Linux. Unsupported platforms do not
receive a launcher token and continue legacy operation. An explicitly supplied
token on an unsupported shim path is refused, never persisted or silently
downgraded. Gateway verification is independent of delivery capability: supplied
invalid/expired tokens are always rejected.

`tether mcp --forward-daemon` and `tether-worker mcp-http-relay` read the runtime
execution token and carry the distinct header on every POST/GET/DELETE, bearer
refresh and replacement-session initialization. Run-bearing clients refuse
redirects. The stdio relay supports agy without its unsupported static header
interpolation. Tests capture actual HTTP headers and actual shim/executor
handoffs; they do not claim a full authenticated agy model run.

Direct HTTP configs may use the spike-tested mappings: Claude
`headers.X-Run-Token="${TETHER_RUN_TOKEN}"`, Codex
`env_http_headers.X-Run-Token="TETHER_RUN_TOKEN"`, OpenCode 1.19.6
`headers.X-Run-Token="{env:TETHER_RUN_TOKEN}"`. Pin the provider/config versions.
Legacy in-process proxy/SSH paths do not establish verified execution context.

## Completed-call row contract

Existing proxy_events writes/readers remain: one completed call row, no start row.
Keep id, daemon UTC timestamp (RFC3339Nano), session_id, server, tool_name,
args_schema_fp, duration_ms, ok, redacted error, telemetry_json, attribution_json
and claimed_session_id. Existing identity/session flags and server-resolved
principal, agent, session, Torque run, task, workstream, launch, project and lineage
retain their meanings. Claimed session and client label remain untrusted hints.

| Additive attribution field | Source |
| --- | --- |
| run_verified | successfully verified execution context; omitted means false |
| execution_run_id | validated launcher execution UUID (`token.run_id`) |
| token_jti | validated token UUID |
| run_parent | validated optional parent execution UUID |

Never store/log raw execution tokens, Authorization, signing material or
token-bearing environment/config as telemetry. A verified execution does not
establish durable Tesseract writer provenance: status/handoff/friction remain
claimed until server-enforced writer_principal binding exists. Registry
registration and identity-service changes are separate work.

## Standalone shared gateway cutover gap

Tether's in-tree gateway and protected Unix `/gateway/mcp/{remote/,}admit` and
native endpoints support the header. Admission responses add optional `run`
validated claims (schema version 1, additive). Native requests preserve the
header; fingerprints bind the execution token.

Standalone hollis-labs/mcp-gateway support is **not implemented or deployed** by
this PR. Its authority/frontend must forward the header on admission, native
requests, per-method admission and monitoring; pin it to views; handle execution
expiry independently of bearer refresh; and propagate `execution_run_id` without
reinterpreting Torque `run_id` or trusting caller metadata. Dropping the optional
header yields legacy operation with no verified execution. Before cutover, test
forgery/expiry/binding, changed/removed tokens, stream expiry, header propagation,
both run identifiers and old-table write parity. No shared-gateway coverage is
claimed until those tests pass. This unit performs no deployment.
