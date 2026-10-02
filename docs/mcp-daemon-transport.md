# Daemon MCP transport

CW-20261001-0539 is being delivered in stages. Stage 1 supplies the shared
upstream runtime, credential resolver and profile-aware SDK view construction.
It does not mount an HTTP endpoint yet or change planted/interactive stdio
forwarders. The approved design is Tesseract workspace item
`01M3X329W5ZSBWR46169XNJ68G`, key `mcp_daemon_transport_design`.

The daemon composition will own one `SharedUpstreams` instance. Each MCP view
has its own native dispatch session and profile/mode policy while sharing every
upstream connection. Closing a view closes its protocol/native sessions, never
the shared pool. The existing SDK profile middleware enforces the same eligible
inventory for protocol listing, semantic hydration/search, direct calls and
dispatch. A requested profile intersects the credential grant; it cannot grant
another upstream. The native adapter receives a scoped status source, so
`tether_health` and gateway status/refresh disclose only that view's origins.
Shared-pool errors scrub endpoint URLs, userinfo and query values before storage
or logging, including literal URLs rather than only secret references.

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

Stage 2 mounts `/mcp` and `/p/<profile>` over Streamable HTTP on the existing
HTTP/UDS handler tree, with verified admission even while the existing API stays
in observe mode. It binds transport session IDs to credential/view policy,
checks loopback/Origin/Host rules, handles revocation and typed daemon-down
errors, and proves identical HTTP/UDS surfaces against a disposable real daemon.

Stage 3 completes refresh notification, shutdown/reconnect and stdio forwarding
coverage. Production forwarder migration belongs to 0230/0254/0540. During
coexistence each legacy proxy still has its existing pool; portfolio-wide
serve-once is reached only at that cutover. No daemon restart or deployment is
part of this change.
