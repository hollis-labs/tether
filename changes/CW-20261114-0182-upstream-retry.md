- An MCP HTTP/SSE upstream that is down or restarting when the gateway first reaches it now recovers on its own
  (CW-20261114-0182). Before, it was marked `failed` and never retried, and a refresh did not help; only a hub
  reload brought it back.
  - **Retry:** it is retried with capped exponential backoff and jitter (5 s doubling to 5 min, ±20%), without limit.
  - **States:** `failed` → `recovering` (while waiting, with `next_retry_at` and `restart_attempts`) → `starting` →
    `connected`, shown in `tether_gateway_status` and published as `mcp.upstream_state_changed`.
  - **Retry now:** a refresh (`tether_catalog_refresh`, a reload), a tool list, or a call that needs a down remote
    upstream retries it at once instead of reporting the stale failure.
  - **Inventory:** while it waits, gateway inventory counts it as unavailable, not incomplete, so a long outage does
    not hold `complete: false`.
  - **A failed tool list:** an upstream that connected but failed its tool list is no longer parked until shutdown;
    it retries too.
  - **Unchanged:** stdio upstreams keep their restart policy and limit.
