- Pin the search ranking with regression tests (CW-20261024-0114). The search-eval fixture gains a `regression` category: the
  telemetry query that the old keyword scorer got wrong (`card_show`, `panel_open` and `python_run` outranked `tether_tool_metrics`),
  exact-name queries and name-prefix queries; Go tests assert the noise tools stay below the right answer, an exact name ranks first and a
  prefix finds its tool in the top 3. Both fail against the keyword scorer. Baselines re-recorded.
