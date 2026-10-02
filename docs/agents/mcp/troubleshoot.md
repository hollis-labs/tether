# Diagnose missing MCP tools

Start with `tether_gateway_status`, available in flat and search modes. It
reports the effective mode/source, origin connection states, cataloged,
eligible and available target counts, and whether the inventory is complete.
Counts describe targets; they need not equal the number of gateway wrapper
tools a search-mode client sees.

## Check one tool

Call `tether_gateway_status` with:

```json
{"name":"tether_session_list"}
```

Read `visible` and `reason`. In search mode, a target can be connected and
eligible while `visible: false` means it is not a directly advertised client
tool. Hydrate it with `tether_tool_list` and dispatch it through
`tether_tool_call`; see [Discovery](discovery.md#search-hydrate-call).

## Match the failure to the next action

| Symptom | Check |
|---|---|
| Startup rejects an unknown/disabled server ID | Compare the selected IDs with enabled catalog entries; [Limit tools](limit-tools.md) explains selectors. |
| Wrong discovery surface | Read status `mode`/`source`; explicit CLI/env/config values determine it, not client brand. |
| Origin excluded with “cannot be confined locally” | Read [Protection](protection.md#remote-upstreams-require-an-explicit-waiver); remote servers require an operator decision. |
| Origin failed, restarting or exhausted | Read its error and recovery fields; correct the upstream executable, credential or endpoint, then restart the client proxy. |
| Search found nothing and `complete` is false | Inspect unavailable origins; an incomplete inventory does not establish absence. |
| A mutation fails auth/scope checks | Check the proxy's token and scope configuration, separately from upstream credentials. |
| Daemon-only startup fails | Restore the daemon connection; that proxy intentionally has no database fallback. |

## Check catalog and filesystem configuration

```sh
tether doctor --json
```

Doctor validates discovery configuration, enabled file credential references,
daemon reachability and reported protection posture. Its credential-file check
does not call helpers or print credential contents. For missing upstream registration, use
[Add an upstream](add-upstream.md); for AI-budget errors, use
[Budgets](budgets.md#ai-cost-budgets).
