- Make search mode measurable (CW-20261024-0048). `tether_tool_search` events now
  record the normalized query terms, result count, zero-result flag, detail level and
  truncation, and a later call from the same connection links back to the search that
  preceded it with its rank in the results. Operator-token clients, which share one
  verified principal, can declare themselves with `tether mcp --forward-daemon
  --client-label <name>` (or `TETHER_CLIENT_LABEL`); the daemon records it as an
  unverified `client_label`, never for authorization. `tether_tool_metrics` (and
  `GET /events/tool-metrics`, `tether events tool-metrics`) gain `mode`,
  `group_by=mode`, `limit` and `compact`; a compact response is a small summary with
  averages and p50/p95 bucket bounds instead of full histograms, defaulting to the top
  25 groups. `tether_gateway_status` reports per-origin and total counts of tools
  that declare an `outputSchema`, annotations and a title. Additive; no schema
  migration, and no argument or result values are recorded.
