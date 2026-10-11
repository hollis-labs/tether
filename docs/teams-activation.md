# Teams MCP tools

When `modules.teams` (or legacy `teams.enabled`) is on, the daemon MCP endpoint
registers one tool per team verb. When teams are off the tools are not listed.

| Tool | Verb |
|---|---|
| `tether_team_form` | create a team run |
| `tether_team_dissolve` | end the run (destructive) |
| `tether_team_member_add` / `tether_team_member_remove` | change membership |
| `tether_team_assign` / `tether_team_delegate` | hand work to members |
| `tether_team_address` | message a member or the team |
| `tether_team_cancel` | cancel in-flight work (destructive) |
| `tether_team_report_result` | member reports a result |

Arguments are `{key, request}`; `key` is a caller-scoped idempotency key. No
caller identity argument is accepted: the principal is the authenticated
bearer's, and extra fields are rejected. Calls need the `team.write` scope.
Launched sessions receive `team.write` in their minted credential only when
teams are enabled, so they reach these tools through `tether mcp --forward-daemon`.
