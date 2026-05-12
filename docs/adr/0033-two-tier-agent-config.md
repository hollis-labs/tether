# ADR 0033 — Two-Tier Agent Config (Agent Ops)

**Status:** Accepted
**Date:** 2026-05-12
**Supersedes:** —
**Superseded by:** —

## Context

Through v005-07, Mux's agent catalog was a single bundle: one directory of `agents/*.yaml` per Mux installation. That was sufficient when Mux only ran sessions for its own AI features. As external consumers (Nanite, Clockwork, Hadron blueprints) start launching Mux sessions, two needs surface that the single-bundle model can't satisfy cleanly:

1. **External consumers bring their own personas.** Nanite has its agent definitions; Clockwork orchestration steps reference agent profiles that live in the Clockwork catalog. They want Mux to launch sessions from those definitions without first registering them in Mux's catalog. Forcing every consumer to copy YAML into `~/.agent-mux/catalog/agents/` couples consumer release cycles to the operator's catalog hygiene — wrong directionally.
2. **Tool surface varies by use mode, not by agent identity.** A single agent persona ("refactoring engineer") might run in a research mode with `vanta` + `hadron` MCP servers, or in a coding mode with `git` + filesystem MCP. The agent definition shouldn't carry MCP server config — that's launch shape, not persona.

A third concern is that operators want per-machine and per-repo overrides without forking the bundled catalog. Today that requires editing the catalog directory directly.

## Decision

### 1. Two tiers for agent ownership

**Tier 1 — Mux-owned internal agents.** Personas Mux uses for its own AI features (bootgen generation, future `mux_discover` semantic search if it grows AI). Mux owns the YAML in its system catalog; managed via the `mux agents` CLI; users can override via project / user layers.

**Tier 2 — Caller-provided launches.** When an external consumer calls Mux to launch a session, it passes the agent definition (and optionally a boot profile + override) as a file path or inline JSON payload at launch time. Mux's service layer assembles the BootDirSpec compilation inputs and routes through `agentsessions.NewFromAdapter`. Mux does not persist the caller's content.

### 2. MCP allowlist lives on the boot profile, not the agent

A boot profile (`bootgen.Profile`) gains an `mcp_servers []string` field. One agent can have multiple boot profiles (research-mode / coding-mode / etc.), each declaring its own MCP allowlist. The launch path pipes the list into `MUX_MCP_SERVERS` so the spawned agent's `mux mcp --proxy` invocation sees it.

This separates "who the agent is" (persona, skills, prompt) from "what tools it can reach" (launch shape, MCP surface). It's the unlock for Hadron-step-calls-Mux-session flows where each step might want a different tool subset against the same agent persona.

### 3. Three-layer discovery with later-layer-wins precedence

Discovery order at launch:

| Order | Layer | Root | Purpose |
|---|---|---|---|
| 1 | system | `<catalogPath>` (default `~/.agent-mux/catalog/`) | Mux bundled defaults |
| 2 | user | `~/.agent-mux/` | Personal customization |
| 3 | project | `./.agent-mux/` (CWD-relative) | Repo-local overrides |

Each layer can carry `agents/`, `boot-profiles/`, and `skills/` subdirectories. Later layers override earlier ones on ID collision. Missing layers are skipped silently; the bundled catalog remains the floor.

### 4. Extended `agent.yaml` schema

```yaml
id: my-agent
name: My Agent
roles: [backend]
skills: [refactor-go, lint-fix]              # NEW v005-08: populated semantics (Phase 3+4)
boot_fragments: []
permissions:
  network: false
  default_sandbox: workspace-only
system_prompt: |                              # NEW v005-08
  You are a careful refactorer...
agent_prompt: |                               # NEW v005-08
  Persona / who-am-I content...
provider_overrides:                           # NEW v005-08
  claude-code:
    env: {CLAUDE_FLAG: "1"}
    extra_args: ["--allow-foo"]
```

All new fields are optional; empty defaults preserve back-compat for existing catalog YAML.

### 5. Extended `boot-profile.yaml` schema

