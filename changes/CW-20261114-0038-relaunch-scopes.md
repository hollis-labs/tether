- A relaunch keeps the seat's scopes (CW-20261114-0038): a worker launch that resumes the same seat (an internal resume
  whose origin session has the same logical agent and launch id) is granted the catalog `mcp_scopes` capped by what that
  previous session held, whoever triggers it, so a seat relaunched from Tachyon chat or a wake no longer silently loses
  `tether.terminal` / `tether.admin`. Nobody gains scopes the seat never held; a first launch is still bounded by the
  launcher. The credential's `launch_attribution` records `origin` for such a grant, and any launch that drops a scope
  publishes `session.scopes_narrowed {requested, granted, dropped, basis, launcher}` (warning). A worker resume now
  takes the catalog agent's identity and scopes (it fell back to the default scopes), and a Tachyon chat launch is made
  with the caller's identity (it was anonymous).
- `mcp.servers: ["*"]` means every enabled upstream the agent may hold (CW-20261114-0038), expanded when the launch is
  resolved (catalog launches and boot profiles); machine-changing upstreams only for agents with `machine_authority`. An
  effective grant still refuses a literal `*`.
