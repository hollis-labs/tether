- Never cut catalog plan reads at the MCP result cap (CW-20261118-0033):
  `tether_catalog_plan`, `tether_catalog_plan_get` and `tether_catalog_validate`
  default to unlimited through built-in per-tool `result_cap` values, so a plan
  whose diff exceeds 32 KiB no longer shows one planned file of five. A profile's
  own `result_cap.tools` entry for the same tool still wins.
- Put the gateway truncation marker into a cut result's `structuredContent` too
  (CW-20261118-0033), under the reserved top-level key `_gateway_truncated`, so
  clients that show only `structuredContent` (Claude Code) no longer see a silent
  cut. Results under the cap are unchanged.
