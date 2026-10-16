- The `editable_agents` allowlist now travels in the saved session MCP policy (CW-20261016-0161): a launch
  saves the catalog agent's allowlist into `SessionPolicy.EditableAgents` (sorted, de-duplicated, part of the
  policy digest, so a changed allowlist changes the MCP view fingerprint), and the hub `/mcp` handler applies it
  to `tether_agent_create` / `tether_agent_edit` for that session. A session on the hub relay therefore keeps
  the restriction the ssh-launched `tether mcp --editable-agent` flags carried; a session with no allowlist is
  unrestricted as before. Policies saved before this field keep their digest. Native-only resume of a session
  saved before its agent gained an allowlist is refused as an authority change.
