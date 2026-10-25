- Stop scanning the whole events table to find its first and last id (CW-20261024-0055). The environment snapshot,
  environment events, and the event bus's stream bounds each asked for MIN(id) and MAX(id) in one statement, which
  SQLite cannot answer from the end of the b-tree, so every `tether_session_health`, snapshot and subscription read
  1.8M rows (92 ms warm, seconds cold on the hub, and it holds the single database connection while it does). Each bound
  is now its own scalar subquery: an O(log n) search with identical results, including after retention purges. A plan
  test asserts no `SCAN events`.
