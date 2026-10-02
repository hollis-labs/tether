# Daemon MCP transport

CW-20261001-0539 is being delivered in stages. Stages 1–2 supply the shared
upstream runtime, credential resolver, profile-aware SDK views and daemon
Streamable HTTP endpoints. Planted/interactive stdio forwarders retain their
existing path until the later cutover. The approved design is Tesseract workspace item
`01M3X329W5ZSBWR46169XNJ68G`, key `mcp_daemon_transport_design`.

The daemon composition owns one lazily constructed `SharedUpstreams` instance. Each MCP view
has its own native dispatch session and profile/mode policy while sharing every
upstream connection. Closing a view closes its protocol/native sessions, never
the shared pool. The existing SDK profile middleware enforces the same eligible
inventory for protocol listing, semantic hydration/search, direct calls and
dispatch. A requested profile intersects the credential grant; it cannot grant
another upstream. The native adapter receives a scoped status source, so
`tether_health` and gateway status/refresh disclose only that view's origins.
Shared-pool errors scrub endpoint URLs, userinfo and query values before storage
or logging, including literal URLs rather than only secret references.

## Endpoints and admission

The endpoint defaults **off**. With `daemon.mcp_endpoint.enabled: true`,
`/mcp` and `/p/<profile>` are mounted alongside the existing daemon API on
its configured listener. Unix sockets carry the same HTTP protocol; clients use
`http://unix/mcp` with a UDS dialer. The synthetic `localhost` authority also
works over UDS. TCP clients use the configured concrete host and port. Host
validation accepts the configured authority and its loopback aliases; it never
trusts forwarded-host headers. An Origin, if present, must be a single, non-null
origin matching the request's scheme and authority. Foreign origins/hosts are
refused before dispatch, and the SDK's localhost protection remains enabled.

MCP requires a valid bearer even while the existing API remains in `observe`.
Every POST/GET/DELETE re-verifies the supplied credential in the identity store;
SDK method dispatch also verifies current authority. Missing/invalid/revoked
credentials receive 401; an unavailable verifier receives 503; identity `off`
disables this endpoint with 503. The endpoint never infers identity from flags,
client info or claimed session headers. Non-loopback TCP requires `enforce` and
a working verifier; the daemon checks both the configured address and actual
bound address, including localhost resolution.

Profiles may be selected by `/p/<id>`, query `profile` or `X-Tether-Profile`.
Discovery mode uses query `discovery_mode` or `X-Tether-Discovery-Mode`.
All supplied selectors must be valid; repeated selectors must agree. Empty,
unknown or conflicting values refuse initialization. Query credentials are
forbidden. Session defaults come from the launch snapshot, never daemon env.

An MCP session ID is SDK transport state, distinct from a Tether session ID.
It is bound to the verified principal, credential hash, scopes and resolved
policy/profile/mode fingerprint. Another credential, even for the same
principal, cannot use it. Profile, grant or mode changes require a new
initialization; requests cannot silently widen an existing view. Native API
clients carry the admitted credential and daemon-resolved caller context.

Only an admitted view's eligible upstream origins start, lazily and at most
once per origin. Each first handshake has its own existing ten-second timeout;
a slow/granted origin cannot block a zero-grant or differently granted view.
Native-only views spawn no upstreams. Typed naming collisions degrade/exclude
the conflicting origins and retain both owners in operator diagnostics;
unrelated origins continue serving and refresh can recover accepted inventory.
No permanent startup error is cached.

