# Daemon-owned MCP upstreams for launched sessions

`global.yaml` controls planting for **new launches and resumes**:

```yaml
daemon:
  mcp_endpoint:
    enabled: false
  mcp_upstream_ownership: legacy_proxy
identity:
  mode: observe
```

Both defaults preserve the legacy per-session proxy pools. Enabling the endpoint
alone does not change planting. `daemon.mcp_upstream_ownership: daemon` selects
an explicit `tether mcp --forward-daemon --daemon-address unix:/absolute/socket`
plant. The credential is the launch-minted session token in `TETHER_TOKEN`, never
argv. The thin route never reads the catalog, opens SQLite, spawns upstreams,
auto-starts the daemon or falls back to a local pool. It never looks up
`operator.token`. `--session` is correlation, not authority.

The daemon enforces the credential's captured upstream grant and profile floor.
The stdio relay forwards admitted tool definitions and calls, application metadata,
results and inventory notifications; cancellation follows the SDK call context.
The relay preserves progress notifications received from its daemon connection.
Upstream-to-daemon progress routing is not yet provided by this change (nor by
legacy proxy calls); it belongs to 0539 stage 3. Protocol negotiation metadata
belongs to each hop.

Idle daemon views expire after two minutes for session principals. The forwarder
reinitializes using the same credential, profile and discovery selector when the
daemon/SDK explicitly reports expiry for a known session (a generic route 404
surfaces for that operation; a later operation may initialize a fresh view).
Lists and pings can also reconnect a closed/lost
transport once; protocol/admission errors and cancellation surface. These
read-only operations can be retried once on the replacement view. Each tool
dispatch first pings the view to recover idle expiry,
then sends the tool call exactly once. Tool calls are never replayed, including
missing-session errors during execution: a background stream failure can end
other in-flight calls whose outcomes are unknown. Network failures, lost responses
and canceled calls are never replayed. Revoked or expired
credentials still fail admission; recovery cannot fall back to a local pool.
The SDK may resume interrupted SSE streams using GET and
`Last-Event-ID`; this retrieves responses/notifications and never resends a
`tools/call` POST. The retry bound applies to consecutive resumes without
progress, so one call may have multiple resume GETs. A replacement view accepts
new calls while calls admitted on the previous view finish; SDK teardown runs
outside the initialization lock.
Concurrent callers share one initialization attempt, including daemon failures
and the setup timeout. If its initiating caller cancels or reaches its own
deadline, live waiters join a new attempt. A later caller can also try again
after a failed attempt finishes.
Other refresh/reconnect lifecycle work belongs to 0539 stage 3 (paused).
The daemon does not configure a resumable EventStore today. With an EventStore,
a daemon restart can return 404 on a resume GET, which currently surfaces as
`daemon_mcp_unavailable` (-32002), rather than `daemon_unreachable` (-32001).
The uncertain tool call is still never replayed.

Every daemon-owned launch first initializes the endpoint using its newly minted
credential with a 15-second initialization bound, allowing cold upstream startup.
Timeout and cancellation have distinct messages from credential rejection.
A disabled/down endpoint, missing session credential or admission
failure fails that launch with a hint to enable the endpoint and verified identity,
or deliberately select `legacy_proxy`. Failed preparation revokes its token.
Observe mode remains observe; its anonymous launch degradation cannot grant access
to the strict MCP endpoint. Identity `off` therefore cannot serve daemon ownership.

The selector is validated at daemon startup and by doctor, without changing shared
catalog loading for stop/status/other clients. It is also re-read and checked at
each launch: an unknown, explicitly empty/null/blank or non-string value fails
that launch rather than using another mode. Only an omitted selector defaults to
`legacy_proxy`. An unrecognized spelling of the YAML key is treated as an
omitted selector and retains legacy ownership; strict key validation is separate
work. Doctor fails if daemon ownership selects a disabled endpoint.
Changing the selector does not require restarting a running daemon; changing its
endpoint enablement or listen address does. The selected ownership and opt-in
reference extraction are captured with the session's immutable MCP policy.
Older policies remain readable. Extraction is documentary correlation, not proof
of ownership (0100).

Claude consumes the boot `.mcp.json`; OpenCode consumes `opencode.json` under
`OPENCODE_CONFIG_DIR`; Codex consumes `CODEX_HOME/config.toml`. Mirrors alone are
insufficient. Credential-bearing files remain 0600. The thin proxy retains
Codex's protect-only wrapper, and Claude/OpenCode retain their outer protection.
Daemon-owned stdio children run under the daemon's mandatory protect-only policy.
The worker's provider launch environment excludes granted upstreams' catalog env
keys and referenced variables, plus secret-like names from ungranted entries.
Routine variables such as `PATH` in ungranted entries do not remove the worker's
own values; unrelated provider model authentication remains. If an
upstream uses a provider's model-auth variable, give it a separate daemon-only
credential instead of sharing that variable with the worker.

This is control-plane write protection, not same-uid secret isolation. It does not
solve operator-token theft (0237), change interactive configs (0540), or complete
app onboarding/fleet serve-once acceptance (0541/0254).

## Optional operator rollout and rollback

1. Back up catalog and `tether.db` with the matching identity token backup.
   Install the reviewed binaries through the coordinated cutover, keeping
   `identity.mode: observe`.
2. Set `daemon.mcp_endpoint.enabled: true`, retain ownership `legacy_proxy`, and
   restart through the existing cutover procedure. A daemon restart interrupts
   or stops managed sessions; checkpoint/drain them first. Run `tether doctor`:
   endpoint health must succeed with an admitted credential, and identity must
   be available. Provision app grants/receiver trust through the app onboarding
   tasks; HTTP receivers must pin the proxy service principal.
3. Set `daemon.mcp_upstream_ownership: daemon` in `global.yaml`. No restart is
   needed for this selector. Launch a canary with each provider and inspect its
   actual consumed config. Verify admitted tools, attribution and one daemon
   upstream owner before expanding rollout.
4. Running legacy sessions retain their existing configs/processes when only
   the selector changes. Drain them, or deliberately checkpoint/stop/resume or
   relaunch to plant the new route and mint new credentials. Mixed fleets still
   contain legacy pools; do not call them serve-once. Doctor reports active
   captured ownership counts; older/unplanted sessions are `unknown`.
5. Rollback: set ownership `legacy_proxy` for subsequent launches. Deliberately
   checkpoint/stop/relaunch existing thin sessions before disabling/restarting
   the endpoint. Never rewrite a running session's boot files or silently switch
   a disconnected forwarder into legacy mode. If rolling back the binary as well,
   relaunch sessions with the older binary: newly captured ownership policy
   digests are not understood by binaries predating this change.

No live cutover is performed by this change.
