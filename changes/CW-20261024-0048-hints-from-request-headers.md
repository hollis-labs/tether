- Fix the client label and MCP-session hash never being recorded on tool-call events
  (CW-20261024-0048). The MCP SDK runs tool handlers on a session-level context, so
  the hints taken from the HTTP request's context in the first release were never seen;
  they are now read from the per-request headers the SDK exposes, in the tool-call
  logging middleware. Before this, `client_label` stayed empty and search-to-call
  linkage fell back to a coarse per-principal key that merges concurrent clients that
  share one credential.
