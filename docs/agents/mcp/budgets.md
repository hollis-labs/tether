# Inspect budgets, policies and retention

Existing budgets govern Tether's AI gateway chat spend. They do not cap
upstream MCP tool results, inventory tokens or calls to an app's own model API.

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
A disabled AI gateway returns a not-found error. On a fresh usage history,
this query currently fails with a SQL NULL aggregate error
(CW-20261002-0004); do not interpret that failure as zero spend or no budget.

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

## Planned MCP policy

CW-20261001-0554 covers result/inventory token budgets and an explicit truncation
and continuation envelope. CW-20261001-0555 covers per-agent quotas, rate limits,
approval-required tools and daemon-side policy telemetry. Neither is provided
by today's discovery mode, server grant or AI cost budget.
