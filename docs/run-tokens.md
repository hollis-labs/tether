# Launcher run correlation and proxy_events contract

CW-20261120-0004 implements design rev4 sections 5 and 6.2. The field
contract was posted before implementation as Torque comment 27481.

## Wire and verification

Each local or worker launch mints a fresh execution UUID (`TETHER_RUN_ID`)
and signed compact JWS (`TETHER_RUN_TOKEN`) into the runtime environment.
The token is separate from the Authorization bearer. Its protected header is
exactly `{"alg":"EdDSA","typ":"tether-run+jwt"}`; claims are
`{urn, run_id, session, task, workstream, parent, exp, jti, iss, aud}`.
Issuer is `tether-launcher`, audience `tether-mcp-run`, and expiry uses the normal
session-token TTL. A bearer with a shorter ceiling still expires independently;
a run token never extends its authority. Optional task, workstream and parent may be empty.
URN is the session row's logical agent binding target, or its session principal
address when it has no logical agent. Parent is inherited only from a verified
run in the launching request context; absence remains empty.

The daemon lazily creates an Ed25519 signing key in the credential-protected
state DB's singleton `run_signing_key` table and caches it in memory. Tokens are
never stored in that table or launch plans. The same key survives restart;
verification needs only local material, the authenticated principal and the
existing server session row, with no Registry/Tesseract calls. Key replacement
invalidates outstanding tokens and must be coordinated with session restart;
there is no automatic key rotation or run-token refresh in this version.

`X-Run-Token` must be a single nonempty header of at most 4096 bytes. Missing
headers retain legacy admission with no verified run. Supplied invalid,
forged, expired, wrong-type/issuer/audience, wrong-session/URN or stale
task/workstream tokens fail admission. Session terminal/revoked credentials
continue to fail independently. A session credential or bound agent JWT is
required, with authenticated subject `msg://session/local/<session>`;
operator/service credentials cannot acquire run attribution.

A run token is pinned to the MCP view, including its jti. Changing, adding or
removing it requires a new MCP session. HTTP requests, method admission and
monitoring recheck it; expiry cancels retained calls/streams. Jti correlates
calls and is not a single-use replay defense. Task/workstream must exactly match
the current row; reassignment requires a fresh launch/token rather than replacing
server-resolved attribution with old signed values.

## Transport

`tether mcp --forward-daemon` reads the run token from its inherited environment
and sends it on every SDK request. `tether-worker mcp-http-relay` does the same on
POST, GET, DELETE, bearer refresh, and replacement session initialization. It
never adds the token to argv or planted MCP config and never follows redirects.
This stdio relay supports agy without relying on its unsupported static header
environment interpolation. The relay test checks real HTTP headers on all three
methods; it is not evidence of a full authenticated agy model/task run.

Direct HTTP client static configs may use the spike-tested mappings:
Claude `headers.X-Run-Token="${TETHER_RUN_TOKEN}"`, Codex
`env_http_headers.X-Run-Token="TETHER_RUN_TOKEN"`, and OpenCode 1.19.6
`headers.X-Run-Token="{env:TETHER_RUN_TOKEN}"`. Pin provider versions/config
shapes to the spike. Legacy local in-process proxy/SSH paths do not traverse the
verifying gateway and therefore do not establish verified run attribution.

## Completed-call row contract

The existing `proxy_events` table keeps its writes and readers; no table is
retired. One completed call creates one row; starts do not. Keep id,
daemon UTC timestamp (RFC3339Nano), session_id, server, tool_name,
args_schema_fp, duration_ms, ok, redacted error, telemetry_json,
attribution_json and claimed_session_id.

`attribution.verified` still means authenticated principal resolution;
`session_resolved` still means session-row resolution. Existing principal,
agent, session, task, workstream, launch, project and lineage fields remain
server-resolved. Claimed session and client label remain hints. Add only:

| attribution field | Source |
| --- | --- |
| run_verified | true only after bound run verification; omitted means false |
| run_id | validated launcher execution UUID |
| token_jti | validated token UUID |
| run_parent | validated optional parent run id |

Never emit raw run/bearer tokens, signing keys, token-bearing environment/config
or unverified decoded claims as telemetry. Existing error redaction remains.
A verified run does not establish durable Tesseract writer provenance: status,
handoff and friction remain claimed until server-enforced writer_principal
binding exists. Registry registration and identity-service changes are separate.

## Standalone shared gateway cutover gap

This change implements the Tether in-tree gateway and its protected Unix
`/gateway/mcp/{remote/,}admit` and native endpoints. Admission responses add an
optional `run` object with validated secret-free claims. Schema version remains
1 (additive field); its fingerprint includes the run-token hash. Native forwarding
preserves the separate X-Run-Token header and revalidates it.

**Standalone hollis-labs/mcp-gateway support is not implemented or deployed by
this PR.** Its authority client/frontend must carry X-Run-Token on admission,
native forwarding, per-method re-admission and monitoring, pin it with the view,
handle run expiry while retaining bearer refresh, and propagate the four
attribution fields without trusting client-supplied metadata. Because missing
tokens intentionally preserve legacy operation, a standalone gateway that drops
the header will produce no verified run, even though Authorization succeeds.
Before cutover, add integration tests for forged/expired tokens, wrong binding,
changed/removed tokens, stream expiry, header forwarding and old-table write
parity. Do not claim shared-gateway verified run coverage until those pass.
