- Slim gateway search results (CW-20261024-0050). `tether_tool_search` gains a
  `detail: names` level (name and origin only) beside `summary` and `schema`. The
  default `summary` now carries the first sentence of the description, at most 160
  characters, instead of the whole text, plus the title, annotations and a new
  `required_args` list of the required argument names, and no schemas or icons;
  `detail: schema` and `tether_tool_list` still return full descriptions and schemas.
  Results are ranked as before and gain `origin_counts`. Responses are capped at
  32 KiB of items and cut at a whole tool: `truncated` with `truncation_reason`
  `limit` or `bytes`, `next_cursor` to resume, and `omitted_names` when hydrating by
  `names` (which has no cursor); the first tool is always returned. A new lint code
  `summary_too_long` flags tools whose first sentence is longer than 160 characters.
  Summary `annotations` is omitted rather than `null` when a tool has none.
