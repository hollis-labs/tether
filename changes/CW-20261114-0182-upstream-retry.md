- An MCP HTTP/SSE upstream that is down or restarting when the gateway first reaches it now recovers on its own
  (CW-20261114-0182). Before, it was marked `failed` and never retried, and a refresh did not help; only a hub
  reload brought it back.
  - **Retry:** it is retried with capped exponential backoff and jitter (5 s doubling to 5 min, ±20%), without limit.
  - **States:** `failed` → `recovering` (while waiting, with `next_retry_at` and `restart_attempts`) → `starting` →
    `connected`, shown in `tether_gateway_status` and published as `mcp.upstream_state_changed`. Every transition is
    published, including a stdio restart (`reconnecting`), the stdio restart limit (`failed`, with its error) and a
    reconnect accepted with a name collision (`connected`).
  - **Retry now:** a refresh (`tether_catalog_refresh`, a reload) retries a down remote upstream at once instead of
    reporting the stale failure.
  - **Implicit retries:** a tool list, search or call that needs a down remote upstream can bring its next attempt
    forward, but only once per backoff step and not before half that step's wait. A dead upstream is never redialed
    back to back.
  - **Mid-life failures:** a remote upstream that fails after connecting (a failed refresh) gets the same timed retry.
  - **Inventory:** while it waits, gateway inventory counts it as unavailable, not incomplete, so a long outage does
    not hold `complete: false`.
  - **A failed tool list:** an upstream that connected but failed its tool list is no longer parked until shutdown;
    it retries too.
  - **Unchanged:** stdio upstreams keep their restart policy and limit.
