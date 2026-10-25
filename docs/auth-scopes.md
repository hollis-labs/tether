# Tether scope vocabulary (issued tokens)

Tokens issued by Tether (`POST /auth/token`, audience `tether`) carry scopes in
one namespace, `tether.<verb>`. Device capabilities map 1:1; legacy worker/MCP child scopes and event
publication are carved out as narrow scopes.

| Canonical | Legacy name | Covers | Held implicitly by |
| --- | --- | --- | --- |
| `tether.read` | read | read routes | - |
| `tether.operate` | operate | operate-level routes (broker, session groups, checkpoint, chat send, ...) | - |
| `tether.terminal` | terminal | session log/attach/input/resize | - |
| `tether.maintain` | maintain | maintain routes | - |
| `tether.admin` | admin | device/deny-list administration | - |
| `tether.worker` | worker | outpost connect | - |
| `tether.sessions` | `session.write` | create/launch/stop/turn, workstream and ref writes, logical-agent resume, session input/resize | `tether.operate` |
| `tether.messages` | `message.write` | send/notify/request, consume/cancel/read/archive/unarchive/claim/ack/nack/reply | `tether.operate` |
| `tether.groups` | `groups.write` | group create/delete/members/leave/post/read | `tether.operate` |
| `tether.teams` | `team.write` | team verbs | `tether.operate` |
| `tether.ai` | `ai.invoke` | AI chat, stream, embeddings, decide | `tether.operate` |
| `tether.events` | events | event publication | `tether.operate`, `tether.worker` |
| `tether.registry` | `registry.write` | registry and scoped-binding writes | `tether.maintain`, `tether.admin` |
| `tether.registry_read` | - | registry reads (`GET /registry/{kind}`, `GET /registry/{kind}/{id}`) | `tether.read`, `tether.registry`, `tether.maintain`, `tether.admin` |
| `tether.catalog` | `catalog.write` | catalog file writes (tools, session callers) | `tether.maintain`, `tether.admin` |

Rules:

- One direction only. A broad scope satisfies the narrow scopes it contains; a
  narrow scope never satisfies a broader one or another narrow one. `admin` does
  not contain the operate-level narrow scopes (ADR 0062: independent
  capabilities).
- A legacy name used as a *check* (for example `session.write` in an MCP tool) is
  satisfied by its canonical twin or the scope that contains it. A legacy
  *credential* gains no canonical authority.
- `mcp_scopes` accepts legacy and canonical names; the launch grant and the
  issued token carry the canonical set. `DefaultMCPScopes` (message.write,
  registry.write, groups.write) therefore yields `tether.messages`,
  `tether.registry`, `tether.groups` and nothing broader.
- A service token may hold only `tether.events`, `tether.read`, `tether.messages`,
  `tether.registry` and `tether.registry_read`. A registry-only service (enrolls and
  looks up actors, cannot send or read mail) is `tether.registry` alone: it contains
  `tether.registry_read` but not `tether.read`, which gates `/messages/list`, `/inbox`,
  `/watch` and `GET /messages/{id}`. On the local listener these scopes are enforced only in
  identity `enforce` mode; in `observe` they are attributed, not enforced.
- Event publication is grantable to service principals and worker devices. It
  grants no event reads: those still require `tether.read`. Admin alone cannot
  publish. Agent session tokens do not receive `tether.events` by default; an
  explicit launch grant may request it.
- Session input/resize need terminal-grade authority: they accept
  `tether.terminal` or `tether.sessions` (what `session.write` always covered). A
  device with `operate` alone still cannot input, as before.
- Issued tokens get this route policy on the local listener in enforce mode;
  unmapped routes are refused. Stored `tth_` credentials are unchanged.
- Not carved: `delivery.write` (message redrive/purge) remains a separate legacy
  check; it is not in `mcp_scopes` and no canonical twin exists yet.

## Basalisk workload capabilities

Exact device capabilities support current assurance reads and transport:

- `basalisk-status.assurance_read`: exchange the stored device credential for a
  direct `basalisk-status` JWT, with this single scope and a maximum 900-second
  lifetime. No session delegation, operator, wildcard or broader-scope fallback.
- `tether.workload_trust_read`: a dedicated stored device credential for
  `POST /auth/workload-trust`. The handler requires this to be its only scope;
  it accepts neither issued JWTs nor maintenance/operator authority.

- `tachyon-assurance.items_read`, `tachyon-assurance.completions_deliver`, and
  `tachyon-assurance.outcomes_read`: a separate direct workload parent may hold
  any nonempty subset of these exact scopes. It exchanges for `tachyon-assurance`
  JWTs with the requested subset (normally one scope per route), at most 900
  seconds and no session. Outcome-only callers cannot mint delivery tokens.

Status, trust-reader and Tachyon route capabilities cannot be mixed with each
other or broad Tether scopes. None grants inventory, deny-list, enrollment or
execution authority; Tachyon independently verifies completion evidence.
The remote Tether listener continues to accept only stored device credentials.
Workload JWTs are presented to their named recipient. Basalisk and Tachyon each
commission a separate trust-reader device credential. See the
[current-workload contract](api/README.md#current-workload-trust).