```yaml
id: my-agent.research-mode
display_name: "Research Mode"
identity: { ... }
slots: { ... }
mcp_servers: [vanta, hadron, cerberus]        # NEW v005-08
```

`mcp_servers` is optional; empty / omitted means the proxy default (all servers).

### 6. Caller-provided launch surface — fields live on session create, not start

Pre-v005-08, session lifecycle was already split: `mux_session_create` (workspace + plan persisted; state=created) → `mux_session_launch` (state=running). The v005-08 Tier-2 fields attach to the **create** step, since that's where the BootDirSpec compilation inputs are assembled. `mux_session_launch` and `POST /sessions/{id}/launch` stay pure state-transitions.

| Surface | Entry point | New fields |
|---|---|---|
| CLI | `mux launch --launch <id>` | `--agent-file`, `--agent-inline`, `--boot-profile`, `--override`, `--boot-prompt` |
| MCP | `mux_session_create` | `agent_file`, `agent_inline`, `boot_profile`, `override` |
| HTTP | `POST /sessions` | same JSON body fields |

(The sub-boot-prompt's reference to `mux sessions launch` was documentation drift; the actual top-level command is `mux launch`.)

### 7. Resolve precedence

For the agent definition (highest → lowest):

```
agent_inline (JSON)  >  agent_file (YAML on disk)  >  catalog agent (resolved via launch)
```

Each higher-precedence source field-merges over the base — non-empty fields replace empty ones in the lower-precedence source. List fields (`skills`, `roles`) **replace** rather than concatenate; concatenation surprises more often than it helps.

For the BootPrompt composition (later wins on conflict):

```
catalog fragments
+ effective_agent.system_prompt    # heading "# System"
+ effective_agent.agent_prompt     # heading "# Agent"
+ compiled skills (per-provider)   # via skills.CompileForProvider
+ override.system_prompt           # if set, replaces composed prompt verbatim
+ boot_prompt_override             # if set, replaces composed prompt verbatim (last word)
```

For env composition: provider-overrides from the effective agent + override JSON env merge into `plan.Env`; the existing env-mode pipeline (`merge` / `whitelist` per provider) takes care of the actual child-env resolution at launch time.

For MCP servers (highest → lowest):

```
boot_profile.mcp_servers (per-call boot profile)
> launch.mcp.servers (catalog launch override)
> project.mcp.servers (catalog project default)
```

### 8. Per-launch override is a JSON flag, not env var

```json
{"system_prompt": "...", "env": {"KEY": "VAL"}}
```

Reasons: explicit, composable, survives audit logs cleanly, doesn't leak through process listings the way a long env var would.

### 9. Skills compile per-provider; transport over BootPrompt for v005-08

`internal/skills/` parses markdown files with YAML frontmatter (`id`, `name`, `description`, `triggers`, body). `CompileForProvider(providerID, []Skill) → []CompiledFile` dispatches per-provider:

- Claude (and aliases `claude-code` / `claude-stream`) → one `.claude/skills/<id>.md` per skill.
- Codex (and aliases `codex-app-server` / `codex-cli`) → single `AGENTS.md` aggregating all skills.

Both compilers return provider-agnostic `CompiledFile` (RelPath + Content + Mode); the BootDirSpec compilation pipeline in `internal/app/agent_ops.go` concatenates them into the assembled `BootPrompt` text alongside the system/agent prompt. **Native per-file placement** (so Claude reads `.claude/skills/<id>.md` directly from the planted boot dir, and Codex picks up `AGENTS.md`) waits on a future `agentsessions` enhancement that lets apps inject extra `PlantedFiles` into `BootDirSpec`. Captured as a follow-up.

Opencode and Nanite-headless are out of v005-08 scope; `CompileForProvider` returns `ErrUnsupportedProvider` for those (non-fatal — skill compilation is skipped, the session still launches).

### 10. `mux agents` CLI

`agents list` now shows a `LAYER` column annotating which discovery layer each entry came from. New subcommands:

- `mux agents create <id> --scope user|project|system [--name --system-prompt --agent-prompt]` — writes a new agent YAML into the chosen layer.
- `mux agents edit <id>` — opens the resolved file in `$EDITOR` (prints the path if `$EDITOR` is unset).
- `mux agents show <id>` — prints the YAML with `id` / `layer` / `path` comments above the body.

### 11. Out of scope

- Cross-provider skill compilation for Opencode + Nanite-headless (capture as v005-08b if a consumer needs it).
- Plugin-sdk integration (post-beta).
- GUI for agent management (separate Mux native GUI track).
- Native per-file skill placement via app-injected `PlantedFiles` (waits on a `go-agent-sessions` enhancement).

## Consequences

**Positive:**

- External consumers can launch Mux sessions with their own agent definitions without operator catalog hygiene becoming a coupling point.
- Operators get per-repo and per-user overrides via the layer system without forking the bundled catalog.
- One agent persona can ship multiple boot profiles for different tool surfaces — the unlock for Hadron-step-driven flows.
- The skills package compiles per-provider, leaving room to add Opencode / Nanite-headless compilers as additive changes when needed.
- `mux agents` CLI gives operators a first-class authoring path for Tier-1 personas.

**Negative / accepted trade-offs:**

- Skill content currently transports over `BootPrompt` text rather than as native per-provider files. Functionally equivalent (the agent still sees the content), but adapter-native skill loading (e.g. Claude's `.claude/skills/<id>.md` autoloader) doesn't activate. Acceptable for v005-08; remediated by the planned go-agent-sessions enhancement.
- The two-tier model adds a Phase-4 service-layer composition step that previously didn't exist. Code complexity grows; offset by the fact that legacy `CreateSession` / `CreateSessionWithBootPrompt` now route through `CreateSessionWithInput` with zero-value Tier-2 fields, so the single composition path is also the only path.
- The discovery layer means catalog state is no longer one directory. `mux agents list` surfaces this clearly (the LAYER column); ADR 0033 + `docs/agent-config-reference.md` document it.

## Implementation

- `internal/config/model.go`: extended `Agent` (SystemPrompt, AgentPrompt, ProviderOverrides) + new `ProviderOverride` struct.
- `internal/bootgen/profile.go`: extended `Profile` with `MCPServers`.
- `internal/config/discovery.go` (NEW): `Layer`, `LayerSpec`, `DefaultLayers`, `Discover`, `LayeredCatalog`.
- `internal/skills/` (NEW package): Skill, Parse, ParseFile, LoadDir, WriteSkillFile, CompiledFile, CompileForProvider, CompileClaude, CompileCodex, ErrUnsupportedProvider.
- `internal/app/agent_ops.go` (NEW): CreateSessionInput, LaunchOverride, applyAgentOps, loadEffectiveSkills, mergeAgent.
- `internal/app/service.go`: CreateSessionWithInput method; legacy CreateSession / CreateSessionWithBootPrompt route through it.
- `internal/api/types.go` + `internal/api/sessions.go`: LaunchRequest extended; LaunchService interface gains CreateSessionWithInput; handleLaunch routes Tier-2 payloads.
- `internal/client/client.go`: CreateSessionWithInput + LaunchWithInput convenience.
- `internal/mcpadapter/sessions.go`: mux_session_create tool gains four optional fields.
- `cmd/mux/launch.go`: --agent-file / --agent-inline / --boot-profile / --override / --boot-prompt flags.
- `cmd/mux/agents.go`: rewritten with create / edit / show subcommands + LAYER column on list.
- `cmd/mux/daemon.go`: serviceAdapter gains CreateSessionWithInput.

## References

- Sprint `v005-08-agent-ops`: `agent-mux-v0-pack/docs/sprints/v005-08-agent-ops.md`
- Sub-boot-prompt: `agent-workspaces/boot/agent-mux/boot-prompt-v005-08-agent-ops.md`
- Vanta `mux_beta_push_roadmap_may_2026` (sprint sequence v005-06 → v005-11)
- Vanta `followup_mux_agent_ops_config_layer_unification` (parent intent)
- Vanta `mux_daemon_shaped_thin_wrapper_role` (Mux is service-control-plane, not user-launcher)
- ADR 0028 — BootDirSpec adoption (prior planting model)
- ADR 0032 — Lib-tier v0.9.x adoption (substrate this sprint builds on)
