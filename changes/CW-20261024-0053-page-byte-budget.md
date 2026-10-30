- Keep default list pages under the 16 KB target (CW-20261024-0053 follow-up). Live telemetry after the paging release showed
  the default `tether_catalog_list_agents` page at 19 KB (25 brief rows, some with long skills lists) and event pages
  up to 23 KB. Brief agent pages and event pages now also stop at a byte budget (12 KB and 14 KB), always returning at least one row,
  and `next_cursor` continues from the last row returned, so a walk still sees every row once. Measured on the hub, release over
  release: average `list_agents` 249 KB to 19 KB, `registry_search` 176 KB to 2.5 KB, `events_history` 97 KB to 14 KB; calls over 32 KiB
  fell from 385 to 48.
