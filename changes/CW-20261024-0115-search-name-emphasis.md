- Rank tool names higher in `tether_tool_search` (CW-20261024-0115). The words of a tool's name now count three extra times in the
  BM25 text, approximating the name-over-description weighting the shared ranker cannot express. On the search-eval fixture (170
  queries) recall@5 goes 0.812 to 0.841 and MRR 0.709 to 0.737, and on 24 queries written before the experiment MRR goes 0.717 to
  0.790; every exact-name and prefix case still ranks first. Argument names, argument descriptions and light stemming were
  tried and are not shipped (they lowered MRR or hard-query recall); the numbers are in docs/agents/mcp/discovery.md. The fixture catalog now
  carries each tool's argument names.
