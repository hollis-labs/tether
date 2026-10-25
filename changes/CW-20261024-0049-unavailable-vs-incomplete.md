- Make `complete: false` mean something (CW-20261024-0049). Discovery and `tether_gateway_status` now distinguish an
  origin that is known to be down (`failed` after the restart limit, or `excluded`) from one that is only temporarily
  absent (`starting`, `reconnecting`). Every unavailable origin is still listed in `unavailable_servers`, now with its
  reason in `unavailable_reasons`; only a temporarily absent origin (`incomplete_servers`) or a name collision makes
  `complete` false. Before this a permanently failed app made every response say incomplete. The lint and
  `unexamined_tools` counts move because each status pass examines a bounded batch; documented in
  docs/mcp-upstream-conformance.md.
