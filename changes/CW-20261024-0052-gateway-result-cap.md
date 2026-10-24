- Cap the size of one tool result at the gateway (CW-20261024-0052, implements
  CW-20261001-0554). A successful result of a read-only tool over 32 KiB is cut at an
  item boundary of its largest array, still valid JSON, with a `gateway_truncated`
  marker and a cursor; the new `tether_result_next` tool serves the rest from a stored
  copy (ten minutes, bounded), so pages never skip or repeat. Errors, write receipts and
  results with nothing to cut are delivered whole. Tool-call events record
  `result_original_bytes`, `result_capped` and `result_cap_skipped`. Set
  `TETHER_MCP_RESULT_CAP_BYTES=0` and restart to turn it off, or use a profile's
  `result_cap` for per-profile and per-tool values.
