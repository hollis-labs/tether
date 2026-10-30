- Fix the brief-page byte budget to count what the gateway counts (follow-up to CW-20261024-0053). A tool result is sent as text
  and again as structuredContent, so `result_bytes` is about twice the text. The first budget (12 KB agents, 14 KB events)
  measured the text, left the default `tether_catalog_list_agents` page at 19 KB of result and an event page near 29 KB, and did not bind.
  Both budgets are now 7 KB of rows (about 14 KB of result), and the tests measure the whole result and fail with the old budgets.
