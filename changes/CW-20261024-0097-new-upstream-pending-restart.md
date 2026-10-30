- Adding an MCP upstream through the catalog no longer breaks operator MCP connections (CW-20261024-0097). A catalog apply that added an
  enabled server made every operator view fail with "unknown or disabled MCP server ID" until the daemon was reloaded, because the grant came
  from the live catalog and was checked against the servers the daemon had started. The new server now degrades alone: the view prepares, the
  other tools are served, and `tether_gateway_status` lists it as `pending_restart` (inventory incomplete, with the reason). The catalog
  reload and apply also report the new server under `restart_required` instead of `immediate`. docs/agents/mcp/add-upstream.md says a reload is needed.
