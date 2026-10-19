- Add bounded, filter-bound cursor pagination to workstream listings across
  HTTP, CLI, MCP and client surfaces (CW-20261018-0008), with deterministic
  timestamp/ID ordering and explicit live-scan continuation/completeness.
- Join the turn router's owned event subscriber during shutdown before storage
  cleanup, closing a replay/temporary-directory cleanup race found while
  validating workstream pagination (CW-20261018-0008).
