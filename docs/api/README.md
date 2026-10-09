# Tether Local API (v0.0.2)

The `tetherd` daemon exposes an HTTP API for session lifecycle, attach streaming,
checkpoints, broker envelopes, and event observation. Clients are assumed to
run on the same host — there is no authentication. Trust is anchored to the
UDS filesystem permissions (or loopback interface for TCP transports).

## Transport

- Default: Unix domain socket at `~/.tether/run/tetherd.sock`.
- Alternate: TCP on loopback when `daemon.listen_addr` is `tcp:127.0.0.1:PORT`.

Over UDS, clients address the daemon with the `http://unix/<path>` convention;
the path component of the URL carries the API route.

## Common conventions

- All request/response bodies are JSON unless noted (attach is
  `application/octet-stream`, event stream is `text/event-stream`).
- Timestamps are RFC3339 UTC. Historical store queries emit RFC3339 with
  nanosecond precision.
- IDs are UUIDv7 for envelopes and checkpoints (time-sortable); sessions keep
  the v0.0.1 UUIDv4 form.

### Error envelope

Every non-2xx JSON response carries:

```json
{
  "error": {
    "code": "<machine code>",
    "message": "<human message>"
  }
}
```

Defined codes:

| Code                | HTTP | Meaning                                       |
|---------------------|------|-----------------------------------------------|
| `invalid_request`   | 400  | malformed body, missing required param        |
| `forbidden`         | 403  | caller identity is not permitted for this resource — on messaging paths, `as` did not match the message's sender or recipient; on launch paths, a launch the daemon refuses by policy: an ACP-mode launch while Tether write-protects its directories, which the ACP launcher cannot do until CW-20261001-0162 (see [provider runtime sessions](../provider-runtime-sessions.md)); a launch whose work directory or workspace lies inside a write-protected directory, the state directory included; or any launch while that protection is on and `bwrap` is not installed (see [control-plane protection](../sandboxing.md#control-plane-protection-every-agent-tether-wraps)) |
| `not_found`         | 404  | resource or action path doesn't exist         |
| `method_not_allowed`| 405  | route exists, method doesn't                  |
| `conflict`          | 409  | state precondition failed (e.g. wrong state)  |
| `provider_session_lost` | 409 | the provider no longer has the session's resume id; the turn was not delivered and a resend starts a fresh provider session without the old history |
| `project_layer_unprotectable` | 403 | a registered project's catalog layer cannot be protected and cannot be left open, because of that project's catalog entry or the file system under it: its `repo_root` runs through a file in a directory an agent can write, or through a symlink an agent could replace, or cannot be examined, or its layer turned up while protection was creating it. The message names the project and the path. It is not a host that cannot provide protection (that is `forbidden`, bubblewrap missing): fix or remove the project. A project whose root is merely missing is never this: it is anchored or skipped. **Not every unprotectable shape is typed yet:** a `<root>/.tether` that is a file, and a user-owned unwritable `/` as the nearest ancestor, still surface as an untyped 500 (see "Known limits" in [sandboxing.md](../sandboxing.md), CW-20261003-0106) |
| `project_root_missing` | 409 | the project a launch is for has a `repo_root` that does not exist or is not a directory, so the launch cannot run: it does not exist, is not a directory, or is only the placeholder protection created for it (see `/health` `sandbox_protect.created_project_roots`); the message names the project and the path. Another project's dead `repo_root` does not cause it: that root is created as a placeholder holding only an anchored `.tether`, or skipped where no agent could create it (`skipped_project_layers`) |
| `idempotency_conflict` | 409 | an `idempotency_key` was reused with a different request; the key stays bound to the session its first request created |
| `payload_too_large` | 413  | body exceeded per-route cap                   |
| `reply_target_not_a_session` | 400 | `POST /messages/{id}/reply` named a message that is not a channel publication from a Tether session (an ordinary mailbox message keeps the mailbox path), so there is no session to deliver to |
| `reply_not_mailbox` | 400 | a mailbox verb (cancel, consume, read, archive, claim, ack, nack, redrive, delete) was aimed at a reply; its delivery state is `GET /messages/{id}/delivery` only |
| `turn_feed_unavailable` | 409 | a reply's target session runs on a runtime with no turn lifecycle (a PTY), so Tether cannot tell when it is idle; nothing was queued |
| `interrupt_unsupported` | 409 | a reply asked for `interrupt:true` and the session's runtime cannot cancel a turn; the reply was **not** accepted |
| `turn_not_yet_started` | 409 | a reply asked for `interrupt:true` while the session's turn was submitted but the runtime had not started it, so there is nothing safe to cancel; nothing was queued, retry |
| `turn_failed`       | 502  | the session's agent process ran the turn and exited non-zero (subprocess runtimes: codex exec, claude -p, opencode run, agy); the message carries the exit status and up to 2 KB of the turn's stderr |
| `locked`            | 423  | resource is archived or otherwise closed to writes |
| `not_implemented`   | 501  | route exists, semantics land in a later version |
| `internal_error`    | 500  | unexpected server failure                     |

---

## Team verbs

The daemon constructs the team host and mounts these routes only when
`teams.enabled` in `global.yaml` is explicitly true; the default is false.
The same service supplies native MCP team tools. A disabled or missing service
has no team routes (404) or MCP tools; with the key off the CLI namespace is absent.
The daemon service enforces authentication and authority. Construction does no
enrollment, launch, publication or recovery work, and recovery is not scheduled.

Production formation remains unavailable: host formation policy refuses it,
and actor/pin trust and exact-pin legacy launch targets are empty. They must be
explicitly provisioned after activation readiness is resolved in
CW-20261004-0002. Neither catalog launch names nor caller-authored definitions
grant trust. Do not enable the flag or restart/deploy the live daemon as part of
this wiring change.

All verbs use POST and the `Idempotency-Key` header (required, at most 256 bytes,
valid UTF-8 without control characters). MCP and CLI reject surrounding key
whitespace. Go trims HTTP header whitespace on a real socket before the handler
sees it, so an HTTP caller's padded header arrives as the unpadded key.
The key is scoped by authenticated caller and verb across runs. Reusing it with
changed content conflicts; retries return the retained result and recipients.
No caller, verified, operator or session claim is accepted in the body. Caller
identity comes exclusively from the daemon's authenticated request context;
unverified or asserted identities are refused. The authenticated local operator
exception affects verification only, never membership, grants or host ceilings.

| Route | Body fields |
|---|---|
| `/teams/form` | `team`, optional `launch` (`counts`, `limits`, `pool_identities`) |
| `/teams/dissolve` | `run_id` |
| `/teams/member_add` | `run_id`, `slot`, optional `limits` |
| `/teams/member_remove` | `run_id`, `member_id` |
| `/teams/assign` | `run_id`, `body`, optional `address`, `kind`, `history` |
| `/teams/delegate` | `run_id`, `body`, optional `address`, `kind`, `history` |
| `/teams/address` | `run_id`, `body`, optional `address`, `kind`, `history` |
| `/teams/cancel` | `run_id`, `member_id`, optional `cascade` (default false) |
| `/teams/report_result` | `run_id`, `delegate_key`, `body` |

`team` follows the mesh team definition schema. `limits` uses snake_case mesh
limit fields, such as `max_depth`, `max_children`, `fan_out`, `budget`, `timeout` (integer nanoseconds),
`max_rounds` and `max_stalls`. `history` and `kind` use mesh vocabulary. Formation
refuses ungoverned authority and limits above the host policy before storing the
definition. Addressing, assignment, delegation and cancellation retain library
grant checks and spawn limits; these wrappers add no authority.

Successful requests return HTTP 200 with the unchanged service Result: optional
`run` metadata, `member`, `recipients`, and `delivery_keys`. Member views contain
only `id`, `slot`, `actor`, `status`, and `kind`. Provision intents, workspaces,
quotas, sessions and internal retry routes are never response fields.

Fields outside a verb's table row are rejected. Required fields must be present;
required strings must be nonempty. Message addresses remain optional: an empty
or omitted address uses the library's authored routing rules and coordinator
fallback. Empty `member_id` and `slot` now return `invalid_request` (400) on all
three surfaces; the underlying service would return typed `not_found` (404).
Top-level field names are case-sensitive; nested JSON field names remain case-insensitive and
duplicate keys take the last value. The service still validates team definitions,
membership and library requests after transport validation.

Team errors use the standard error envelope with cause-free code/message:
`unauthenticated` (401), `unavailable` (503), `denied` (403), `not_found` (404),
`conflict` (409), `invalid_request` (400), or `internal_error` (500). Raw bodies
are limited to 64 KiB; encoded team definitions to 64 KiB; whole transport
requests to 128 KiB. Unknown fields, trailing JSON and oversized inputs are
invalid requests. CLI transport-only codes include `method_not_allowed` (405)
and `teams_disabled` (404, absent route without a team error envelope). The client
maps non-envelope 5xx responses to `unavailable` (503); canonical team
`internal_error` envelopes retain 500. Existing endpoints retain their own error
conventions.

The CLI sends the same JSON request through the daemon, using the normal bearer
credential configuration (`--token-file` or the existing credential lookup):

```sh
tether team assign --key work-1 --request '{"run_id":"run-id","address":"@workers","body":"Review the change"}'
tether team cancel --key stop-1 --request '{"run_id":"run-id","member_id":"member-id","cascade":true}'
tether team form --key start-1 --request-file team-request.json
```

Every route suffix is also a CLI verb, including `member_add`, `member_remove`
and `report_result`. `--request-file -` reads stdin. Results are JSON; CLI errors
retain the same typed code and HTTP status without server causes. There is no
CLI flag or environment variable that enables teams.

Activation follow-ups: agent session credentials currently lack `team.write`
(`workerScopes` mints session, message and catalog write scopes). Agents cannot
use the MCP team verbs until scope minting is deliberately changed; who receives
that scope is an activation policy decision. HTTP has no token-scope checks
anywhere under the existing convention, so HTTP and CLI can reach the team
service with a verified read-only-scoped bearer while MCP refuses it. Membership
and service grants remain the authority gate; HTTP scope enforcement is a
separate hardening follow-up. This change does not alter session minting.

---

## AI Gateway

The AI gateway is optional. `tetherd` only mounts `/ai/*` when `global.yaml`
contains at least one enabled AI provider that can be built successfully at
startup.

All write-side AI endpoints accept a typed JSON envelope:

```json
{
  "request": {
    "operation": "chat",
    "provider_hint": "anthropic-work",
    "model_hint": "claude-sonnet-4-5",
    "mode": "summarize",
    "intent": "release-notes",
    "request_id": "req-123",
    "session_id": "sess-123",
    "caller_id": "cli:tether",
    "max_output_tokens": 512,
    "token_budget": 4000,
    "cost_budget_usd": 0.10,
    "latency_target_ms": 1500,
    "input": [
      {
        "role": "system",
        "parts": [{"type": "text", "text": "Be concise."}]
      },
      {
        "role": "user",
        "parts": [{"type": "text", "text": "Summarize this PR."}]
      }
    ],
    "tools": [],
    "attachments": [],
    "metadata": {"surface": "api"}
  }
}
```

`request.operation` defaults to `"chat"` when omitted. `POST /ai/chat` and
`POST /ai/chat/stream` reject any other operation; `POST /ai/embeddings`
defaults to `"embedding"` and accepts only that operation. `POST /ai/routes/preview`
accepts the full normalized request so the planner can inspect capabilities,
budgets, tools, attachments, and embedding inputs before choosing a route.

### `GET /ai/providers`

List the configured AI providers that are active in the daemon.

Response (200):

```json
{
  "providers": [
    {
      "id": "anthropic-work",
      "type": "anthropic",
      "default_model": "claude-sonnet-4-5",
      "base_url": ""
    }
  ]
}
```

### `GET /ai/models`

List the configured models visible through the mounted providers.

| Query param | Type | Description |
|-------------|------|-------------|
| `provider_id` | string | optional configured provider id filter |

Response (200):

```json
{
  "models": [
    {
      "configured_provider_id": "anthropic-work",
      "vendor_provider_id": "anthropic",
      "id": "claude-sonnet-4-5",
      "name": "Claude Sonnet 4.5",
      "family": "claude-sonnet-4",
      "context_window": 200000,
      "max_output_tokens": 16000,
      "input_modalities": ["text"],
      "output_modalities": ["text"]
    }
  ]
}
```

### `GET /ai/routes`

List the live planner routes mounted into the daemon in evaluation order.
The returned allow/deny fields are the effective resolved policy values after
global defaults, provider policy, and route overrides are merged. The same is
true for `max_output_tokens`, `max_cost_usd`, and `usage_budget` when present.

Response (200):

```json
{
  "routes": [
    {
      "provider": "anthropic-work",
      "model": "claude-sonnet-4-5",
      "requires_reasoning": true,
      "allow_tools": false,
      "usage_budget": {
        "level": "route",
        "max_cost_usd": 1.0,
        "window": "day",
        "scope": "caller"
      }
    },
    {
      "provider": "openai-work",
      "model": "gpt-5",
      "mode": "summarize"
    }
  ]
}
```

### `POST /ai/routes/preview`

Preview which provider/model the planner would choose without invoking a
model.

Response (200):

```json
{
  "route": {
    "provider": "anthropic-work",
    "model": "claude-sonnet-4-5",
    "estimated_cost_usd": 0.0031,
    "reasons": ["matched provider hint"],
    "policy_version": "catalog-ai-v1"
  }
}
```

