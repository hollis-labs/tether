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
| Understand a protected Codex proxy | [Protection](protection.md) |
| Add an app and its credential to the gateway | [Add an upstream](add-upstream.md) |
| Inspect budgets and event retention | [Budgets and policies](budgets.md) |
| Diagnose missing tools or a failed server | [Troubleshoot](troubleshoot.md) |

Profiles are planned in CW-20260926-0008. MCP result budgets, daemon-enforced
quotas/approval policy, and guided onboarding are planned in
CW-20261001-0554, CW-20261001-0555, and CW-20261001-0556 respectively.
Their proposed knobs are not commands you can use yet.

## Read these guides through Tether

The running binary embeds this documentation and its linked references. Call
`tether_docs_list` for guide IDs, titles and one-line usage triggers. Call
`tether_docs_get` with `{"id":"connect"}` for that guide's body and reference
manifest. Fetch one reference with `tether_docs_get_file`, supplying the same
`id` and an exact `path` from the manifest. The manifest includes MIME type,
byte size and SHA-256 digest. File retrieval never reads arbitrary host paths.

Resource-capable clients can use `resources/list` and `resources/read`:
`tether://docs/mcp/connect` returns the guide and manifest as JSON; reference
URIs from the manifest return Markdown. Profiles that exclude the matching
docs get/file tool also exclude those resources. In search mode, hydrate and
dispatch the docs tools through the regular discovery wrappers.

HTTP clients use `GET /docs/mcp`, `GET /docs/mcp/{id}` and
`GET /docs/mcp/{id}/file?path=<manifest-path>`. MCP and HTTP share the same docs
service. A stdio forwarder that only relays tools can use the tools above;
resource access requires forwarding `resources/list` and `resources/read`.
