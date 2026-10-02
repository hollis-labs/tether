# Use MCP through Tether

These guides are for agents connecting to or using Tether's MCP gateway.
They describe the shipped stdio surface; the [full MCP reference](../../mcp.md)
contains tool schemas and integration details. Catalog edits belong to the
operator or an agent authorized to configure that catalog.

| What you need to do | Guide |
|---|---|
| Connect a client to Tether alone | [Connect](connect.md) |
| Choose flat or search; discover and call a tool | [Discovery](discovery.md) |
| Restrict upstreams or grant them to a launched agent | [Limit tools](limit-tools.md) |
| Choose daemon-owned upstreams for launched sessions | [Daemon ownership](daemon-ownership.md) |
| Understand a protected Codex proxy | [Protection](protection.md) |
| Add an app and its credential to the gateway | [Add an upstream](add-upstream.md) |
| Inspect budgets and event retention | [Budgets and policies](budgets.md) |
| Diagnose missing tools or a failed server | [Troubleshoot](troubleshoot.md) |

Profiles are planned in CW-20260926-0008. MCP result budgets, daemon-enforced
quotas/approval policy, and guided onboarding are planned in
CW-20261001-0554, CW-20261001-0555, and CW-20261001-0556 respectively.
Their proposed knobs are not commands you can use yet.
