- Rank `tether_tool_search` with BM25 instead of the word-count scorer (CW-20261024-0058).
  On the search-eval fixture recall@5 rises from 0.594 to 0.804, recall@1 from 0.348 to
  0.601 and MRR from 0.456 to 0.690, with response size and latency unchanged. Exact tool
  names and name prefixes rank first. Set `TETHER_MCP_SEARCH_SCORER=keyword` and restart
  to restore the previous scorer. Adds the `libs/agent/core` module (`toolselect`).
