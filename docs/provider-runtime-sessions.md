# Provider Runtime Sessions

Mux separates catalog launch IDs from boot profile IDs.

- `torque-claude` is a launch profile. It starts the existing managed Claude
  session through `claude-code` (`provider: claude`, `runtime_kind:
  streaming-stdio`).
- `torque-claude-tui` is a launch profile. It starts an interactive Claude TUI
  through `claude-pty` (`provider: claude`, `runtime_kind: pty`).
- `torque.engineer.main` is a boot profile ID when present in the user catalog.
  It renders dynamic boot slots, then creates a session from its configured
  launch.
- `torque.engineer.tui` is the TUI boot profile counterpart when present. It
  should point at `torque-claude-tui`.

Catalog-owned boot profiles can still be dynamic. Slots such as `agent`,
`recap`, `history`, `status`, `memory`, and `skills` render at boot time and
feed the resolved launch plan without duplicating prompt text in launch YAML.

## When to use each runtime

Use managed streaming when another tool will send turns through Mux:

```sh
mux launch --launch torque-claude
mux sessions attach <session-id>
```

`mux sessions turn`, MCP `mux_session_send_turn`, and the local API send framed
NDJSON user messages to Claude's long-lived streaming-stdio process. This is
the default for automation and delegated agents.

Use Claude TUI when a human wants to drive the live terminal UI:

```sh
mux launch --launch torque-claude-tui
mux sessions attach <session-id>
```

Attach sends raw terminal input to the PTY and resize events flow through the
session manager. Detach leaves the session running under the daemon.

## Smoke Checklist

The quick checks below cover managed Claude streaming and TUI. The full
cross-provider smoke matrix — Claude/Codex/Opencode, injection, skills,
worktree isolation — lives in
[`provider-launch-smoke-matrix.md`](provider-launch-smoke-matrix.md).

Managed streaming:

```sh
mux launch --launch torque-claude
mux sessions attach <session-id>
```

- `mux sessions inspect <session-id>` reports provider `claude-code`.
- Attach shows managed session output.
- The event log includes the boot-dir planted event.

Claude TUI:

```sh
mux launch --launch torque-claude-tui
mux sessions attach <session-id>
```

- `mux sessions inspect <session-id>` reports provider `claude-pty`.
- The Claude TUI accepts stdin through attach.
- Terminal resize changes are reflected in the TUI.
- Detach exits the client attachment without stopping the session.
- The event log includes the boot-dir planted event.