Views are bounded to 128 globally and 16 per exact principal (across that
principal's tokens). Sixteen global slots are reserved for the exact operator.
Non-operator idle views expire after two minutes, operator idle views after
15 minutes. A one-second monitor gives each view its own two-second verification
deadline. Definitive invalid/revoked/terminal/changed policy closes its streams
and cancels its in-flight calls. Temporary verifier/catalog failures keep the
view and log/retry; new calls remain refused until verification succeeds.
Catalog generations are cached and reparsed only on authority-layer metadata
changes; an inaccessible/broken changed generation never serves stale grants.
Revocation/cancellation cannot undo upstream effects: an in-flight call's
outcome may be unknown, and it is never replayed.

Shutdown stops admission and closes idle-view streams immediately. Streams
attached to active calls remain through response drain because SDK stream
closure otherwise fails pending POSTs. It drains admitted in-flight calls up to
`daemon.shutdown_timeout`, cancels any remaining calls, then closes the pool
and finally the store. A SIGTERM does not immediately kill an admitted call.
Invalid shared-runtime roots or credential references fail view construction
with a static 503 plus a private-safe daemon log; existing API routes remain
available. Verified MCP initialization omits the daemon runtime PID capability.

### Operator enable/disable

This endpoint is an optional phase-2 rollout for CW-20261001-0230, separate
from the live cutover. In the selected catalog's `global.yaml`, set:

```yaml
daemon:
  mcp_endpoint:
    enabled: true
```

Keep `identity.mode: observe` or `enforce` with provisioned credentials and a
Unix socket or concrete loopback host/port. Run `tether doctor --catalog <root>`
for the endpoint finding, then restart that selected daemon. Non-loopback needs
`enforce` and a working verifier. To disable, set `enabled: false` (or remove
`mcp_endpoint`), run doctor, and restart; no handler, pool or MCP routes are
constructed, so `/mcp` and `/p/` return the existing API's 404. Disabled daemons
retain existing listener behavior, including wildcard and ephemeral ports;
enabled endpoints refuse invalid/non-concrete authorities loudly at startup
and doctor. The seeded catalog documents the default-off switch.

`internal/client.Client.ConnectMCP` reuses its frozen credential and HTTP/UDS
dialer, with an SDK-owned stream lifetime. A missing socket, refused connection
or dial timeout wraps `client.ErrDaemonUnreachable`. Auth/policy/protocol errors
remain distinct. The wire error string `daemon_unreachable` belongs to the
0230/stage-3 stdio forwarding surface; this connector supplies the Go sentinel.
There is no local catalog/SQLite/upstream fallback, daemon
auto-start or retry of an uncertain mutation.

## Inventory changes and reconnects

Accepted eligible inventory changes fan out to the view's SDK sessions. Flat
views publish eligible tool additions, removals and declaration updates. Search
views retain their four infrastructure tools and signal the changed semantic
inventory with `notifications/tools/list_changed`; clients then hydrate/search
again. Hidden origins and changes confined to profile-denied tools do not
signal. A view observes a complete installed generation when it relists.
Notifications are hints, not a durable replay log.

Client disconnect/reconnect creates a new MCP session while reusing the same
shared upstream process. After a daemon restart an old MCP session ID returns
404. A client must explicitly initialize with its credential/selectors again
and relist before continuing. The endpoint never replays an uncertain tool
call or reconstructs a view from a caller's stale session ID.

For an active tool call, the daemon replaces the caller's progress token with a
random per-call upstream token, then restores the original token only on that
call's initiating SDK session. Identical tokens on other views cannot capture
the progress. Origin binding prevents another upstream from sending on that
route; completion/cancellation removes it. Upstream progress metadata is not
trusted caller context, and catalog credentials are scrubbed from messages.
Notifications arriving after the call has completed are discarded.

## Principal grants

Service and interactive principals are provisioned by exact verified principal
ID in the operator's `global.yaml`:

```yaml
identity:
  mode: observe
  mcp_grants:
    "svc:hadron":
      servers: [torque, tesseract]
    "msg://agent/local/interactive":
      servers: [loom]
```

These entries do not mint a credential or change its scopes. Missing mappings,
omitted/null `servers`, and `servers: []` grant zero upstreams. There is no
wildcard/all alias. An operator principal explicitly has the enabled inventory;
other principals do not become operators by carrying broad scopes.

The existing catalog grant validator checks every mapping at `NewDaemon`
startup and in doctor, naming the principal and unknown/disabled upstream.
Shared loaders still permit inspection/repair. A view checks only its selected
principal grant, so an unrelated broken entry does not block that view.
Validation reads only upstream IDs and enablement, never credential values.

For session principals the verified session ID selects an immutable policy
snapshot in `session_mcp_policy`. The daemon captures the effective launch plan
after provider/boot overrides, before minting/delivering the session credential.
Only resolved upstream IDs, profile/mode defaults, session/agent IDs and a
consistency digest are stored; no environment, bearer or upstream credentials.
The captured launch profile includes immutable rules: the effective view is
credential grants ∩ launch profile ∩ requested profile. A broader request or
later profile edit cannot remove launch-time read-only, deny or origin limits.
Explicit-empty upstream grants remain empty. The digest detects inconsistent
state; it is not an authentication token. The snapshot must match the actual
session row and remain active. A preparation retry can repeat the same snapshot
but cannot widen it. Empty/unknown profile or invalid discovery-mode capture
returns typed `ErrInvalidSessionMCPPolicy` and marks the session failed before
runtime start or credential delivery; it does not leave a stuck created session.
Legacy sessions without a snapshot must resume/relaunch to
use the new endpoint. Token verification remains separately required.

## Upstream execution authority

Claude/OpenCode upstream children today inherit their agent's sandbox. A
daemon-owned pool removes that per-agent execution boundary: each upstream now
runs with the daemon's authority for every caller. Every daemon-launched stdio
upstream therefore runs inside mandatory go-sandbox protect-only confinement,
with canonical catalog/run/state roots read-only. Missing roots or an unavailable
sandbox fail closed; there is no unconfined fallback or environment kill switch
for this new pool. Child environments inherit only PATH/HOME, locale settings
and TMPDIR, plus the entry's explicit environment and resolved credentials;
unrelated daemon environment and credentials are not inherited. Host reads,
network access, socket connections and writes
elsewhere remain possible. Per-app Torque allowed-root policy (0464) and Loom
export-root policy (0465) are load-bearing, not replaced by this confinement.

Protection also covers every catalog layer read by `LoadLayered`: the user
`~/.tether` layer, each registered repository's `.tether` layer, and catalog
directories configured outside the primary root. Loading and protection share
the layer enumeration. Empty roots are prepared as mount anchors before launch
so children cannot create a new layer; an unavailable parent fails closed.
The same directory policy now protects planted Codex proxies and Claude/other
wrapped agents. Their writable user/project layers were a pre-existing gap in
#97 and the earlier agent protection. Workspaces must be outside protected
layer roots; protection does not make an entire repository or HOME read-only.

A daemon view's native API client must verify as the exact admitted principal,
session and scopes. An operator-credential client is refused for a restricted
view, including credentials frozen at construction before environment changes.

HTTP/SSE upstreams cannot inherit local confinement. Their exclusion is visible
with `cannot be confined locally`; an operator can deliberately enable an entry
with `allow_unconfined_remote: true`. This retains the #97 policy.

Read-only protection does not prevent token reads. Same-uid agents can read
`operator.token` until CW-20260930-0237 protects it, so endpoint authentication
is only as strong as that file's protection. Every caller of one upstream shares
its catalog credential; upstream attribution collapses to that credential,
unchanged from today's proxies. Trusted forwarded caller attribution is later
work, with explicit service authentication and per-call context.

## Remaining stages

Stage 3 completes refresh notification, shutdown/reconnect and stdio forwarding
coverage. Production forwarder migration belongs to 0230/0254/0540. During
coexistence each legacy proxy still has its existing pool; portfolio-wide
serve-once is reached only at that cutover. No daemon restart or deployment is
part of this change.