Errors: `invalid_request` for malformed payloads or unsupported inputs;
`internal_error` for planning failures.

### `POST /ai/routes/explain`

Return a structured planner trace for one normalized request. Unlike
`/ai/routes/preview`, this surface includes every configured route plus the
winner, exclusion reasons, and per-candidate evaluation failures, including
historical usage-budget rejections.

Response (200):

```json
{
  "policy_version": "catalog-ai-v1",
  "winner": {
    "provider": "anthropic-work",
    "model": "claude-sonnet-4-5",
    "policy_version": "catalog-ai-v1",
    "reasons": ["matched route reasoning requirement"]
  },
  "candidates": [
    {
      "provider": "openai-work",
      "model": "gpt-5",
      "matched": true,
      "error": "route policy disallows tools",
      "reasons": ["matched route reasoning requirement"]
    },
    {
      "provider": "anthropic-work",
      "model": "claude-sonnet-4-5",
      "matched": true,
      "selected": true,
      "reasons": [
        "matched route reasoning requirement",
        "supports reasoning",
        "selected after ordered fallback"
      ]
    }
  ]
}
```

### `GET /ai/budgets`

List the live durable `usage_budget` entries mounted into the daemon, along
with current spend and remaining headroom from `ai_events`.

Query params:

| Query param | Type | Description |
|-------------|------|-------------|
| `provider` | string | optional configured provider id filter |
| `model` | string | optional model id filter |
| `caller_id` | string | caller correlation id used for caller-scoped budgets |
| `session_id` | string | session correlation id used for session-scoped budgets |

Response (200):

```json
{
  "budgets": [
    {
      "provider": "anthropic-work",
      "model": "claude-sonnet-4-5",
      "usage_budget": {
        "level": "route",
        "max_cost_usd": 1.0,
        "window": "day",
        "scope": "caller"
      },
      "window_start": "2026-05-25T00:00:00Z",
      "spent_cost_usd": 0.6,
      "remaining_cost_usd": 0.4,
      "filter": {
        "provider": "anthropic-work",
        "model": "claude-sonnet-4-5",
        "caller_id": "agent-1",
        "operation": "chat"
      }
    }
  ],
  "count": 1
}
```

When a caller- or session-scoped budget cannot be evaluated because the
required correlation id is missing, the entry is still returned with an
`error` field instead of failing the whole response.

When no route wins, the endpoint still returns `200` with `error` populated and
the per-route failure/exclusion details in `candidates`. Input validation and
unsupported operations still return `invalid_request`.

### `POST /ai/chat`

Execute one normalized chat request through the gateway.

Response (200):

```json
{
  "response": {
    "provider": "anthropic-work",
    "model": "claude-sonnet-4-5",
    "output": [
      {
        "role": "assistant",
        "parts": [{"type": "text", "text": "Here is the summary."}]
      }
    ],
    "stop_reason": "end_turn",
    "usage": {
      "input_tokens": 120,
      "output_tokens": 42,
      "estimated_cost_usd": 0.0031
    },
    "route": {
      "provider": "anthropic-work",
      "model": "claude-sonnet-4-5",
      "reasons": ["matched provider hint"],
      "policy_version": "catalog-ai-v1"
    }
  }
}
```

Errors: `invalid_request` when the body is malformed or `operation` is not
`chat`; `not_found` when no route/provider can satisfy the request;
`internal_error` for upstream provider failures.

### `POST /ai/chat/stream`

Execute one normalized chat request through the gateway and stream
incremental SSE events.

Response content type: `text/event-stream`

Current normalized event kinds:

- `response.start`
- `response.output_text.delta`
- `response.refusal.delta`
- `response.tool_use`
- `response.error`
- `response.completed`

Example:

```text
event: response.output_text.delta
data: {"kind":"response.output_text.delta","provider":"anthropic-work","model":"claude-sonnet-4-5","delta":"Hello"}

event: response.completed
data: {"kind":"response.completed","provider":"anthropic-work","model":"claude-sonnet-4-5","stop_reason":"end_turn","response":{"provider":"anthropic-work","model":"claude-sonnet-4-5","output":[{"role":"assistant","parts":[{"type":"text","text":"Hello"}]}]}}
```

`response.completed` carries the final normalized `llm.Response`, including
usage and route information. `response.error` is emitted when the upstream
stream fails after the SSE response has already started.

### `POST /ai/embeddings`

Generate embedding vectors through the gateway.

Response (200):

```json
{
  "response": {
    "provider": "openai-work",
    "model": "text-embedding-3-small",
    "embeddings": [
      {"index": 0, "vector": [0.1, 0.2]},
      {"index": 1, "vector": [0.3, 0.4]}
    ],
    "usage": {
      "input_tokens": 11
    },
    "route": {
      "provider": "openai-work",
      "model": "text-embedding-3-small",
      "policy_version": "catalog-ai-v1"
    }
  }
}
```

Errors: `invalid_request` when the body is malformed, `embedding_input` is
missing, or `operation` is not `embedding`; `not_found` when no route/provider
can satisfy the request; `internal_error` for upstream provider failures.

### `GET /ai/usage`

Return durable aggregate usage derived from `ai_events` rows of
`event_type=chat`.

| Query param | Type | Description |
|-------------|------|-------------|
| `provider` | string | optional configured provider id filter |
| `model` | string | optional model id filter |
| `session_id` | string | optional session correlation filter |
| `caller_id` | string | optional caller correlation filter |
| `operation` | string | optional operation filter; typical value `chat` |
| `since` | RFC3339 | optional lower-bound timestamp |

### `GET /ai/audit`

Return durable sanitized AI audit events.

| Query param | Type | Description |
|-------------|------|-------------|
| `event_type` | string | optional event kind such as `chat`, `route_preview`, or `budget_rejection` |
| `provider` | string | optional configured provider id filter |
| `model` | string | optional model id filter |
| `session_id` | string | optional session correlation filter |
| `caller_id` | string | optional caller correlation filter |
| `since` | RFC3339 | optional lower-bound timestamp |
| `limit` | int | optional max rows, capped by the server |
| `errors_only` | bool | optional failed-events-only filter |

`budget_rejection` rows are emitted in addition to the parent `chat`,
`route_preview`, or `route_explain` row when durable `usage_budget` policy
blocks a candidate route. These rows pin the rejected provider/model directly
so operators can alert on spend governance failures without parsing the
aggregated planner error text.

---

## Health

### `GET /health`

Returns daemon liveness and basic telemetry.

Response:

```json
{
  "status": "ok",
  "pid": 12345,
  "uptime_sec": 42,
  "listener": "unix:/srv/me/.tether/run/tetherd.sock",
  "sessions": 2,
  "sandbox_protect": {
    "enabled": true,
    "reason": "on: Claude, OpenCode and every agent Tether wraps cannot write the catalog, run/ or state/; Codex is NOT protected (CW-20261001-0230), it relies on its own workspace-write sandbox",
    "codex": "not protected",
    "codex_reason": "not protected (CW-20261001-0230): codex runs under its own workspace-write sandbox … and codex spawns every MCP server it is given outside that sandbox …",
    "bwrap_checked": true,
    "bwrap_usable": true,
    "created_project_roots": [
      {"project": "old-site", "repo_root": "/srv/me/dev/old-site", "reason": "does not exist and an agent could have created it (/srv/me/dev is writable): it was created holding only a read-only .tether, so a protected agent cannot plant a layer there"}
    ]
  }
}
```

