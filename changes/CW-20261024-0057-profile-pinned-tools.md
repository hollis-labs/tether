- Add `pinned` to MCP profiles (CW-20261024-0057): in search mode the named tools are listed beside the four gateway
  tools with their real schemas (marked always-load) and callable by name, while every other eligible tool stays
  reachable through `tether_tool_search`. This is the missing half of "flat role profile plus search for the long
  tail": until now a flat profile had no search hatch, so narrowing it lost tools. Pins are validated like
  `order`/`always_load`, must pass the profile's own rules, and do nothing in flat mode. Pinned tools also refresh when
  their upstream's tool list changes. Without `pinned` nothing changes.
