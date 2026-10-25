# Inspect budgets, policies and retention

Existing budgets govern Tether's AI gateway chat spend. They do not cap
inventory tokens or calls to an app's own model API; upstream tool results have a
separate [result cap](#tool-result-cap).

## AI cost budgets

For an installation with enabled AI providers, the catalog's `global.yaml`
can include this `ai.policy` fragment alongside its existing providers:

```yaml
ai:
  policy:
    max_cost_usd: 0.05
    usage_budget:
      max_cost_usd: 5
      window: month
      scope: total
```

`max_cost_usd` is a request planning ceiling; `usage_budget` checks durable
estimated chat spend in `ai_events`. Usage windows are `day` or `month`; scopes
are `total`, `caller` or `session`. Caller/session scopes require their
correlation IDs and are not proof of verified identity. Provider and route
policies can override the global policy. The daemon applies edits on restart;
see [AI configuration](../../dev-setup.md#ai-gateway-setup) for provider
setup and policy inheritance.

In flat mode, call `tether_ai_budgets` with `{}` to inspect mounted budgets and
spend; it requires a configured MCP token and the running AI-enabled daemon.
In search mode, hydrate and dispatch that name as shown in
[Discovery](discovery.md#search-hydrate-call).
A disabled AI gateway returns a not-found error.

## Event retention

The catalog's default is an hourly age-based sweep of `events`, `proxy_events`
and `ai_events`, retaining 90 days:

```yaml
daemon:
  events_retention:
    enabled: true
    days: 90
```

```sh
tether settings --json
```

This reads catalog configuration, not the running daemon's settings. Changes
apply on daemon restart. `enabled: false` or `days` below 1 disables the sweep.
Retention removes AI usage history used by durable budgets, so choose a window
that retains the budget history you intend to enforce. It is not a row-count
or disk-size cap.

## Tool result cap

A tool that returns hundreds of kilobytes in one result makes clients spill the
output to a file or cut it blindly. The gateway therefore caps the size of one
tool result at 32 KiB (the JSON-encoded result, the same measure as
`result_bytes` in tool-call events). This is a safety net; the real fix for a
large listing is paging in the tool that owns it (CW-20261024-0053).

What the cap does:

- It cuts only a **successful result of a tool annotated read-only**. An error
  is never cut, and neither is a write receipt or any result from a tool that
  is not annotated read-only.
- It cuts the largest array in the result at an item boundary, so the text is
  still valid JSON, with at least the first item kept. Fields next to the array
  are kept. A `structuredContent` copy is cut the same way.
- It appends one extra content block, the marker:

  ```json
  {"gateway_truncated":{"truncated":true,"path":"data.items","from":0,"returned":156,
   "total":600,"original_bytes":124189,"next_cursor":"<id>.156",
   "how_to_continue":"call tether_result_next with {\"cursor\":\"<id>.156\"}; ..."}}
  ```

- It keeps a copy of the original result in memory (ten minutes, at most 64
  results and 48 MiB, none over 8 MiB) and serves the rest with
  `tether_result_next {"cursor": "..."}`, optionally with `max_bytes`. Pages
  come from that one copy, so they never skip or repeat items even if the data
  changes. Repeating a cursor returns the same page. A cursor belongs to the
  gateway view that issued it; in search mode call `tether_result_next` through
  `tether_tool_call`. After expiry or a daemon restart the cursor is refused
  with `unknown or expired cursor`; call the tool again.
- A result with no array to cut (one big object or plain text) is delivered
  whole and noted in telemetry as skipped (`result_cap_skipped`: `no_array`,
  `not_json`). A result too large to store (over 8 MiB) or arriving when the
  store is full is still cut, with no cursor, and the marker says to narrow the
  call.

Each tool-call event records `result_original_bytes` (before the cap),
`result_capped`, and `result_cap_skipped`; `result_bytes` is what was sent.

### Changing the cap

- Daemon default: `TETHER_MCP_RESULT_CAP_BYTES` in the daemon environment, then
  restart. `0` or a negative value turns the cap off (the rollback switch).
- Per profile, in the MCP profile config:

  ```yaml
  mcp:
    profiles:
      reviewer:
        result_cap:
          default_bytes: 65536     # 0 inherits, -1 removes the cap, otherwise >= 1024
          tools:
            tether_catalog_list_agents: -1
            torque_task_list: 16384
  ```

## List tools page and stay brief

The result cap above is the safety net. The fix is at the source: tools that list
records return brief rows, a page at a time, and refuse an argument they do not
declare instead of ignoring it (CW-20261024-0053).

| Tool | Default page | Brief by default | Notes |
|---|---|---|---|
| `tether_catalog_list_agents` | 25, max 200 | yes (`prompt_bytes` reports what was left out) | `query`, `role`, `verbose` |
| `tether_registry_search` | 25, max 200 | yes | `query`, `verbose`, plus the existing exact filters |
| `tether_events_history`, `tether_session_events` | 25, max 1000 | payloads over 2 KiB are cut (`payload_truncated`, `payload_bytes`) | `full_payload=true` returns them whole |

Contract for the paged tools: sorted by a stable key; the response carries `count`
(this page), `total` (all matches), `detail` (`brief` or `full`) and, when more
remain, `next_cursor`. Pass it back as `cursor` with the same filters. A cursor is
the last key returned, so adding or removing records between calls never repeats
or skips a row. An invalid `limit` or `cursor` is an `invalid_request` naming the
field. The event tools keep their numeric `next_cursor`.

## Planned MCP policy

CW-20261001-0555 covers per-agent quotas, rate limits, approval-required tools
and daemon-side policy telemetry. It is not provided by today's discovery mode,
server grant, result cap or AI cost budget. Result-size budgets for the
inventory (CW-20261001-0554) are covered by the cap above and the discovery
response limits in [Discovery](discovery.md).
