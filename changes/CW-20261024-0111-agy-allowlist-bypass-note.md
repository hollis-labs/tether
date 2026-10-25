- Correct `docs/agy-mcp-allowlist.md` (CW-20261024-0111): worker-hosted agy launches already run with
  `--dangerously-skip-permissions`, so MCP calls work there without the allowlist; the allowlist
  matters for hub-local launches in accept-edits, and for workers only if agy is later moved off bypass
  (which also needs a policy for the non-MCP tools). A live `tether-agy` session on agent-os built its
  private HOME and called `tether_whoami`, but under bypass.
