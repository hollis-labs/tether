# Tether scope vocabulary (issued tokens)

Tokens issued by Tether (`POST /auth/token`, audience `tether`) carry scopes in
one namespace, `tether.<verb>`. The six device scopes plus `worker` map 1:1; five
legacy worker/MCP child scopes and two more are carved out as narrow scopes.

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
| `tether.registry` | `registry.write` | registry and scoped-binding writes | `tether.maintain`, `tether.admin` |
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
- Session input/resize need terminal-grade authority: they accept
  `tether.terminal` or `tether.sessions` (what `session.write` always covered). A
  device with `operate` alone still cannot input, as before.
- Issued tokens get this route policy on the local listener in enforce mode;
  unmapped routes are refused. Stored `tth_` credentials are unchanged.
- Not carved: `delivery.write` (message redrive/purge) remains a separate legacy
  check; it is not in `mcp_scopes` and no canonical twin exists yet.
