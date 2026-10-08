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
[`examples/catalog/boot-profiles/tether.codex.main.yaml`](../../examples/catalog/boot-profiles/tether.codex.main.yaml).

## Generate A Boot Prompt

```sh
tether generate-boot <boot-profile-id>
```

This renders the boot profile to stdout without launching a provider. Use it to
verify slot expansion and prompt content before creating a session.

## Launch With A Boot Profile

```sh
tether boot <boot-profile-id>
```

`tether boot` renders the boot prompt and creates a daemon-managed session through
the boot profile's configured launch. This is the supported boot path for
Claude, Codex, and Opencode managed sessions.

## Legacy Direct Exec

```sh
tether boot-exec <boot-profile-id>
```

`boot-exec` directly execs the native Claude PTY runtime. It is Claude-TUI-only
and rejects Codex or Opencode launch profiles. This path is maintained for
interactive Claude terminal use and is expected to be deprecated from
user-facing workflows.

For Codex and Opencode, use `tether boot <profile>` or `tether launch --launch
<launch-id>`.

## Example Boot Profile

```yaml
id: tether.codex.app-server
display_name: "Tether - Codex App Server"
launch: tether-codex-app-server
mcp_servers: [torque]
mcp_tools: [torque_task_get, torque_task_list]
identity:
  profile_id: tether-codex-app-server
  role: backend
  project: tether
slots:
  agent:
    type: role_summary
    path: ~/.nanite/roles/domain/backend.md
```

The important field is `launch`: it points to the launch profile that selects
the provider runtime. `mcp_tools` optionally restricts final MCP wire names;
omission inherits and `[]` grants no targets. See the
[tool grant and flat-mode contract](../agents/mcp/limit-tools.md#boot-tool-grants-and-flat-mode).

## Preflight

```sh
tether list-boot-profiles
tether generate-boot <boot-profile-id>
tether resolve --launch <launch-id>
```

If `generate-boot` succeeds but `boot` fails, inspect the launch provider and
workspace resolution first. Boot prompt rendering and provider launch are
separate steps.
