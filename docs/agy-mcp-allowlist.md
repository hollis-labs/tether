# agy MCP allowlist

A headless `agy -p` run has nobody to answer a permission prompt. Without an
allow rule every MCP tool call is auto-denied: the run exits 0 with an empty
response and a stderr hint ("a tool required the "mcp" permission that headless
mode cannot prompt for"). Tether launches agy in accept-edits, so before this
change an agy agent could not call Tether's tools at all unless the launch was
`bypass` (`--dangerously-skip-permissions`, which approves everything).

## Turning it on

It is **off by default**. The switch is `TETHER_AGY_MCP_ALLOWLIST=1`, read from, in
this order, the launch's own env and then the process environment:

- **One launch (preferred):** in the launch profile,

  ```yaml
  overrides:
    env: {TETHER_AGY_MCP_ALLOWLIST: "1"}
  ```

  A launch that sets it to `0` opts out even when the process setting is on.
- **Everything on one host:** set it in the daemon's environment (hub-local
  launches) or the worker's environment (worker-hosted launches) and restart.

It applies to both paths an agy session can take. A launch with `worker: <id>` is
started by `tether-worker` on that machine, so the worker binary must include this
feature; a hub-local launch is started by the daemon.

## What the switch does

For each antigravity launch with the switch on, the launcher then:

1. reads the server names the shared planter wrote into
   `<boot dir>/.agents/plugins/tether/mcp_config.json` (agy names a plugin's
   server `<plugin>_<server>`, so Tether's own server is `tether_tether`);
2. builds a private HOME at `<session workspace>/agy-home`: a symlink for every
   entry of the real home except `.gemini`, `.agents`, `.DS_Store`,
   `.CFUserTextEncoding` and `.Trash`, plus a private `.gemini` that links the
   operator's login files and holds a generated
   `antigravity-cli/settings.json` with `permissions.allow`;
3. starts agy with `HOME` pointing at it.

The rule is `mcp(tether_tether/*)`: Tether's own MCP server, server-wide. That
server's tool set is already limited server-side by the agent's profile (deny by
default), so the per-tool precision lives there, in one auditable place. Any
other server in the plugin gets no rule and stays denied. Everything outside MCP
(`run_command`, file writes, URL reads) stays denied headless; a policy for those
is a separate follow-up.

The real `~/.gemini` is never written by Tether. Settings are merged, not
clobbered: keys and allow rules Tether does not own are kept, and a rule it wrote
earlier that is no longer granted is dropped at the next launch (the rules it
owns are tracked in `tether-owned-rules.json` next to `settings.json`).
`settings.json` is mode 0600. If the switch is on and the home cannot be built the
launch fails with the reason; it does not fall back to a HOME that denies every
call.

## Known limits

- **Token refresh writes through.** The login files are symlinks to the real
  ones, so a token refresh by an agent rewrites the operator's real token file
  (observed on Linux). Copying tokens instead would go stale.
- **The private home is per session.** A new session cannot see an earlier
  session's agy conversation, unlike Codex, which keeps a stable native-state root
  across resume and recovery. Parity is tracked in CW-20261024-0127.
- **Rule syntax is observed, not documented.** `mcp(<server>/<tool>)`, the
  settings path and the `<plugin>_<server>` naming are agy 1.4.7 behavior (the
  older 1.4.0 on worker-1 was upgraded first). Re-run a stub turn after each agy
  update.
- Hooks cannot allow a call (an allow from a hook is advisory), so they are not
  used.

## Rollback

Remove the launch's `overrides.env` entry (or unset the process setting and restart).
New launches use the real HOME again; existing `agy-home` directories sit inside session workspaces
and can be deleted with them.

## Verification (2026-10-24)

A stub stdio MCP server with `ok_tool` and `no_tool`, a workspace plugin, a farm
HOME and `permissions.allow = ["mcp(stub_stub/ok_tool)"]`, one `agy -p
--output-format stream-json` turn: `ok_tool` ran, `no_tool` was denied, exit 0,
the standard stderr hint. macOS (Keychain login under the relocated HOME) and
Linux (worker-1, agy 1.4.7) both passed; the real `~/.gemini` was unchanged on
macOS and on Linux only the login token was refreshed through its symlink.