`sandbox_protect` is the daemon's own view of [control-plane
protection](../sandboxing.md#control-plane-protection-every-agent-tether-wraps), decided from
the daemon's environment, which `tether doctor` reads from here and not from its
own shell. `enabled` says whether launches are protected; `disabled_by_operator`
is present when `TETHER_SANDBOX_PROTECT=0` turned it off; `reason` says what the
state means for an agent. On Linux with protection on, the daemon probes
bubblewrap: `bwrap_usable` is false, with `bwrap_error`, when it cannot build the
sandbox, in which case every launch except Codex's is refused (`bwrap_usable` is
omitted, not `null`, when it is false). `created_project_roots` lists registered
projects whose `repo_root` did not exist and that an agent could have created
(the nearest existing directory above it is writable by the daemon's user):
protection created the root as a placeholder holding only a `.tether` (with a
one-file note) and anchored it read-only, so a protected agent cannot plant a
project layer there. An entry is listed while the root is still only that
placeholder, including after a daemon restart, and is a stale catalog entry to
fix; the project's own launches are refused (409 `project_root_missing`) until
the repository is restored and its marker removed (a placeholder is a placeholder
while it holds `.tether/created-by-tether-protection`, whatever else has been put in
the directory since). `anchored_project_ancestors` lists projects whose `repo_root`
is missing under a directory that is not writable but that an agent can get past
(it runs as the daemon's user, who owns the directory, or can write the one above
it): protection anchored that directory read-only (it cannot be made writable,
written to or renamed inside the sandbox), so nothing can be planted under it, and
no launch is refused for it except one whose own directories lie inside it.
`unprotectable_project_layers` lists every project whose layer cannot be protected
and cannot be left open (`project`, `repo_root`, and `why`, which says what to fix:
for a symlink, to replace it with the real directory or fix `repo_root`); `plan_error`
carries the same joined. Launches of agents Tether protects are refused with 403
`project_layer_unprotectable` while any is listed, whichever project is launched (the
loader reads every project's layer on every launch). `skipped_project_layers` lists
projects whose `repo_root` is
unusable AND that no agent could create: the agent runs as the daemon's user, so
nothing from the nearest existing directory above the root up to `/` may be owned by
that user or writable by it, and no symlink in the path may be one the agent can
replace. There is nothing to plant into; their layer is left out. Every project's
launches work otherwise. `plan_error` is present when Tether cannot work out
what to protect at all (for instance a project whose `repo_root` runs through a
file or a symlink an agent could replace, or cannot be examined), and every launch it
must protect is refused until that is fixed (403 `project_layer_unprotectable`,
which names the project; the planted proxy of a Codex launch and the daemon's MCP
gateway are not refused, and leave that one layer out of their confinement with a
warning); it is a catalog or filesystem problem, never a bubblewrap one, and
`bwrap_checked`/`bwrap_usable` still report the host probe on their own. **Known
limits** (see "Known limits" in [sandboxing.md](../sandboxing.md), CW-20261003-0106):
the gateway and the Codex proxy are not spared by a `<root>/.tether` that is a file
or by a root under a file (`/mcp` answers 503; protected launches get an untyped 500
for the former); a project added to the catalog while the daemon runs is not in what
`plan_error` and the lists above are computed from until the next restart; and
`EACCES`/`ENAMETOOLONG` roots are typed or skipped only in unit tests, because a
running daemon does not start with one. `codex` is how
Codex is protected: `not protected` as shipped (Codex runs as it did before
protection, under its own sandbox, and spawns MCP servers outside it, so an MCP
tool can reach the catalog; the catalog-writing `tether` tools are still refused for
it), `guarded` only if the dormant guard is switched on, or `not applicable`
(protection is off); `codex_reason` says what that means and names
CW-20261001-0230, the structural reason (Codex spawns MCP servers outside its
sandbox). An older daemon omits the field.

---

## Sessions

Direct-launch sessions move through `created → launching → running → {completed|failed|killed}`.
Shim recovery additionally uses the non-terminal `detached` and `orphaned` states.
`POST /sessions` creates (state=created); `POST /sessions/{id}/launch` starts.
The full vocabulary is `created`, `ready`, `launching`, `running`, `detached`,
`orphaned`, `completed`, `failed`, and `killed`. State filters use exact matches.
What each state means is under
[Driving sessions from an orchestrator](#driving-sessions-from-an-orchestrator).

### `POST /sessions`

Create a session from a launch profile. Does not start the runtime.

Request body:

```json
{ "launch": "my-launch-id" }
```

Response (201):

```json
{
  "id": "88e1c18c-fca2-40a9-ac3a-ba25fd790869",
  "workspace": "/path/to/workspaces/proj/88e1c18c.../",
  "log": "/path/to/workspaces/proj/88e1c18c.../logs/session.log",
  "provider_id": "claude-stream"
}
```

`provider_id` resolves from the launch profile's referenced provider. Clients
use it to dispatch provider-kind-specific paths (chat surface vs. raw PTY
attach) without a follow-up `GET /sessions/{id}` round-trip.

Every create response carries `"replayed": false`, or `true` for an
idempotent replay (below).

The launch is read from the catalog's `launches/` on every create, so a launch
file added or edited there is used without a daemon restart. Projects, agents
and providers are still the ones loaded at daemon start, and a launch naming one
added since then is refused until the daemon restarts. An unknown `launch`
answers 404 `not_found`; the message lists the launches the catalog defines.

The optional `route` object opts this session into per-turn channel routing:
`{"channel":"ops","kinds":["final","question","approval","failure"]}`.
Omitting `route` leaves routing disabled. Omitting `kinds` selects all four;
`[]` selects none. Invalid channels or unknown kinds answer 400
`invalid_request` using the standard error envelope. The resolved route is
persisted at create time. Replies always return to the sender. See
[caller-launched sessions](../caller-launched-sessions.md#opt-in-session-routing)
for catalog, override and CLI configuration.

#### Idempotency keys

`POST /sessions` and `POST /logical-agents/{id}/resume` accept an optional
`idempotency_key` (at most 512 bytes, no surrounding whitespace, no control
characters). It makes a request safe to retry after a lost response:

- The first request with a key creates the session and binds the key to it
  permanently, in the same transaction as the session row. A create that fails
  before the row commits binds nothing.
- A retry with the same key and the same request returns that session with
  `"replayed": true` and HTTP **200** (a fresh create is 201), whatever state it
  is now in. A session that has since failed or stopped is returned unchanged,
  never replaced; the caller decides what to do with it.
- The same key with a different request answers 409 `idempotency_conflict`.
  "The request" is what the caller sent, hashed: for a create, `launch` and
  every payload field; for a resume, the logical agent. It is never anything
  the daemon resolved, so a resume retried after the resumed session has
  written its own checkpoint still replays.
- Only a digest of the request is stored, never the request itself.
- `POST /sessions/{id}/launch` on a session created with a key is idempotent
  too: once it is past `created`, a repeat answers 200 with `"replayed": true`
  instead of 409.

Keys are one **global, unauthenticated** namespace: Tether has no
authenticated caller identity yet (CW-20260918-0037), so a key is not a
security boundary. Prefix keys with your application and scope, e.g.
`hadron/<run>/<node>/<iteration>`. Identity-scoped keys can follow once caller
identity lands. Keys live as long as their session row; Tether does not purge
sessions today, so in practice they are permanent.

### `POST /sessions/{id}/launch`

Transition a `created` session to `running`. Returns 409 `conflict` when the
target is in any other state, except for a session created with an
`idempotency_key`, where a repeat answers 200 with `"replayed": true`.

Response (200):

```json
{
  "id": "88e1c18c-...",
  "workspace": "/...",
  "log": "/...",
  "provider_id": "claude-stream"
}
```

### `GET /sessions`

List sessions with optional filters.

| Query param | Type       | Description                                                   |
|-------------|------------|---------------------------------------------------------------|
| `limit`     | int 1–1000 | default 100                                                   |
| `cursor`    | RFC3339    | return rows strictly older than this created_at               |
| `state`     | string     | exact-match state filter                                      |

Response (200):

```json
{
  "sessions": [
    {
      "id": "88e1c18c-...",
      "launch_id": "my-launch",
      "project_id": "demo",
      "logical_agent_id": "demo-agent",
      "provider_id": "api-stub",
      "workspace": "/...",
      "state": "running",
      "pid": 0,
      "created_at": "2026-04-19T08:16:41Z",
      "updated_at": "2026-04-19T08:16:41Z",
      "attached_clients": 0
    }
  ],
  "next_cursor": "2026-04-19T08:14:10Z"
}
```

`next_cursor` is only emitted when the page filled the requested limit; pass
it back as `?cursor=` to continue.

### `GET /sessions/{id}`

Fetch a single session. 404 when not found.

Response (200): single `SessionDTO` as above, plus `workstream_id` when the
session is assigned to a workstream.

### `POST /sessions/{id}/stop`

Signal the runtime to terminate the session. No body.

Response: 204 on success; 404 when the session is not currently running
(already exited or never registered).

The stop is asynchronous: 204 means the signal was sent. When the process
exits, the session's state becomes `killed`. That is distinct from
`completed` and `failed`, whatever exit code the process returned.
`GET /sessions/{id}/wait` returns once that state is recorded.

### `GET /sessions/{id}/wait`

Long-poll until the session reaches a terminal state. Returns the exit code.

Response (200):

```json
{ "exit_code": 0 }
```

### `POST /sessions/{id}/input`

Write raw bytes to the session's PTY input. Request body is
`application/octet-stream` — no framing, no newline normalization. 1 MiB
per-request cap.

Response: 204 on success; 409 `conflict` when the session has no writable
input channel (e.g. api-stub runtimes); 409 `provider_session_lost` when the
provider no longer has the session's resume id. The same applies to
`POST /sessions/{id}/turn`. On `provider_session_lost` the runtime has already
dropped the id and a `provider.session_lost` event is published; resending
the same request is the caller's decision, because the new turn starts
without the old history.

On a subprocess-runtime session (one agent process per turn), a turn whose
process exits non-zero answers 502 `turn_failed`: the process failed, not
the daemon, and the session stays up for the next turn. The message reads
`turn failed: … runner: process exited <code>` followed by up to 2 KB of
that turn's stderr. The full stderr, and the turn's rendered output (reply
text, `[tool_use:…]`, `[error] …`, `[turn_done]`), are appended to the
session's `logs/session.log`, which `tether sessions tail` reads.

### `GET /sessions/{id}/attach`

Live-stream the session's PTY output as an unframed byte stream
(`application/octet-stream`). Flushes after each chunk. Connection stays
open until the session exits or the client disconnects.

| Query param | Type  | Description                                                  |
|-------------|-------|--------------------------------------------------------------|
| `since_seq` | int64 | resume: replay bytes beyond this session-byte offset only    |

When `since_seq` is older than the oldest retained byte (ring eviction),
the handler silently replays the full ring — clients detect gaps by byte-
count comparison.

### `POST /sessions/{id}/resize`

Propagate a terminal resize to the session's PTY. Called by any attached
client whose terminal needs to keep the underlying PTY's window size in
sync (e.g. interactive shell wrappers around the attach stream).
See ADR 0014 for the full rationale.

```json
{"rows": 42, "cols": 120}
```

Response: 204 on success.

Errors: `invalid_request` (missing/zero rows/cols or malformed JSON);
`not_found` (session is not currently registered in the runtime — e.g.
already exited); `internal_error` (rare `pty.Setsize` failure).

Provider runtimes without a PTY (stub API provider) accept the call and
no-op.

---

### Driving sessions from an orchestrator

What an orchestrator hosting agents on Tether (for example a workflow
engine's session host) can rely on, and what it cannot:

- **Launching without duplicates.** Send an `idempotency_key` on create or
  resume, and launch the returned id. Retrying either call after a lost
  response returns the same session (see [Idempotency keys](#idempotency-keys)).
  The key binds to one session for good, so a replay after that session has
  failed returns the failed session; starting over is a new key.
- **Observing.** `GET /sessions/{id}` gives the durable `state` (it survives a
  daemon restart), `GET /sessions/{id}/health` the live runtime,
  `GET /sessions/{id}/wait` blocks for the exit code, and
  `GET /sessions/{id}/events` / `GET /events/stream` carry state changes.
- **Cancelling.** `POST /sessions/{id}/stop`. The session ends in `killed`,
  durably, so a cancel is never mistaken for an agent that finished.
- **A daemon restart ends running sessions.** Agent processes are children of
  tetherd. On startup tetherd sweeps every session left `launching` or `running` to
  `failed` with exit code -1, so after a restart an observer sees a definite terminal state, not
  a session that silently vanished. Nothing reattaches to the old process.
- **Resume makes a new session.** `POST /logical-agents/{id}/resume` starts a
  new session linked to its parent by `parent_session_id`; it never reattaches
  the old one.
- **There is no "result" value.** Tether does not record a terminal result for
  a session. What an agent produced is in the attach stream
  (`GET /sessions/{id}/attach`) and the session log while it runs, and in any
  message it sends; its end is a state and an exit code. An orchestrator that
  needs a result value has the agent send it as a message (for example a reply
  to the envelope or message that started the work) and reads it from there.

#### Session states

```text
created ──launch──▶ launching ──▶ running ──┬──▶ completed   exited on its own, code 0
   │                    │                   ├──▶ failed      exited on its own, non-zero
   │                    │                   └──▶ killed      ended by POST /sessions/{id}/stop
   └────────────────────┴──▶ failed   the launch failed, exit code 1

launching | running ──planned daemon shutdown──▶ killed   reason daemon-shutdown
launching | running ──crash, then restart, process gone──▶ failed   exit code -1
```

| State       | Terminal | Reached by                                                        | `exit_code`                         |
|-------------|----------|-------------------------------------------------------------------|-------------------------------------|
| `created`   | no       | `POST /sessions`, or resume                                       | —                                   |
| `launching` | no       | `POST /sessions/{id}/launch`                                      | —                                   |
| `ready`     | no       | reserved runtime preparation state (currently not written)        | —                                   |
| `detached`  | no       | child alive, daemon disconnected; shim reconciliation can reattach | —                                  |
| `orphaned`  | no       | shim and child gone, or shim reconciliation disabled at restart   | —                                   |
| `running`   | no       | the runtime started                                               | —                                   |
| `completed` | yes      | the process exited on its own with code 0                         | `0`                                 |
| `failed`    | yes      | exited on its own non-zero; the launch failed; or swept at daemon start | the process's code; `1` for a failed launch; `-1` when swept |
| `killed`    | yes      | `POST /sessions/{id}/stop` (also `tether sessions stop`, MCP `tether_session_stop`, ACP session close), or a planned daemon shutdown | whatever the stopped process returned |

**`exit_code` -1 from the startup sweep means "swept at daemon start", not an
observed failure.** When the daemon starts, it settles every session the
previous daemon left `launching` or `running`:
- A session whose own process is still alive keeps its state. "Its own"
  means the recorded pid is alive and has the start time recorded at launch,
  not a later process that reused the pid.
- Every other session becomes `failed` with `exit_code` -1 and `ended_at` set
  to the restart. Tether did not see these processes exit; their outcome is
  unknown.

Each sweep that acts publishes one `daemon.sessions_swept` event that names
both sets.

A spared direct-launch session is still `running`, but the new daemon holds no
runtime handle for it, so it cannot be steered or stopped through Tether.

`detached` means a shim-hosted child is alive while the daemon is disconnected.
It preserves the child PID, credentials, bindings and workspace. This is distinct
from client detach (`client_attachments.detached_at`, CLI Ctrl-]), which does not
change the session state. `orphaned` means the shim and child are gone: credentials
and all binding generations are revoked, and the stale PID is cleared. Neither
state is terminal; no exit code or `ended_at` is invented. Workspace cleanup
preserves both states. Resume from an orphaned checkpoint creates a new session;
resume from a detached checkpoint returns HTTP 409 `conflict`, because its child
is still alive and the shim reconciler must reattach it.

The state plumbing alone does not move launches into these states. The opt-in
shim integration owns those transitions. With the shim path disabled, existing
launching/running sweep and shutdown behavior stays as described here. If startup
finds a detached row with no shim reconciler, it moves to orphaned with reason
`shim_reconcile_disabled`, regardless of PID liveness. Already orphaned rows are
left alone. Recovery transitions emit the existing `session.state_changed`
payload `{from, to, reason}`; reasons include `daemon-shutdown`,
`shim_unreachable`, `shim_reconcile_disabled`, and `shim_gone`.

Rows swept before this behaviour (up to 2026-10-01) were failed regardless of
liveness and are not backfilled. Treat their `exit_code` -1 the same way.

**`killed` with reason `daemon-shutdown` means the daemon was stopped on
purpose.** On a graceful shutdown (SIGTERM or SIGINT to `tetherd`,
`tether daemon stop`, `systemctl stop`/`restart`), the daemon marks every live
session as stopping before it drains them. Any session the daemon sees exit
during the drain (`daemon.shutdown_timeout`, default 10s) gets two records:
- its row is `killed`, with the process's own `exit_code`;
- its terminal `session.state_changed` event has `reason`
  `"daemon-shutdown"`.

The daemon does not signal sessions itself. A session that has not exited when
the drain ends is left `launching`/`running`, and the next start's sweep
settles it:
- spared if its own process is still alive;
- `failed` with `exit_code` -1 if it is gone.

A crash leaves no shutdown record, so its sessions are always settled by that
sweep. One `daemon.shutdown_sessions_ended` event names the sessions that
ended during the drain and those still running.

On a Linux host running the daemon as a systemd unit with the default
`KillMode=control-group`, agent processes share the daemon's cgroup and get
the same SIGTERM. A planned `systemctl restart` therefore normally records
them `killed` / `daemon-shutdown`. A crash, or an agent that outlasts the
drain, is swept at the next start. Sessions survive a restart only when the
daemon runs outside such a unit, for example `tether daemon start` or launchd.

Branch on `state`, not `exit_code`. A stopped process may exit `0` (it
handled `SIGTERM` and exited cleanly) or `-1` (a signal ended it). Only `killed` says the session was stopped. The same value
is the `to` of the session's terminal `session.state_changed` event. A
terminal state is final: a session never leaves it, and resuming makes a new
session.

## Checkpoints

v0.0.2 ships persistence only. No runtime side effects: creating a
checkpoint does not pause the session or snapshot context. Resume is a
501 placeholder until v0.0.3 Sprint v003-04.

### `POST /sessions/{id}/checkpoint`

Create a checkpoint row bound to the session's logical agent. Body is an
optional free-form object; every field is optional.

Request body (example):

```json
{
  "task_id": "T-42",
  "workflow_id": "wf-1",
  "status": "in_progress",
  "completed_work": "parsed input",
  "pending_work": "write output",
  "key_decisions": "chose impl A over B",
  "referenced_artifacts": "file:///tmp/foo.md",
  "summary": "mid-task checkpoint",
  "next_recommendation": "continue on branch foo"
}
```

Response (201):

```json
{
  "id": "019da4d0-32b2-7318-a2ab-290a92d58fba",
  "logical_agent_id": "demo-agent",
  "summary": "mid-task checkpoint",
  "created_at": "2026-04-19T08:16:41Z",
  "source_session_id": "88e1c18c-..."
}
```

### `GET /logical-agents/{id}/checkpoints`

List checkpoints for a logical agent, newest first.

Response (200):

```json
{
  "checkpoints": [
    { "id": "...", "logical_agent_id": "demo-agent", ... }
  ]
}
```

### `GET /logical-agents`

List logical agents with compact continuity metadata.

Response (200):

```json
{
  "agents": [
    {
      "id": "demo-agent",
      "name": "Demo Agent",
      "launch_id": "demo-launch",
      "checkpoint_policy": "on_stop",
      "checkpoint_status": "auto-stop"
    }
  ]
}
```

### `POST /logical-agents/{id}/resume`

Start a new session for the logical agent using its most recent checkpoint as
boot context and the agent's stored `launch_id`.

The body is optional. `{"idempotency_key": "..."}` makes the resume safe to
retry: the same key returns the session the first resume created
(`"replayed": true`, HTTP 200) rather than starting a second agent. See
[Idempotency keys](#idempotency-keys).

Response (201):

```json
{
  "id": "8e2b9f6b-...",
  "workspace": "/tmp/tether/demo-agent/...",
  "log": "/tmp/tether/demo-agent/.../session.log",
  "provider_id": "codex",
  "provider_kind": "cli",
  "logical_agent_id": "demo-agent"
}
```

### `GET /logical-agents/{id}/policy`

Read the daemon-honored policy for one logical agent.

Response (200):

```json
{
  "logical_agent_id": "demo-agent",
  "name": "Demo Agent",
  "launch_id": "demo-launch",
  "checkpoint_policy": "manual",
  "checkpoint_status": "",
  "updated_at": "2026-05-24T19:12:00Z"
}
```

### `PATCH /logical-agents/{id}/policy`

Update the daemon-honored policy for one logical agent.

Request body:

```json
{
  "checkpoint_policy": "on_stop",
  "checkpoint_status": "auto-stop"
}
```

Supported `checkpoint_policy` values today:

- `manual` — stopping a session does not create a checkpoint automatically.
- `on_stop` — `StopSession` creates a checkpoint before stopping the live runtime.

When `checkpoint_policy=on_stop`, a checkpoint write failure blocks the stop so
operators do not lose resumable continuity silently.

---

## Broker envelopes

Envelopes are the mailbox-style inter-session message carrier. Write paths
emit `broker.envelope_created` / `broker.envelope_replied` events on the
bus; payloads are NOT leaked into events (metadata only).

### `POST /broker/envelopes`

Create and persist a new envelope. Server assigns `id` (UUIDv7) and
`created_at`; every other field round-trips.

Request body:

```json
{
  "sender": "alice",
  "recipient": "bob",
  "workflow_id": "wf-1",
  "correlation_id": "optional-seed",
  "message_type": "request",
  "priority": 0,
  "payload": "{\"op\":\"do-thing\"}",
  "audit_json": "{}"
}
```

Response (201): full `EnvelopeDTO`.

### `GET /broker/envelopes`

List envelopes by recipient OR workflow. Exactly one of `?recipient=` or
`?workflow_id=` is required; passing both is 400. `?correlation_id=` scopes
a workflow query further.

Query params:

| Param            | Description                                              |
|------------------|----------------------------------------------------------|
| `recipient`      | undelivered envelopes for this recipient, FIFO by created_at |
| `workflow_id`    | all envelopes in workflow, chronological                 |
| `correlation_id` | (with workflow_id) scope to one conversation thread      |

### `GET /broker/envelopes/{id}`

Fetch a single envelope. 404 when missing.

### `POST /broker/envelopes/{id}/reply`

Create a reply envelope: swaps `sender`/`recipient` from the original,
propagates `workflow_id`, and sets `correlation_id` to either the original's
correlation_id (if set) or the original's id (seeding a new thread).
Request body is an `EnvelopeCreateRequest`; its sender/recipient fields are
ignored, but `message_type`, `priority`, `payload`, `audit_json` carry
through.

Response (201): full reply `EnvelopeDTO`.

---

## Events

v0.0.2 has an in-memory pub/sub bus (Sprint v002-06) over session, daemon,
and broker scopes. Every event also persists to the `events` table.

### `session.turn_output`

Reduced output events come from every native agentkit runtime and the ACP wrapper
Activity feed. Empty final answers are skipped; reducer-produced failure and
terminal outputs can emit events even when text is empty. Repeated identical
errors suppressed by the reducer can produce neither a failure event nor a
routed failure message, although internal turn completion records the failure. Reasoning, tool calls, and
narration from earlier blocks are excluded by the shared `turnoutput` reducer.

Payload:

```json
{
  "session_id": "session-uuid",
  "turn_id": "turn-id",
  "kind": "final",
  "stop_reason": "end_turn",
  "confidence": "exact",
  "runtime": "codex",
  "logical_agent_id": "agent-id",
  "project_id": "project-id",
  "workstream_id": "workstream-id",
  "message_id": "durable-message-uuid"
}
```

`kind` is `final`, `question`, `approval`, `failure`, or `terminal`; confidence
is `exact` or `heuristic`. Runtime uses registry IDs (`claude`, `codex`,
`opencode`, `antigravity`, `copilot`, `pi`). Unassigned workstream is an empty string.

When the session has a persisted route and its resolved `kinds` selects this
output, the full text is stored **once** as a staged message with payload
`{"text":"..."}`, attributed to `msg://session/local/<session_id>`; the event
carries its `message_id`. The channel router attaches that existing message.
Staging has no delivery, inbox, wake, subscription, or default list visibility,
including the sysop User inbox. It survives daemon restart. An unattached stage
can be explicitly purged after the normal 30-day retention window; purged stages
remain hidden and cannot be published.

Without a selected route kind, no durable message is created. The event instead
carries `text`, an excerpt of at most 4096 bytes on a UTF-8 boundary, and
`text_truncated: true` when shortened. Non-context route/body persistence errors
log and fall back to that excerpt without a `message_id`. Non-context session
metadata errors log and continue with an empty `workstream_id`.

The synchronous persistence attempt has a five-second overall budget. Context
errors on reads/staging, and any event-publication error, retry off the reader
with operation deadlines and backoff from 100 milliseconds to five seconds.
Retries retain a staged message ID; staging itself is idempotent for the
session/turn/kind tuple when the body matches. Empty turn IDs use fresh message
IDs; reused IDs with different bodies are logged and stored separately, with
stable IDs on retries of each body. Events can be delayed and arrive out of turn order within
one session (turn 2 before a retried turn 1). Use the session and turn IDs to
identify outputs; event order is successful persistence order, not model-turn
order. Channel publication can reorder independently.

The retry pool holds at most 64 outputs / 16 MiB for up to one minute. Shutdown
cancels in-flight work, makes one final bounded attempt per pending output and
joins workers before storage closes. An event is not guaranteed for every
completed turn: a full pool, prolonged failure or failed final attempt can lose
it. Before staging, a crash can also lose the output body. After staging, the
durable router scan can publish the body even when the event is lost. These
limits and empty-terminal attribution are detailed in
[the runtime contract](../runtime-turn-output.md#persistence-and-submission-boundaries).

This is the canonical event for turn output. Consumers such as Tangent's bridge
should migrate from `session.turn_waiting_input` to `session.turn_output`; no
second alias event is emitted. Existing session lifecycle events are unchanged.

### Retention

The daemon retains event history for **90 days by default**, with an hourly
sweep. Configure the shared app knob in `~/.tether/catalog/global.yaml`
(and restart the daemon to apply it):

```yaml
daemon:
  events_retention:
    enabled: true   # default true; explicit false also disables
    days: 90        # default 90; any value below 1 disables
```

This is daemon-wide app configuration, not the project/user onboarding settings
cascade. The same window applies to all three event histories, credential audit
and terminal A2A tasks; inserts no longer
evict proxy or AI records at 2,000 rows. Query limits still bound response size.
`tether doctor` and `tether settings` (also `--json`) show the effective catalog
value. They report catalog configuration, which the daemon applies on restart,
not a live daemon configuration snapshot. `tether settings` is read-only and
does not open the state database.

| Table | Retention policy | Reason / operator control |
|-------|------------------|---------------------------|
| `events` | Shared age window, default 90 days | Session/daemon/broker replay history |
| `proxy_events` | Shared age window, default 90 days | Durable tool-call history; no row-count eviction |
| `ai_events` | Shared age window, default 90 days | AI summaries and usage; usage totals cover retained history only |
| `a2a_tasks` | Shared age window since last update, default 90 days; terminal states only | Completed, failed, canceled and rejected tasks expire; submitted, working, input-required, auth-required and unknown states are preserved regardless of age. Expired tasks are no longer available through peer task lookup |
| `broker_envelopes` | Indefinite, including bodies; permanently outside the messages purge and automatic sweep | No broker-specific safe-purge contract; delivery obligations and request/reply correlation history must survive expiry |
| `session_refs` | Indefinite; outside automatic sweep | Provenance pointers; dangling refs after session deletion are retained (FK cascades are not enforced) |
| `checkpoints` | Indefinite; outside automatic sweep | Resume/recovery state; age alone does not establish safe deletion |
| `messages` | Indefinite structural rows; explicit manual body purge only | `/messages/retention/candidates` and `/messages/{id}/purge` preserve pending/repairable obligations; this knob does not purge bodies |
| `message_purge_audit` | Indefinite; outside automatic sweep | Atomic manual body-purge receipts: table, message ID, self-asserted `authorized_by` URN and timestamp; no body copy |
| `retention_audit` | Indefinite; outside automatic sweep | Durable sweep receipts, independent of expiring event history |
| `principals` | Indefinite; outside automatic sweep | Identity and revocation history; no automatic credential/principal deletion |
| `identity_audit` | Same `daemon.events_retention` window (default 90 days) | Credential-bearing request receipts; purge is audited, queue overflow/failure counters are in health/doctor |

**Existing installs:** unless explicitly disabled, the first sweep deletes rows
older than the configured window. Back up the state database before cutover.
There is no archive-before-delete step. The sweep deletes in batches of 1,000
and yields between batches. Each nonempty batch commits its deletion and a
`retention_audit` receipt atomically (table, cutoff, removed count, timestamp).
If the receipt cannot be written, that batch is rolled back. Logs report
per-table removals, including a partially completed sweep.

`Service.SweepEventRetention` returns a structured result with per-table counts,
cutoff and committed batch audit IDs, including partial results on cancellation
or error. This is the integration hook for future post-sweep consumers; it does
not contain deleted bodies or provide an archive-before-delete guarantee.
Replay is unaffected for a `since_seq` inside the window. Resuming from an older
event returns retained events after it, without the deleted gap.

Manual `POST /messages/{id}/purge` requires `authorized_by` (a valid,
self-asserted URN). Every actual content removal writes a durable
`message_purge_audit` row in the same transaction; a failed receipt write leaves
payload and metadata intact. Already-purged retries return `purged: false`
without another receipt. Receipts remain queryable by `message_id` even after
structural message deletion and are independent of the shared age window.
Pending, leased, dead-lettered and group-fanout obligations remain protected.
Legacy `broker_envelopes` has no purge path and is permanently outside this
messages-only mechanism; a separate broker policy would require its own safe
completion contract.

### `GET /events`

Durable cross-scope event history from the shared `events` table. Results are
newest first.

| Query param  | Type  | Description |
|--------------|-------|-------------|
| `scope`      | string (repeatable) | optional allow-list of `session` / `daemon` / `broker` |
| `kind`       | string (repeatable) | optional exact event kind allow-list |
| `session_id` | string | optional exact session id filter |
| `since_seq`  | int64 | optional lower bound; only events with seq > N are returned |
| `cursor`     | int64 | optional pagination cursor; only events with seq < N are returned |
| `limit`      | int 0–1000 | optional row limit; default 100 |

Response (200):

```json
{
  "events": [
    {
      "seq": 42,
      "at": "2026-05-25T18:42:00.123456Z",
      "scope": "broker",
      "session_id": "abc",
      "kind": "broker.envelope_sent",
      "payload_json": "{\"id\":\"m1\"}"
    }
  ],
  "next_cursor": 12
}
```

### `GET /events/tool-metrics`

Counters and cumulative latency histograms over retained completed-call events,
with optional exact `tool`/`upstream` and inclusive `since`/exclusive `until`
RFC3339 bounds. Returns `{groups, truncated, window:"retained_events"}`. Groups
contain tool/upstream/outcome, call/byte counters, `metadata_samples` and
`duration`/`gateway`/`forward` histograms (`count`, `sum_ms`, cumulative buckets).
Buckets end at 5/25/100/500/1000/5000 ms and infinity (`upper_ms:null`). A maximum
of 1,000 groups is returned by descending call count, then tool/upstream/outcome
to break ties; narrow filters when
`truncated` is true. Invalid selectors return 400 `invalid_request`, storage
failures 500 `internal_error`. Starts are excluded, and restart preserves
counts until the existing event-retention sweep removes old rows. This query
is global/unscoped across all callers, without identity filtering, and scans
retained completed-call events; a rollup is a later option for larger histories.

MCP: `tether_tool_metrics`. CLI: `tether events tool-metrics --json`. See
[tool-call telemetry](../mcp.md#tool-call-telemetry-v2) for metadata/privacy and
process-lifetime OTel semantics. `/proxy/events` query responses also include
v2 call metadata from migration 0042's compatibility projection.

### `GET /events/stream`

SSE stream of bus events. Replays history (via `since_seq`) then switches
to live.

| Query param  | Type  | Description                                                     |
|--------------|-------|-----------------------------------------------------------------|
| `since_seq`  | int64 | replay events with seq > N before live (0 = full replay)        |
| `scope`      | string (repeatable) | one of `session` / `daemon` / `broker`; filter      |
| `session_id` | string | restrict to a single session's events                          |

Frame format per event:

```
id: 42
event: session.state_changed
data: {"scope":"session","session_id":"abc","payload_json":"{...}"}

```

A blank line terminates each event. A keep-alive comment (`: ping`) fires
every 15s to keep proxies from closing idle connections.

Clients track `since_seq` themselves (typically the last seen `id:` field);
server does not persist subscription offsets.

CLI parity:

- `tether events history` → durable `GET /events`
- `tether events watch` → live `GET /events/stream`

### `GET /sessions/{id}/events`

Historical, paginated list of events for a single session. Newest first.

| Query param | Type       | Description                              |
|-------------|------------|------------------------------------------|
| `limit`     | int 1–1000 | default 100                              |
| `cursor`    | int64      | return rows with seq < cursor            |

Response (200):

```json
{
  "events": [
    {
      "seq": 42,
      "at": "2026-04-19T08:16:41.052136Z",
      "scope": "session",
      "session_id": "abc",
      "kind": "session.state_changed",
      "payload_json": "{\"from\":\"launching\",\"to\":\"running\"}"
    }
  ],
  "next_cursor": 38
}
```

`next_cursor` is emitted only when the page filled the limit.

### `GET /auth/context`

`GET /auth/context` returns secret-free session attribution derived only from
the middleware-verified principal and daemon session/binding state. It ignores
query/body/header session selectors. A missing principal, non-session principal,
or missing session returns an unverified context. It grants no authorization.
See [trusted session context](../trusted-session-context.md) for the wire fields
and provenance/forwarded-metadata contract.

### `GET /proxy/events`

Rows include an `attribution` object and an optional `claimed_session_id`.
`attribution.verified` describes a credential-derived session context.
Daemon-resolved operator/service principal identity remains present when
`verified:false`; session/agent/workstream claims require verified binding.
Legacy/anonymous top-level session IDs remain claims. See the
[trusted session context contract](../trusted-session-context.md).

Tool calls the MCP proxy has recorded, newest first. 404 when the daemon has no
proxy-event store.

| Query param   | Type    | Description                                  |
|---------------|---------|----------------------------------------------|
| `server`      | string  | exact upstream server id                     |
| `tool_name`   | string  | tool name prefix                             |
| `session_id`  | string  | exact session id                             |
| `limit`       | int     | default 100, max 500                         |
| `since`       | RFC3339 | exclude events at or before this time        |
| `errors_only` | bool    | `true` returns only failed calls             |

Response (200): `{"events": [{"id", "session_id", "server", "tool_name",
"args_schema_fp", "duration_ms", "ok", "error", "timestamp"}], "count": N}`.

### `POST /proxy/events`

Record a proxied tool call. Body:

| Field            | Type   | Description                                                     |
|------------------|--------|-----------------------------------------------------------------|
| `tool_name`      | string | required, at most 256 bytes                                     |
| `server`         | string | upstream server id; empty for a native tether tool                 |
| `session_id`     | string | the calling session                                             |
| `args_schema_fp` | string | fingerprint of the argument names, at most 64 bytes             |
| `duration_ms`    | int    | not negative                                                    |
| `ok`, `error`    | bool, string | outcome; `error` is cut to 4 KiB with a `…[truncated]` marker. A caller should cut it first: a body over 64 KiB is refused whole |
| `timestamp`      | RFC3339 | accepted and ignored: the daemon stamps every record itself   |
| `phase`          | string | `end` (default) records the call in `proxy_events`; `start` records nothing there and needs `publish` |
| `publish`        | bool   | also publish the call on the daemon's event bus as `tool_call_start` or `tool_call_end`. A non-empty `session_id` must then name an existing session (400 otherwise); an empty one goes on the daemon scope |

Response: 201 `{"ok": true}`. 400 for an unknown `phase`, a `start` without
`publish`, a missing or over-long field, or a body over 64 KiB; 404 for
`publish` when the daemon has no event bus or no way to look sessions up.

The daemon stamps the time of every record itself and ignores `timestamp`, so
a record cannot be back-dated. The caller of a published record is the
`tether mcp --daemon-only` server Tether plants in an agent, which cannot write
the daemon's state. Identity fields are recomputed from the verified principal;
`claimed_session_id` records a bounded unverified claim and client-supplied
`attribution` is ignored. Anonymous/off calls retain their legacy session claim
with `attribution.verified: false`; other call details remain caller assertions.

---

## Catalog

Read-only list endpoints for the four catalog types. Writes (create /
update / delete / reload) are deliberately not exposed in v0.0.2 —
see [ADR 0012](../adr/0012-catalog-read-api.md) for the "reads now,
writes deferred" rationale.

Conventions (all four routes):

- Method: `GET` only. Other methods return `405 method_not_allowed`.
- Response: `{"<type>": [<record>, …]}` — the inner records are the
  catalog structs as-loaded from YAML (see
  `internal/config/model.go`). No pagination, no cursor — the catalog
  is small enough that clients pull the full list and filter locally.
- Records are sorted by `id` so repeated calls emit a stable order.
- Fresh reads per request: handlers re-read the catalog root on each
  call, so edits to the YAML files are picked up without a daemon
  restart.
- Errors: a missing or unreadable catalog root surfaces as `500
  internal_error` with a message citing the offending path (e.g.
  `"catalog load failed: load global: read /…/global.yaml: no such
  file or directory"`).

### `GET /catalog/projects`

Response (200):

```json
{
  "projects": [
    {
      "id": "demo",
      "name": "Demo Project",
      "repo_root": "~/dev/hollis-labs/apps/tether",
      "tracking_root": "~/.tether/tracking/demo",
      "boot_fragments": ["boot/common.md"],
      "workspace": {
        "default_mode": "hybrid",
        "session_root": "~/.tether/workspaces/demo"
      }
    }
  ]
}
```

### `GET /catalog/agents`

Response (200):

```json
{
  "agents": [
    {
      "id": "demo-agent",
      "name": "Demo Agent",
      "roles": ["general"],
      "permissions": { "network": true, "default_sandbox": "workspace-only" }
    }
  ]
}
```

### `GET /catalog/providers`

Response (200):

```json
{
  "providers": [
    {
      "id": "api-stub",
      "type": "api",
      "command": "",
      "bootstrap": {},
      "env": { "mode": "merge" }
    }
  ]
}
```

### `GET /catalog/launches`

Response (200):

```json
{
  "launches": [
    {
      "id": "demo-launch",
      "project": "demo",
      "agent": "demo-agent",
      "provider": "claude-code",
      "workspace": { "mode": "hybrid" },
      "prompt": {
        "include_project_boot": true,
        "include_agent_boot": true,
        "include_knowledge_base": false
      },
      "overrides": {}
    }
  ]
}
```

---

## Session lifecycle policy

Agent profiles and launch entries accept a `lifecycle` block. Launch fields
inherit the profile field by field; omitted fields inherit, and `"0s"`
explicitly disables a limit. The create request's JSON `override.lifecycle`
applies last. Resolved values persist in the launch plan and remain stable
when the catalog changes or the daemon restarts.

```yaml
lifecycle:
  idle_timeout: 30m
  max_duration: 24h
  lease_duration: 2h
  orphan_grace: 30s
  request_grace: 2s
  terminate_grace: 5s
  kill_grace: 5s
```

Idle, duration and session lease limits default to disabled; no implicit 12-hour
ceiling applies. Duration and configured session lease budgets start at launch;
expiry of a current runtime binding also triggers lease reaping. Genuine runtime
activity resets the idle budget. The daemon checks every five seconds, gives
launches an orphan grace, and retains unknown/live process identities. Verified
missing processes become `orphaned` with authority revoked and their workspaces
preserved; late terminal transitions remain terminal.

Stop records its cause before requesting SIGINT, then sends SIGTERM and SIGKILL
only after the preceding grace expires. Shim stops use the authenticated,
generation-checked host controller. Linux direct process groups use a verified
pidfd; older kernels and non-process runtimes retain the owning runtime's native
teardown contract and grace behavior. Every attempted reap records an outcome,
including a retained session when teardown fails.

## Event kinds

Current (v0.0.2):

| Scope    | Kind                          | Emitted by                             | Payload                                                      |
|----------|-------------------------------|----------------------------------------|--------------------------------------------------------------|
| daemon   | `daemon.started`              | tetherd at listener-up                    | `{version, pid, listener}`                                   |
| daemon   | `daemon.shutdown_started`     | tetherd on ctx cancel                     | empty                                                        |
| daemon   | `daemon.shutdown_completed`   | tetherd after runtime drain, before Close | empty                                                        |
| daemon   | `daemon.shutdown_sessions_ended` | tetherd after the graceful-shutdown session drain, when any session was live | `{ended, ended_session_ids, still_running, still_running_session_ids}` — ended sessions were recorded `killed` with reason `daemon-shutdown`; still-running ones are left for the next start's sweep (see [Session states](#session-states)) |
| daemon   | `daemon.sessions_swept`       | the startup sweep, when it settles any session | `{swept, swept_session_ids, spared, spared_session_ids}` — swept sessions were failed with `exit_code` -1; spared ones still had their own process alive (see [Session states](#session-states)) |
| daemon   | `ai.budget_rejected`          | AI service on durable budget rejection | `{request_id?, session_id?, caller_id?, provider, model, policy_version?, error}` |
| session | `session.reaper` | periodic reaper and explicit stop | `{reason, stage, outcome, error?}`; reasons include `idle_timeout`, `max_duration`, `lease_expired`, `process_missing`, `user_stop`; stages include `requested`, `request_stop`, `terminate`, `kill`, `runtime_fallback`, `outcome`. Intent persists before signals; outcome records terminal or retained state. |
| session | `session.activity` | periodic snapshot of genuine runtime activity; durable history only | `{at}`; original observed activity timestamp, excluding binding heartbeats and reaper telemetry. |
| session  | `session.state_changed`       | runtime.Manager at every transition    | `{from, to, exit_code?, reason?}` — terminal `to` is `completed`, `failed` or `killed` (see [Session states](#session-states)) |
| session  | `session.turn_interrupt_requested` | CancelTurnAndWait before a runtime cancel attempt | `{actor, session_id, turn_id, result:"requested"}`; no reply body |
| session  | `session.turn_interrupt_completed` | CancelTurnAndWait on every outcome, including invalid actor/missing session | `{actor, session_id, turn_id?, output_turn_id?, output_kind?, stop_reason?, result, error?}`; result is `completed`, a typed refusal reason (`unsupported`, `no_turn_in_progress`, `turn_not_yet_started`, `turn_superseded`, `session_ended`, `interrupt_timeout`), or `error` |
| session  | `provider.session_lost`       | a resume turn that ran in a new provider session (agy) | `{requested, actual, reason}` — the turn ran; history was lost |
| session  | `provider.permission_denied`  | a headless tool action auto-denied (agy) | `{action, display_name}`                                   |
| session  | `routing.reply_delivered`     | the reply dispatcher, after a reply was injected as the session's next turn | `{reply_id, parent_id, state, reason?, original_session_id, target_session_id, delivered_to_session_id, logical_agent_id?, actor}` — no reply text; `reason` is `handed_off` when `delivered_to_session_id` differs from `original_session_id` (see [Replies to routed messages](#replies-to-routed-messages)) |
| session  | `routing.reply_undeliverable` | the reply dispatcher, when it gave up on a reply | same shape with `state: "undeliverable"` and `reason` / `detail` saying why |
| session | `session.turn_routed` | durable audit of atomic channel attachment; event-history reads only, absent from live/SSE fanout | `{actor, publisher, session_id, turn_id, channel, message_id}` |
| broker   | `broker.envelope_created`     | broker.Service on successful persist   | `{id, sender, recipient, workflow_id, correlation_id, message_type}` — metadata only, never payload |
| broker   | `broker.envelope_replied`     | broker.Service on successful reply     | same shape as created                                        |

Additional kinds will land as v0.0.3 extends runtime and broker semantics.

To watch live budget alerts, subscribe to
`GET /events/stream?scope=daemon&kind=ai.budget_rejected`.

---


## Routing capabilities

`GET /routing/capabilities` reports installed consumer paths. Add
`?session_id=<id>` to inspect the runtime/mode in that session's persisted
launch plan; an unknown session returns 404 `not_found`.

```json
{
  "route_supported": false,
  "reply_to_sender": false,
  "interrupt": false,
  "kinds_available": [],
  "delivery": "next-turn",
  "runtimes": {
    "codex": {
      "route_supported": false,
      "reply_to_sender": false,
      "interrupt": false,
      "kinds_available": [],
      "final_text_confidence": "unknown"
    }
  }
}
```

The runtime map uses registry primary ids (`claude`, `codex`, `antigravity`,
`opencode`, and ACP agent ids). Gateway booleans mean at least one advertised
runtime supports that path; gateway kinds are their union. When several catalog
providers use different modes of the same runtime, its entry reports their
common guarantees. A session query selects only its persisted mode.

Routing requires both the output publisher and the running router sink. Route
configuration alone does not enable it. Reply support requires the installed
reply service. Interrupt requires the actual adapter's `cancel_turn`
advertisement AND Tether's installed interrupt path; a session interface or
lifecycle stop is insufficient. Missing wiring reports false. Question and
approval kinds remain unavailable until their runtime-specific detector and
publication paths are wired. The global publisher kinds are intersected with
`RoutingRuntimeKinds(runtimeID)`; without that hook only final/failure can
be available.

`final_text_confidence` accepts `exact`, `heuristic`, `none`, and `unknown`.
Before the output publisher is wired it is `unknown`. The runtime resolver
owns the baseline claim: supported Claude, Codex, Antigravity and OpenCode run
modes produce exact final text; ACP is heuristic; OpenCode serve has none.
Unsupported modes remain unknown. **Per-output confidence is authoritative**;
the capability response does not replace the actual turn's confidence.
Delivery is `next-turn`.

MCP consumers use `tether_channel_list` (bounded `limit`/`offset` pages),
`tether_channel_read` (`since`/`limit` or `last`), and `tether_routing_get`
(optional `session_id`). These are read-only calls through the same consumer
services. Channel names are stable handles and responses retain derived
addresses, publication cursors and purge tombstones. SSE subscription remains
an HTTP surface. The client library's `SubscribeChannel` does not reconnect
automatically: resume explicitly with the last received `Seq`.

## Named channels

Channels are named public topics, independent of groups and private mailboxes.
The stable consumer handle is a case-sensitive name: 1–64 ASCII characters,
starting with a letter or digit, followed by letters, digits, `.`, `_` or `-`.
The derived address is `msg://service/local/channel/<name>`. This uses the
released go-messaging service kind; the name remains stable if a dedicated
channel address kind is introduced later. Responses carry both `name` and
`address`. Paths are keyed by name.

A successful publication creates the channel implicitly. Publish through
`POST /messages` with `to` set to its derived address; `channel` is filled from
the name when absent, and a conflicting label is rejected. Existing private
mailbox messages with a `channel` label do **not** appear in channel discovery,
history or subscriptions, even when their label matches a public topic.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/channels?as=<caller-urn>` | `GET` | List published channels, sorted by name. Returns `{channels: [{name, address}]}`. |
| `/channels/{name}/messages?as=<caller-urn>&since=0&limit=100` | `GET` | Non-destructive history in publication order. Returns `{name, address, messages: [{seq, ...envelope}], next_since}`. `since` is exclusive, defaults to 0; `limit` defaults to 100, maximum 1000. Continue with `since=next_since`. |
| `/channels/{name}/subscribe?as=<caller-urn>&since=<seq>` | `GET` | SSE replay strictly after `since`, followed by live publications. Omit `since` for live-only; `since=0` replays all history. Reconnect uses the greater of `since` and `Last-Event-ID`. Live-only streams send an initial `id` at the captured high-water mark. |

No membership is required to read or subscribe. Any valid caller URN may be
asserted using `as`; it need not equal the channel address or sender. A
middleware-verified bearer principal supplies caller identity directly, so
`as` may be omitted for that caller. The shared channel service exposes
identity-based authorization hooks for list/history/subscribe/publish; the
current default observes identity without adding scope enforcement or grants.
Daemon identity mode still controls authentication (ADR 0045 / 0253).
Publication `from` attribution retains the existing observe-mode convention.

Unknown valid names have empty history and can be subscribed to before their
first publication; they appear in discovery after the first publication.

```json
{"kind":"notice","from":"msg://session/local/s123","to":"msg://service/local/channel/ops","payload":{"text":"Ready for review"}}
```

SSE events include a durable insertion-sequence cursor independent of timestamps
and message-ID generation order:

```text
id: 42
event: message
data: {"seq":42,"id":"...","kind":"notice","channel":"ops","from":"msg://session/local/s123","to":"msg://service/local/channel/ops",...}
```

Subscriptions read committed history in bounded batches, with backpressure and
up to 250 ms polling latency when caught up. A `: ping` comment is sent every
15 seconds. Reconnect using the last received `id`; replay crosses restarts
without consuming messages or acknowledging deliveries. Negative/malformed
cursors return 400 `invalid_request`. Subscription cursors beyond the current
high-water mark are clamped to it so EventSource can continue reconnecting. Use 0 to replay a never-published channel.
Sequence numbers are global and can have gaps within a channel.
To load recent messages directly, use `/channels/{name}/messages?last=N`
(with the caller identity as usual). `N` is 1–1000; the latest N publications
are returned oldest first. `last` cannot be combined with `since` or `limit`.

Channel envelopes use the existing **messaging retention policy**: structural
rows and replay sequences remain indefinitely; bodies and metadata may be
removed only through the explicit audited `/messages/{id}/purge` action.
Publications have no per-recipient delivery obligation and are therefore
purge-eligible without subscriber acknowledgments. A purge preserves the
message, channel name, address and sequence and writes a durable author receipt.
History and SSE replay expose `purged: true` and `purged_at` from that receipt,
so an empty published payload remains distinguishable from removed content.
Mailbox inbox/list/subscribe, consume, cancel, read/archive and redrive actions
reject channel publications with 400 `channel_not_mailbox`; notify to a channel
address is rejected before publication. The shared atomic publication seam
passes both the caller principal and envelope `From` to the authorization hook,
including dispatcher and daemon-internal sends. A nil hook remains observe mode.
The automatic event age policy does not purge channel messages. Replies, launch
routing configuration and consumer UIs are separate surfaces/workstreams.


## Messages

`/messages/*` is the durable go-messaging mailbox surface. `POST /messages`
stores an envelope only. `POST /messages/notify` stores the same envelope and
then best-effort wake-injects a live session with a mailbox reminder turn.

Message kinds remain semantic: `request`, `response`, `notice`,
`status_update`, `handoff`, `escalation`. Delivery urgency is separate and is
stored in message metadata as `urgency=very-low|low|normal|high`, following the
RFC 8030 Web Push vocabulary.

| Route | Method | Behavior |
|---|---|---|
| `/messages` | `POST` | Store an envelope. Body: `{"from":"msg://...","to":"msg://...","kind":"notice","payload":{...}}`. |
| `/messages/notify` | `POST` | Store an envelope, count unread messages for `to`, and wake a live recipient session when resolvable. |
| `/messages/{id}` | `GET` | Fetch one message. |
| `/messages/inbox?to=<urn>` | `GET` | Agent pull model. Destructive: returned messages are marked delivered. With `as_session=<id>` naming a live session that is the recipient itself (the session address, or a session of the recipient actor), they are also consumed, settling their deliveries. Any other caller gets the listing only. |
| `/messages/list?to=<urn>` | `GET` | Operator/UI model. Non-destructive; supports `unread_only`, `include_archived`, `limit`, `offset`. |
| `/messages/{id}/read?as=<urn>` | `POST` | Mark read. |
| `/messages/{id}/archive?as=<urn>` | `POST` | Archive for recipient. |
| `/messages/{id}/consume?as=<urn>` | `POST` | Mark consumed. |
| `/messages/{id}/reply?as=<urn>` | `POST` | Reply to the session that sent a routed (channel) message: queued, then injected as that session's next turn. See [Replies to routed messages](#replies-to-routed-messages). |
| `/messages/{reply_id}/delivery` | `GET` | Where a reply stands: queued, delivering, delivered or undeliverable, and why. |

Notify body extends the normal message envelope with:

```json
{
  "urgency": "normal",
  "session_id": "optional-explicit-live-session",
  "wake": true,
  "wake_text": "optional override"
}
```

If `session_id` is omitted, the daemon resolves `msg://session/<auth>/<id>` to
that live session, or `msg://agent/<auth>/<logical_agent_id>` to the newest
running session for that logical agent. Offline recipients still receive the
durable message; the response includes `wake_attempted`, `wake_delivered`, and
`wake_error`. An agent recipient whose binding names a session that is not
running gets `wake_reason: "session-not-running"` and no wake.

`wake_delivered: true` means the reminder turn was submitted, not that the
message was read. The delivery then stays leased at `turn_submitted` for up to
15 minutes, waiting for the recipient to consume the message. Any of these
closes it as `delivered`: `POST /messages/{id}/consume`, the recipient's own
inbox pull (the MCP `tether_message_inbox` tool passes its session), or the next
attempt finding the message consumed or read. If none happens within the
window, the delivery is retried, which wakes the recipient again as a
reminder. A wake that was not submitted (busy, offline, submit failed) is
retried after a short backoff, as before. While the lease is held, another
claimant of that delivery (a bridge's `POST /messages/{id}/claim`) gets 409
`conflict` ("delivery already claimed") until it ends.

#### `wake_reason` values

When no wake was delivered for a reason that is not an error, `wake_reason`
says which. The message is stored in every case. Its delivery is retried by
the daemon's wake sweep, except `already-handled`, which is settled.
go-tether-client mirrors these as `WakeReason*` constants.

| `wake_reason` | Meaning |
|---|---|
| `busy` | The session was mid-turn. Retried after a short backoff. |
| `offline` | No live session to wake. |
| `offline-race` | The session stopped between resolution and the wake. |
| `stale-generation` | The actor moved to a newer session while the wake was in flight. |
| `claim-unavailable` | Another attempt already holds the delivery; this one stood down rather than wake twice. |
| `marker-write-failed` | The daemon could not record the attempt's bookkeeping and released the delivery for retry. |
| `already-handled` | The recipient had already consumed or read the message, so it was settled without a wake. Comes from retries rather than a fresh notify. |
| `settle-failed` | Settling an already-handled message failed; it is retried. |
| `session-not-running` | The agent recipient's binding names a session that is not running. Nothing is woken until a live session owns the address. |

A wake that was attempted and failed is reported in `wake_error` instead,
which carries the error's text (for example, submitting the turn failed).

### Replies to routed messages

A session whose output is routed to a [named channel](#named-channels) is the
sender of those messages (`msg://session/local/<id>`). A reply to one of them
is delivered to **that session, as its next turn**, not to a mailbox:

```
POST /messages/{id}/reply?as=msg://user/local/chris
Idempotency-Key: <optional>
{"body": "use the second option", "interrupt": false}
```

`202 Accepted`:

```json
{"reply_id": "…", "parent_id": "{id}", "state": "queued", "target_session_id": "<session>",
 "interrupt": "cancelled", "duplicate": false}
```

`POST /messages` with `in_reply_to` naming a routed message does the same. It
answers like any send, `201` with the stored reply envelope (its `id` is the
`reply_id`), plus a `routing_reply` field carrying the receipt above, so clients
that read a send response (the MCP `tether_message_send` tool, `tether message
send`) need no change. `to`, if given, must be the sender session; the text is
the payload's string, or its `body`/`text`/`message`; interrupting needs
`POST /messages/{id}/reply`.

`POST /messages/notify` with such an `in_reply_to` is routed the same way (the
mailbox wake would inject a generic reminder, not the reply text, and leave a
mailbox copy): it answers `201` in notify's shape with the stored reply as
`message`, `wake_attempted: false`, and the receipt as `routing_reply`; `wake`,
`wake_text` and `urgency` are ignored. `in_reply_to` on any other message is
unchanged. "Routed" means a message a session published to a channel; a mailbox
message that merely has a session as its sender is answered through the mailbox.

The `as` identity is the verified principal when one is present, else the
self-asserted `?as=` (or the envelope's `from`), as for channels; observe mode
records it and refuses nothing.

- **Single writer.** Tether queues the reply and injects its body with the same
  path as `POST /sessions/{id}/turn` at the session's idle boundary: when its
  current turn completes, fails or is interrupted (not on a timer), or at once if
  it is idle. Consumers never submit the turn themselves. One reply is injected
  per idle boundary, in arrival order, except that an interrupting reply goes
  ahead of the others queued for that session. Only the head of a session's queue
  is delivered: a younger reply that is due does not jump an older one that is
  waiting out a retry.
- **At most once.** A reply is injected at most once into a session. It is
  retried (up to five attempts, with backoff) only when the runtime itself says
  it took no turn: it rejected the submission, the process could not start or be
  sandboxed, the CLI had no login (`provider_not_authenticated`) or the session
  it was asked to resume was gone (`provider_session_lost`). Those outrank
  everything else, because a CLI that was launched and then refused the turn
  still shows activity on Tether's turn feed and the model never saw the reply.
  Every other failure after the submission is reported and never repeated.
  Tether's turn feed counts any turn activity, including the terminal a
  subprocess runtime's adapter synthesizes on **every** process exit, so a
  subprocess runtime (`codex exec`, `claude -p`, `opencode run`, `agy`) whose
  process exits non-zero, even without printing anything, makes the reply
  `delivered` with `reason: "turn_failed"` after one attempt, and it is not
  retried. Tether cannot tell whether such a process read the reply before it
  died, and repeating a reply the model may have acted on is the worse error.
  Streaming and JSON-RPC runtimes report a failed turn on the session's
  `session.turn_output`. The one case Tether cannot know is a daemon that stops
  while a reply is being injected: see "A daemon restart".
- **Runtimes that reject mid-turn input** (OpenCode, ACP) simply wait for the turn
  to end. A rejection is not a failed attempt.
- **A runtime with no turn lifecycle** (a PTY) never says when it is idle, so a
  reply to a running one is refused at accept with 409 `turn_feed_unavailable`,
  and a reply that reaches one later (a hand-off) becomes `undeliverable` with
  `no_turn_feed`. Nothing is queued behind a boundary that cannot come.
- **`interrupt: true`** reserves the reply (a `pending` row that holds its
  idempotency key and its priority), holds the session's queue, cancels the
  session's open turn and waits for it to end (the caller is recorded as the
  interrupting actor), and then queues the reply ahead of every older one: the
  boundary the cancel creates is the interrupting reply's, not an older reply's.
  If the runtime cannot cancel a turn the reply is refused with 409
  `interrupt_unsupported`; if the turn was submitted but not yet started, 409
  `turn_not_yet_started`. A refused reply leaves nothing behind and its key is
  free again. If there was nothing to cancel (`no_turn_in_progress`,
  `turn_superseded`, `session_not_running`) the reply is accepted as an ordinary
  next-turn delivery and the receipt's `interrupt` says which. If the cancel was
  requested but the turn did not end within the daemon's bound
  (`interrupt_timeout`), the reply is accepted too, so a retry cannot queue a
  duplicate, and is delivered when the turn does end. A retry with the same
  `Idempotency-Key` returns the earlier reply (`duplicate: true`) and does not
  cancel again, including after the client disconnected mid-cancel.
- **An ended session** hands its queued replies to the session its actor is
  currently bound to (the registry binding, never a "newest running" guess), and
  the delivery reads `delivered` with `reason: "handed_off"` and
  `delivered_to_session_id` naming the successor. With no usable binding the
  reply is `undeliverable`; it is kept and stays readable, never dropped.
- **A daemon restart** never re-sends a reply that was being injected into a
  session that is still running: it becomes `undeliverable` with
  `daemon_restarted_during_delivery`. An interrupting reply that was still being
  reserved becomes `undeliverable` with `interrupt_unconfirmed`: its caller never
  got a receipt and nothing says whether the cancel happened, so resubmit it
  **with a new `Idempotency-Key`**: the old key still names that undeliverable
  reply, and resubmitting with it answers 202 `duplicate: true` with
  `state: "undeliverable"` and delivers nothing. The same resolution runs on the
  daemon's repair sweep, not only at startup.
- **A reply is not a mailbox item.** Cancel, consume, read, archive, claim, ack,
  nack, redrive and delete aimed at one are refused with 400 `reply_not_mailbox`
  and change nothing; read its state from `/delivery`.
- **Known limitation: a reply retried after a lost provider session lands in a
  fresh provider session.** `provider_session_lost` means the resume id the
  session named is dead. The reply did not run, so it is retried, and the retry
  starts a new provider conversation (the second submission carries no
  `--resume`): the receiving agent may not have the question the reply answers.
  The consumer sees plain `delivered` (`attempts: 2`, no `reason`), with nothing
  marking the new conversation. A reply that depends on the question it answers
  should restate it.

`GET /messages/{reply_id}/delivery` returns `{reply_id, parent_id, state, reason?,
detail?, original_session_id, target_session_id, delivered_to_session_id?,
interrupt_requested, attempts, next_attempt_at?, created_at, updated_at,
settled_at?}`. `state` is `queued`, `delivering`, `delivered` or `undeliverable`;
`pending` is seen only while an interrupting reply's cancel is in flight (or after
a crash, until the sweep settles it). `detail` is one line of at most 256 bytes.
It never carries anything a runtime process printed: a process failure is
reported as its exit code or signal only, because a CLI can echo the reply text on
its stderr, and `GET /delivery` and the events are readable more widely than the
reply is.
The same outcome is published as `routing.reply_delivered` /
`routing.reply_undeliverable` on the session's event stream, without the reply
text.

| `reason` | State | Meaning |
|---|---|---|
| `handed_off` | `delivered` | The originating session had ended; the reply went to the session its actor is bound to. |
| `turn_failed` | `delivered` | The reply was injected and its turn ran, then failed (a subprocess runtime returns that as the submit error, including a process that exited without printing anything); not retried. `detail` says how the process ended (exit code or signal), nothing else. |
| `session_ended_no_binding` | `undeliverable` | The session ended and its actor has no current binding (or the reply has no actor). |
| `bound_session_not_running` | `undeliverable` | The binding names a session that is not running. |
| `pull_only_binding` | `undeliverable` | The actor is a published-local bridge; Tether cannot inject a turn into it. |
| `resolve_failed` | `undeliverable` | The binding lookup kept failing. |
| `submit_failed` | `undeliverable` | The runtime did not take the turn five times (rejected, would not start, no login, resume target gone); `detail` carries the last error, or the exit code or signal if a process ended. While retrying the state is `queued` with this reason. |
| `no_turn_feed` | `undeliverable` | The session's runtime reports no turn lifecycle (a PTY). |
| `daemon_restarted_during_delivery` | `undeliverable` | See above. |
| `interrupt_unconfirmed` | `undeliverable` | See above. |
| `body_purged` | `undeliverable` | The reply text was purged before it could be delivered. |
| `waiting_for_idle` | `queued` | The runtime rejected mid-turn input; waiting for the turn to end. |

An unknown message id is `404 not_found`; a message that is not a channel
publication from a session is 400 `reply_target_not_a_session`. With the reply
path not running (the dispatcher failed to start) the answer is 501
`not_implemented`, and that includes `POST /messages` and `POST /messages/notify`
with `in_reply_to` naming a routed message: they do not fall back to the mailbox.
The reply text is at most 128 KiB and the request body at most 1 MiB, both 413
`payload_too_large`; an unknown JSON field is 400, so a misspelled `interrupt` is
not silently a plain reply. A PTY is not delivered to by idle detection: see above.

---

## Registry

The federation directory service. Tether owns public-identity rows for
agents + projects; substrates retain operational config behind each row's
`callback` URI. Cross-substrate dedup is driven by substrate-local
`external_id` attachments and `LookupBy(kind, external_id, substrate?)`.
See [ADR 0041](../adr/0041-registry-directory-service.md),
[ADR 0043](../adr/0043-cross-substrate-dedup.md), and
[docs/registry/overview.md](../registry/overview.md).

The `{kind}` URL segment is **plural** (`agents`, `projects`); the
internal `Kind` value is singular (`agent`, `project`).

### `POST /registry/{kind}` — Register

Body: a Profile JSON. The caller supplies `display_name` and any optional
identity fields. The server assigns `urn`, `kind`, `created_at`,
`updated_at`, `tether_instance_id` and ignores any caller-supplied versions
of those.

Response: `201 Created` + the canonical Profile JSON (with the minted
URN).

```bash
curl -X POST http://unix/registry/agents -d @profile.json
```

### `GET /registry/{kind}` — Search

Query parameters (all optional, combine with AND):
- `role` — exact match on the `role` column
- `title` — exact match on `title`
- `project` — exact match on `project`
- `capability` — row has the capability in its `registry_capabilities`
- `skill_name` — row has a skill with this name
- `status` — `active` (default) | `deprecated` | `*` (all)
- `external_id` — resolve one substrate-local identifier instead of list-search
- `substrate` — optional scope for `external_id` lookup

Response: `{"<kind-plural>": [Profile, ...]}` — alphabetical by
`display_name`. Empty result is `{"agents": []}`, never null.

When `external_id` is present, the route changes semantics from list-search to
dedup lookup. Response shape becomes `{"<kind-singular>": Profile}` and a
miss returns `404 not_found`.

```bash
curl 'http://unix/registry/agents?role=reviewer&status=active'
```

### `GET /registry/{kind}/{urn}` — Lookup

`{urn}` is URL-encoded (`msg%3A%2F%2Fagent%2Ftether%2Fagt_xxx`).

Response: `200 OK` + Profile, or `404 not_found`. Soft-deleted rows are
still returned here with `status: "deprecated"`.

### `PATCH /registry/{kind}/{urn}` — UpdateSelf

Body: `UpdatePatch` JSON. Partial-merge semantics:
- Scalar fields use pointer-nil semantics (`null` or omitted = no change).
- Array fields (`capabilities`, `skills`, `links`) accept two shapes:
  - Shorthand: `"capabilities": ["a", "b"]` — equivalent to a REPLACE.
  - Explicit: `"capabilities": {"mode": "append"|"replace"|"remove", "value": [...]}`.
- Empty `value` is a no-op on every mode.
- `last_updated_by` is required.

```json
{
  "title": "Tether Sprint Implementer",
  "skills": {"mode": "append", "value": [{"name": "go-generics", "learned_at": "2026-05-20T00:00:00Z"}]},
  "last_updated_by": "operator@example"
}
```

### `DELETE /registry/{kind}/{urn}` — Deregister

Soft-delete; row's `status` flips to `deprecated`. Child rows
(capabilities/skills/links) are NOT touched. Response: `200 OK` + the
now-deprecated Profile.

### `POST /registry/{kind}/{urn}/sync` — Sync

Refreshes thin-profile columns from the row's `callback`. Raw payload is
never stored (D18). `file://` and `cli://` schemes ship in v1; `http://`
and `mcp://` land in v060-02.

- `204 No Content` if the row has no `callback`
- `200 OK` + refreshed Profile otherwise

### `POST /registry/{kind}/{urn}/merge` — Merge

Admin cleanup path for residual duplicates. Body:

```json
{"into": "msg://agent/agent-mux/prj_xxxxxxxxxx"}
```

Moves external IDs and union-shaped metadata from `{urn}` into the destination
URN, marks the source row `status: "merged"`, and sets `merged_into` on the
source tombstone. Response: `200 OK` + the canonical destination Profile.

### `POST /registry/bootstrap?force=true&substrate=...` — Re-run a bootstrap importer

Daemon already runs `BootstrapFromCatalog(force=false)` once at startup.
This endpoint lets operators apply catalog drift after editing a YAML by
re-running with `force=true` (refreshes existing rows from the source
YAML's current state). `substrate=tether` (default) re-runs the Tether catalog
importer; `substrate=cerberus` re-runs the Cerberus index importer.
`write_back=false` disables `registry_urn` write-back for the current run.
Body is empty; response is a `BootstrapReport`:

```json
{"imported": 0, "skipped": 30, "refreshed": 2, "errors": []}
```

### Error mapping

| Error | HTTP | Code |
|---|---|---|
| `registry.ErrInvalidRequest` | 400 | `invalid_request` |
| `registry.ErrNotFound` | 404 | `not_found` |
| `registry.ErrNoCallback` | 204 | (no body) |
| `registry.ErrNoResolver` | 400 | `invalid_request` |
| `registry.ErrPayloadInvalid` / `ErrPayloadTooLarge` / `ErrPathOutsideRoot` | 502 | `internal_error` |
| `registry.ErrMintExhausted` | 503 | `internal_error` |
| unsupported kind segment | 404 | `not_found` |
| method not allowed | 405 | `method_not_allowed` |
| other | 500 | `internal_error` |

---

## Identity, session bootstrap & runtime bindings

Added by the messaging vNext epic. These separate *who an actor durably is*
from *which live session currently receives its mail*. See
[ADR 0045](../adr/0045-messaging-principal-trust-model.md) for the same-host
trust model and [messaging-adoption.md](../messaging-adoption.md) for the
adoption walkthrough.

> **Casing is not uniform across this group.** Request bodies are `snake_case`.
> `/whoami` responds in `snake_case`, but the binding routes marshal
> `registry.RuntimeBinding` and `registry.ScopedBinding` directly — those
> structs carry no JSON tags, so their responses come back **`PascalCase`**
> (`ID`, `TargetURN`, `LeaseExpiresAt`, …). Decode them into the Go types
> rather than hand-written snake_case structs.

### `GET /whoami` — Self-discovery

Query: `as` (required) — the `msg://` URN to look up, self-asserted and
unverified.

Every field is independently best-effort; an unregistered or never-bound
identity is a normal `200`, not an error.

```json
{
  "urn": "msg://agent/agent-mux/agt_x9k2p4qrst",
  "profile": { "...": "redacted Profile; omitted when unregistered" },
  "external_ids": [ { "substrate": "tether", "external_id": "...", "attached_at": "..." } ],
  "groups": [ { "...": "redacted group Profiles" } ],
  "binding": { "...": "RuntimeBinding (PascalCase); omitted when unbound" }
}
```

| Condition | Status | Code |
|---|---|---|
| `as` missing | 400 | `invalid_request` |
| method other than GET | 405 | `method_not_allowed` |

### `POST /sessions/bootstrap` — Resolve a session's canonical identity

The launch-boundary helper. **Idempotent** — a repeated call for the same
`session_id` returns the existing identity rather than minting a competitor.

```json
{
  "session_id": "sess-1",
  "intent": "...",
  "parent_session_id": "...",
  "logical_agent_id": "...",
  "publication": "...",
  "provider_mappings": [ { "owner": "...", "provider": "...", "native_session_id": "..." } ]
}
```

Response: `{"session_id": "...", "created": true}` — `created` distinguishes a
fresh mint from an idempotent replay.

| Condition | Status | Code |
|---|---|---|
| malformed body | 400 | `invalid_request` |
| `session_id` missing | 400 | `invalid_request` |
| method other than POST | 405 | `method_not_allowed` |

### Runtime bindings and session lifetime

Launching a session for a logical agent leases a binding for
`msg://agent/<authority>/<logical_agent_id>`, one generation above the
previous one. The current binding is the highest generation that is neither
revoked nor lease-expired. A binding never outlives its session:

- When a session ends (stopped, exited on its own, or crashed), the daemon
  revokes every binding it holds, current or superseded.
- On startup, the daemon revokes the bindings of every session that has ended,
  including those its sweep has just marked `failed`. Bindings whose
  `session_id` is not a Tether session, such as a published-local bridge's,
  are left alone.

A superseded generation stays while its session runs. So when two sessions
share an agent and the newer one stops, the actor goes back to the older one
if it is still running, and is unbound if it is not. It never goes to a
session that has ended. When the current binding still names a session that
is not running (it died without the daemon seeing it), notify does not
reroute: it stores the message and answers `wake_reason:
"session-not-running"`.

### `POST /registry/bindings` — Lease a runtime binding

```json
{
  "target_urn": "msg://agent/agent-mux/agt_x9k2p4qrst",
  "session_id": "sess-1",
  "host_id": "host-1",
  "attempt_id": "attempt-1",
  "capabilities": ["pull-only"],
  "ttl_seconds": 3600
}
```

`target_urn`, `session_id`, `host_id` and `attempt_id` are required.
`capabilities` must be **exactly** `["pull-only"]` — caller-supplied-webhook
push bridging is not implemented, so the bridge pulls its own mailbox.
`ttl_seconds` of 0 or omitted means no expiry. Visibility is always minted
`published-local`; a caller-declared visibility is never accepted.

Response: `201 Created` + the `RuntimeBinding` (PascalCase), carrying `ID`,
`Generation`, `Visibility` and `LeaseExpiresAt`.

| Condition | Status | Code |
|---|---|---|
| malformed body, missing required field, or capabilities not exactly `["pull-only"]` | 400 | `invalid_request` |
| target is bound to a Tether-managed session this endpoint cannot supersede | 409 | `conflict` |
| registry not configured | 404 | `not_found` |

### `GET /registry/bindings` — List bindings for a target

Query: `target_urn` (required), `current=true` (optional) to return only the
active binding.

Response: `{"bindings": [ RuntimeBinding, … ]}`, newest generation first.

> **No ownership check.** Any same-host caller can list any target's bindings
> (CW-20260907-0034).

| Condition | Status | Code |
|---|---|---|
| `target_urn` missing | 400 | `invalid_request` |

### `POST /registry/bindings/{id}/renew` — Extend a lease

Body is optional: `{"ttl_seconds": 3600}`. Response `200` + the refreshed
`RuntimeBinding`.

| Condition | Status | Code |
|---|---|---|
| unknown binding id | 404 | `not_found` |
| a newer generation exists for this target | 409 | `conflict` |
| method other than POST | 405 | `method_not_allowed` |

A `conflict` here means the caller has been fenced out by a newer generation —
an expected outcome of concurrent-actor-session handling, not a server fault.
Stop rather than retry.

### `POST /registry/bindings/{id}/revoke` — Relinquish a lease

No body. Response **`204 No Content`**. Idempotent.

| Condition | Status | Code |
|---|---|---|
| unknown binding id | 404 | `not_found` |
| method other than POST | 405 | `method_not_allowed` |

An unrecognized action segment returns `404 not_found` naming the action.

### `POST /registry/scoped-bindings` — Publish a role/slot revision

```json
{
  "scope": "run-42",
  "slot": "reviewer",
  "target_urns": ["msg://agent/agent-mux/agt_x9k2p4qrst"],
  "relationship": { "any": "json" },
  "created_by": "msg://agent/agent-mux/agt_other"
}
```

`scope` and `slot` are required. `relationship` is opaque JSON passed through
untouched — **note this field is HTTP-only; the `tether_registry_scoped_binding_set`
MCP tool does not expose it.**

Tether does not interpret scope or slot names, and a binding confers no command
authority. Response: `201 Created` + the `ScopedBinding` (PascalCase), carrying
its `Revision`.

| Condition | Status | Code |
|---|---|---|
| malformed body, or `scope`/`slot` missing | 400 | `invalid_request` |
| method other than POST | 405 | `method_not_allowed` |

### `GET /registry/scoped-bindings/resolve` — Resolve a slot

Query: `scope`, `slot`, and optional `single=true`.

With `single=true` the response is `{"target_urn": "...", "binding": {…}}` and
zero-or-several targets is a `409 conflict` rather than a guess. Without it,
every target is returned.

### `GET /registry/scoped-bindings/revisions` — Revision history

Query: `scope`, `slot`. Response: `{"revisions": [ ScopedBinding, … ]}`,
newest first.

### Other routes not yet given full entries

Reachable and stable, but documented here only in summary:

| Route | Method | Description |
|---|---|---|
| `/messages/retention/candidates` | `GET` | Messages eligible for privacy-safe body purge. Optional `older_than_hours`. |
| `/session-groups`, `/session-groups/{id}` | `GET`, `POST` | Session-group membership surface. |
| `/broker/requests` | `POST` | Legacy broker envelope intake, retained for pre-vNext consumers. |
| `/logs/daemon` | `GET` | Tail the daemon log. |

---

## Groups

Group messaging — a `group` registry kind with mailbox-pull delivery
semantics, per-member read cursor, and server-side `@` mention parsing.
See `docs/adr/0042-group-messaging.md` for the architectural decision
and `docs/groups/symbols.md` for the `@` / `!` / `:` vocabulary.

| Route | Method | Description |
|-------|--------|-------------|
| `/groups` | `POST` | Create a group. Body: `{display_name, description?, role?, capabilities?, avatar?, last_updated_by}`. The caller URN goes in `last_updated_by`; it's auto-added to `group_members` with `role='owner'` inside the same transaction as the profile insert. Returns the reloaded `Profile`. |
| `/groups?member=<urn>` | `GET` | List groups that `<urn>` belongs to, ordered alphabetically by `display_name`. Status is not filtered (archived groups appear). |
| `/groups/{urn}` | `GET` | Read a group profile by URN. URN is path-escaped (e.g. `msg%3A%2F%2Fgroup%2Ftether%2Fgrp_x9k2p4`). |
| `/groups/{urn}` | `DELETE` | Archive a group (soft — sets `status='archived'`, read-only). Body: `{"by": "<urn>"}` (or `?as=<urn>`). Owner/moderator only. |
| `/groups/{urn}/members` | `POST` | Add a member. Body: `{"member": "<urn>", "by": "<urn>", "role"?: "member|moderator|owner"}`. Role defaults to `member`. Owner/moderator only. |
| `/groups/{urn}/members` | `GET` | List members of a group, ordered by `joined_at` ASC, with `display_name` hydrated. |
| `/groups/{urn}/members/{member_urn}` | `DELETE` | Remove a member. Body: `{"by": "<urn>"}` (or `?as=<urn>`). Owner/moderator only; cannot remove the owner. |
| `/groups/{urn}/members/{member_urn}` | `PATCH` | Change a member's role. Body: `{"role": "...", "by": "<urn>"}`. Promotion to `owner` is owner-only. |
| `/groups/{urn}/leave` | `POST` | Self-leave path. Body: `{"member": "<urn>"}`. If the leaver is the owner and no other owner/moderator exists, the leave is refused. |
| `/groups/{urn}/messages` | `POST` | Send a message to the group. Body: `{"from": "<urn>", "kind": "...", "payload"?: ..., "thread_id"?: "...", "content_type"?: "..."}`. Returns `{message_id, group_seq}`. Non-member → 403; archived group → 423 Locked. Mention parser runs server-side: ambiguous `@`-short-form → 400 `invalid_request` with the `candidates` array. |
| `/groups/{urn}/messages?since_seq=N&thread_id=...&limit=N&as=<urn>` | `GET` | List messages addressed to the group with `group_seq > since_seq`. `as` is the requesting-member URN; required for membership + joined_at gate. `since_seq=0` defaults to the member's `last_read_seq`. Does NOT bump the cursor. |
| `/groups/{urn}/read` | `POST` | Mark messages read. Body: `{"up_to_seq": N, "as": "<urn>"}`. Monotonic — smaller `up_to_seq` is a no-op. |
| `/mentions?as=<urn>&since=<ts>&limit=N` | `GET` | List the caller's mention notices (notice envelopes with `payload.group` set), newest first. |

### Caller identity (v1)

Until v060-03 token auth lands, the caller URN is supplied per-request via
one of these channels (the handler picks whichever fits the verb's
semantic — body wins when both body and query are supplied):

- **Body fields:**
  - `last_updated_by` on `POST /groups` (creator URN, becomes owner).
  - `by` on moderation actions (archive / add-member / remove-member /
    set-role) — the actor.
  - `member` on `POST /groups/{urn}/leave` — the self-leaver.
  - `from` on `POST /groups/{urn}/messages` — the message author.
  - `as` on `POST /groups/{urn}/read` — the member whose cursor moves.
- **Query fallback:** `?as=<urn>` on GET / DELETE verbs where building a
  body is awkward (the `tether` CLI uses this).

The HTTP layer does not authenticate the URN — same-host UDS trust per
ADR-0041 §D7.

### Symbol vocabulary

The daemon parses `@` mentions in `POST /groups/{urn}/messages` payloads
and emits notice envelopes to mentioned URNs' personal inboxes. `!` and
`:` are reserved-namespace for agent-side handling — the daemon
transports them verbatim. See `docs/groups/symbols.md` for the full
reference; `tether_group_post` MCP tool description embeds the same
distinction inline so agent authors see it at the tool level.

### Error mapping

| Sentinel | HTTP | Envelope code |
|----------|------|---------------|
| `registry.ErrInvalidRequest` | 400 | `invalid_request` |
| `*registry.ErrAmbiguousMention` | 400 | `invalid_request` (with `candidates` array in the response payload) |
| `registry.ErrForbidden` | 403 | `forbidden` |
| `registry.ErrNotFound` | 404 | `not_found` |
| `registry.ErrGroupArchived` | 423 | `locked` |
| method not allowed | 405 | `method_not_allowed` |
| other | 500 | `internal_error` |

### Known v1 limitations

- Display-name ambiguity blocks the `@<short-form>` mention path. If
  two agents share `display_name`, `SendToGroup` aborts with 400 + the
  candidate URN list; the caller must use the full URN.
- Group URNs are not yet recognized by go-messaging v0.2.1's
  `AddressKind` enum, so the existing `/messages/*` routes reject them
  at `ParseURN`. Group reads go through `/groups/{urn}/messages`.
  Cross-substrate group routing via the ADR-0040 federation Router
  works structurally (3-segment URN preserves the authority segment)
  but waits on a go-messaging bump for end-to-end correctness.
- v1 has no private-membership model — member lists are visible to all
  members.
- No event emission on group writes — consumers re-pull on cache miss
  (same scope-fence as the registry's other surfaces).

---

## Workstreams, refs & digests

The durable container for work that outlives a session, what each session
touched, and the assembled recovery view. Full reference, including what an
empty digest does and does not mean, is in
[`docs/workstreams.md`](../workstreams.md) — this is the endpoint index.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/workstreams` | Create a container. Empty body is valid. |
| `GET` | `/workstreams` | List, newest first. `?status=`, `?workflow_id=`. |
| `GET` | `/workstreams?ref=<kind>:<ref_id>` | **Reverse lookup**: which workstreams touched this object. Returns every match. Not combinable with `status`/`workflow_id`. |
| `GET` | `/workstreams/{id}` | One workstream. |
| `PATCH` | `/workstreams/{id}` | Partial update of `name`, `workflow_id`, `status` (`active`/`closed`). Absent field = unchanged; `""` clears name/workflow_id. 404 for unknown id, 400 for a bad status. Returns the refreshed workstream. |
| `POST` | `/workstreams/{id}/sessions` | Assign a session (`{"session_id": "..."}`). |
| `GET` | `/workstreams/{id}/refs` | Flat ref roll-up across the container's sessions. |
| `GET` | `/workstreams/{id}/digest` | Assembled digest, rolled up across the lineage. |
| `POST` | `/sessions/{id}/workstream` | Assign, clear, or `{"ensure": true}` to create one for the lineage. |
| `GET` | `/sessions/{id}/refs` | This session's refs. |
| `POST` | `/sessions/{id}/refs` | Attach a ref. Idempotent on `(session, kind, ref_id, relation)`. |
| `GET` | `/sessions/{id}/digest` | Assembled digest for this session alone. |
| `GET` | `/sessions/{id}/workstream-namespace` | Where the workstream's scratch belongs in Tesseract workspace. `?project=` (defaults to session's project_id), `?owner=`, `?tail=` (defaults to `scratch`). |

Digest filters, shared by both grains: `kind`, `relation`, `source`, `since`
(RFC3339 **UTC**), `limit`.

Three things to know before reading a response:

- **`source` means observed, not validated.** `proxy` says the proxy saw the
  identifier go by; nothing checked that it refers to anything, and under
  ADR 0045 the daemon cannot distinguish the real proxy from another same-host
  caller. It is provenance, never authentication.
- **An absent ref is never evidence.** Read `coverage.proxy_attributable` and
  each session's `ref_attribution` before concluding a session did nothing.
  Today that count is always zero — `--extract-refs` has no config seam
  (`CW-20260912-0112`), so `source=proxy` is unreachable by construction.
- **Truncation is reported, not silent.** `coverage.limit` and
  `coverage.truncated` always appear.

## Versioning & stability

The routes documented here are stable for v0.0.2 — Nanite and Clockwork
adopt these shapes in v0.0.3 integration work. Adding new endpoints is
additive and non-breaking. Changing existing response shapes requires a
version bump at the mount-point level (not yet in scope).
