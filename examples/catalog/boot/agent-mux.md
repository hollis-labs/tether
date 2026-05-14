# Agent Mux — Project Context

Agent Mux (`mux`) is a Go CLI + daemon that manages AI agent sessions. It provides a
unified provider interface so sessions backed by Claude Code, Claude Stream, OpenAI
Codex, Gemini, Aider, and others can be launched, attached to, and controlled
uniformly — including via an MCP stdio server (`mux mcp`).

## Key packages

| Path | Purpose |
|------|---------|
| `cmd/mux/` | Cobra CLI entrypoint |
| `internal/provider/` | Runtime + Session contracts; registry |
| `internal/provider/cli/` | CLI-backed providers (Claude adapters, goprovider, opencode) |
| `internal/runtime/` | Session lifecycle manager |
| `internal/app/` | Composition root (Service) |
| `internal/config/` | Catalog YAML schema + validation + loader |
| `internal/launch/` | Plan resolution + boot prompt assembly |
| `internal/bootgen/` | Boot profile YAML → rendered boot prompt |
| `internal/mcpadapter/` | MCP stdio server (23 tools) |
| `internal/store/` | SQLite + migrations |
| `internal/workspace/` | Per-session temp directory layout |
| `pkg/claudestream/` | Claude NDJSON event parser |

## Build & test

```
make build          # produces bin/mux
make test           # full suite
go test ./...       # same
go build ./...      # compile check
```

## Provider contract

All providers implement `provider.Runtime` (Start/Prepare/Caps) and
`provider.Session` (SendInput/Stop/Wait/Health). CLI providers are selected by
provider brand plus `runtime_kind`:
- **Managed streaming stdio**: `claude-code` (`provider: claude`, `runtime_kind: streaming-stdio`)
- **Interactive PTY/TUI**: `claude-pty` (`provider: claude`, `runtime_kind: pty`)
- **JSON-RPC stdio**: `codex-app-server` (`provider: codex`, `runtime_kind: jsonrpc-stdio`)
- **Subprocess per turn**: `claude-goprovider`, `codex-cli`, `opencode`

Boot mode `agents_md` writes the boot prompt to `AGENTS.md` in the session
workdir before the first turn. PTY launches use `bootstrap.mode: stdin` plus
AutoPlantBootDir so `mux sessions attach` can drive the live TUI.
