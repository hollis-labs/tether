# Boot Prompt Generation

Boot profiles layer dynamic prompt generation on top of launch profiles. The
launch profile owns the provider/runtime/workspace setup. The boot profile owns
identity, MCP allowlists, and prompt slots that are rendered at boot time.

## Files

```text
<catalog>/
  launches/<launch-id>.yaml
  boot-profiles/<boot-profile-id>.yaml
  boot/*.md
```

Launch examples live under
[`examples/catalog/launches/`](../../examples/catalog/launches/). Boot profile
examples live under
[`examples/catalog/boot-profiles/`](../../examples/catalog/boot-profiles/).
For a concrete boot profile file, see
[`examples/catalog/boot-profiles/agent-mux.codex.main.yaml`](../../examples/catalog/boot-profiles/agent-mux.codex.main.yaml).

## Generate A Boot Prompt

```sh
mux generate-boot <boot-profile-id>
```

This renders the boot profile to stdout without launching a provider. Use it to
verify slot expansion and prompt content before creating a session.

## Launch With A Boot Profile

```sh
mux boot <boot-profile-id>
```

`mux boot` renders the boot prompt and creates a daemon-managed session through
the boot profile's configured launch. This is the supported boot path for
Claude, Codex, and Opencode managed sessions.

## Legacy Direct Exec

```sh
mux boot-exec <boot-profile-id>
```

`boot-exec` directly execs the native Claude PTY runtime. It is Claude-TUI-only
and rejects Codex or Opencode launch profiles. This path is maintained for
interactive Claude terminal use and is expected to be deprecated from
user-facing workflows.

For Codex and Opencode, use `mux boot <profile>` or `mux launch --launch
<launch-id>`.

## Example Boot Profile

```yaml
id: agent-mux.codex.app-server
display_name: "Agent Mux - Codex App Server"
launch: agent-mux-codex-app-server
mcp_servers: []
identity:
  profile_id: agent-mux-codex-app-server
  role: backend
  project: agent-mux
slots:
  agent:
    type: role_summary
    path: ~/.nanite/roles/domain/backend.md
```

The important field is `launch`: it points to the launch profile that selects
the provider runtime.

## Preflight

```sh
mux list-boot-profiles
mux generate-boot <boot-profile-id>
mux resolve --launch <launch-id>
```

If `generate-boot` succeeds but `boot` fails, inspect the launch provider and
workspace resolution first. Boot prompt rendering and provider launch are
separate steps.
