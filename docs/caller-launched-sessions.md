# Caller-Launched Sessions (v005-08 Tier-2)

This guide is for external consumers (Nanite, Clockwork, Hadron blueprints, custom integrations) that want to launch Mux sessions using their own agent definitions without first registering them in Mux's catalog.

## When to use Tier-2 vs Tier-1

- **Tier 1 (catalog-only).** Mux owns the agent persona. The caller knows a launch ID; everything else is resolved from the catalog. Use when the agent lives in the Mux catalog and the caller just wants to spawn a session of that agent.
- **Tier 2 (caller-provided).** The caller has its own agent definition / boot profile / per-launch override. Mux assembles the BootDirSpec compilation inputs at session-create time and routes through the same runtime pipeline.

Tier-2 doesn't replace the launch profile entirely — the catalog still supplies project root, provider, workspace shape, and env-mode. The caller is overriding **persona** (agent definition) and optionally **MCP allowlist + per-launch tweaks**.

## Payload fields

All four are optional; any one set routes through the Tier-2 path.

| Field | Shape | Effect |
|---|---|---|
| `agent_file` | filesystem path | Loads an agent YAML matching `config.Agent`. Field-merged over the catalog agent. |
| `agent_inline` | JSON string | Same shape as `agent_file` but inline. Highest precedence in the agent resolve order (inline > file > catalog). |
| `boot_profile` | filesystem path | Loads a `bootgen.Profile` YAML. Currently consumed for `mcp_servers` (the MCP allowlist for this launch). |
| `override` | JSON string | Per-launch override applied last over the resolved plan. Shape: `{"system_prompt": "...", "env": {"KEY": "VAL"}}`. |

Plus `boot_prompt` (string) — the pre-v005-08 raw boot-prompt override, preserved for back-compat. Wins over all v005-08 composition layers when set.

## Resolve precedence

For the agent definition (highest → lowest):

```
agent_inline > agent_file > catalog agent
```

Each higher-precedence source field-merges over the base — non-empty fields replace empty ones. List fields (`skills`, `roles`) **replace** rather than concatenate.

For the BootPrompt composition (later wins on conflict):

```
catalog fragments
+ effective_agent.system_prompt    # heading "# System"
+ effective_agent.agent_prompt     # heading "# Agent"
+ native skill files (per-provider, planted in the bootdir)
+ override.system_prompt           # if set, replaces verbatim
+ boot_prompt                      # if set, replaces verbatim (last word)
```

For env: provider-overrides + override.env merge into `plan.Env`; the existing per-provider env mode (`merge` / `whitelist`) takes care of the rest.

## Surfaces

### CLI

```bash
mux launch --launch my-launch-id \
  --agent-file ./my-agent.yaml \
  --boot-profile ./research-mode.yaml \
  --override '{"system_prompt":"You are now a code reviewer."}'
```

### MCP (`mux_session_create`)

```jsonc
{
  "launch_id": "my-launch-id",
  "agent_inline": "{\"id\":\"reviewer\",\"system_prompt\":\"You are a code reviewer.\"}",
  "boot_profile": "/path/to/research-mode.yaml",
  "override": "{\"env\":{\"REVIEW_MODE\":\"strict\"}}"
}
```

Follow with `mux_session_launch` to start the session.

### HTTP (`POST /sessions`)

```jsonc
{
  "launch": "my-launch-id",
  "agent_file": "/path/to/agent.yaml",
  "boot_profile": "/path/to/profile.yaml",
  "agent_inline": "",
  "override": "{\"system_prompt\":\"...\"}",
  "boot_prompt": ""
}
```

Returns `201 Created`. Follow with `POST /sessions/{id}/launch`.

## Library client

```go
import "github.com/chrispian/agent-mux/internal/client"
import "github.com/chrispian/agent-mux/internal/api"

c, _ := client.NewLocal()
res, err := c.LaunchWithInput(ctx, api.LaunchRequest{
    Launch:       "my-launch-id",
    AgentFile:    "/path/to/agent.yaml",
    BootProfileFile: "/path/to/profile.yaml",
    Override:     `{"system_prompt":"..."}`,
})
```

Or step-wise:

```go
created, err := c.CreateSessionWithInput(ctx, api.LaunchRequest{...})
launched, err := c.LaunchSession(ctx, created.ID)
```

## Error semantics

- Malformed JSON in `agent_inline` or `override` returns `invalid_request` (HTTP 400 / MCP error code `invalid_request`).
- Missing `agent_file` / `boot_profile` paths return `internal_error` with the underlying `read <path>: no such file or directory` wrapped.
- Skill resolution errors (a skill referenced by `agent.skills` not found in any discovery layer) return `internal_error`. Provider-unsupported skill compilation is silently skipped (the session still launches). Supported providers receive compiled skill files in the planted bootdir.

## See also

- ADR 0033 — Two-Tier Agent Config (full design rationale)
- `docs/agent-config-reference.md` — schema reference
- `docs/skill-format.md` — skill format spec
