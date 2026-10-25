- List tools now page and stay brief at the source (CW-20261024-0053). `tether_catalog_list_agents` returned every agent
  with its prompts, body, env and spec (370 KB) and ignored arguments; it now returns brief rows (25 per page by default,
  `prompt_bytes` for what was left out) with `limit`, `cursor`, `query`, `role` and `verbose`. `tether_registry_search`
  never declared `query` or `limit`, so callers' filters were silently dropped; it now honors both, with the same paging
  and brief rows. `tether_events_history` and `tether_session_events` default to 25 events and cut payloads over 2 KiB
  (`payload_truncated`, `payload_bytes`; `full_payload=true` for whole). These four tools now refuse undeclared
  arguments. Paging contract is in docs/agents/mcp/budgets.md.
