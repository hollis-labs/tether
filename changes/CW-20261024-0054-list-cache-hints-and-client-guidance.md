- Send real cache hints on `tools/list` and document client guidance (CW-20261024-0054). The gateway was zeroing `ttlMs`
  and leaving `cacheScope` public; it now sends `private` (the list depends on the caller's profile) with a 5-minute TTL
  in search mode (four fixed tools) and 30 seconds in flat mode. Tests assert the search-mode `tools/list` is
  byte-identical across restarts and across upstream changes. docs/agents/mcp/discovery.md gains client guidance: flat
  with a role profile as the default for hosted agents, when to choose search, Claude Code's double deferral, and the
  stable downstream-name field (`tool_input.name`) for hook authors.
