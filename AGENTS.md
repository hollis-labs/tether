# Tether — Agent Orientation

## What this is and why

Tether is the **local agent session control plane** for the hollis-labs
portfolio — the runtime fabric that owns session lifecycle, process/PTY
management, sandboxed execution, checkpoint/resume, brokered messaging, and
event streams for any CLI-backed agent (Claude Code, Codex, Kiro, opencode,
etc.). It runs as a per-user daemon (`muxd`) on-device, with no cloud
dependency. Every agent session — launch, attach, stop, checkpoint — goes
through the daemon; clients reach it over a Unix-domain socket via the `mux`
CLI, the HTTP API, the MCP stdio adapter, the ACP surface, or the
`go-tether-client` Go library. When Nanite, Torque, or any other app needs
to run an agent session or send a cross-system message, it goes through
Tether. Tether is the runtime, not the orchestrator.

> **Naming note:** the project was renamed from **Agent Mux** to **Tether**.
> The Go module is `github.com/hollis-labs/tether`; the CLI binary is still
> `mux` and the daemon `muxd`. `~/dev/hollis-labs/apps/agent-mux` is a
> backward-compat symlink to the canonical `tether` directory. Some in-repo
> docs (notably `README.md` and `planning/`) still carry the old name and the
> stale `~/.agent-mux/` state path — the canonical state root is `~/.tether/`.

## Where to start

- **`README.md`** — quick start, build, MCP adapter setup.
- **`cmd/mux/`** — the single CLI entry point. Cobra command tree: `daemon`,
  `sessions`, `agents`, `projects`, `launch`, `workspaces`, `mcp`, `acp`,
  `boot`/`boot-exec`/`generate-boot`, `resolve`. `main.go` just executes
  `rootCmd`; `root.go` wires the command list.
- **`internal/app/`** — composition root (`Service`). Start here to trace how
  the pieces wire together.
- **`internal/`** — implementation packages (see Domain concepts below).
- **`docs/dev-setup.md`** — full dev setup, catalog schema, common tasks.
- **`docs/api/README.md`** — HTTP/UDS daemon API reference.
- **`docs/adr/`** — architecture decision records (ADR 0000–0035); read these
  before changing transport, provider, sandbox, or MCP behavior.

## Key domain concepts

- **Daemon (`muxd`)** — long-lived per-user process. Holds the live
  `RuntimeManager` with real subprocess handles. Owns session state. Listens
  on `~/.tether/run/muxd.sock` (UDS) by default. Session-mutating operations
  must route through the daemon (ADR 0035) to avoid split-brain.
- **Session** — a launched, attachable agent process. Lifecycle: create →
  launch → attach/input/resize → stop, with checkpoint/resume. State enum in
  `internal/session/`.
- **Logical agent** — a runtime-registered agent identity that can be
  checkpointed and resumed across concrete sessions (`internal/agent/`).
- **Provider** — a `Runtime` + `Session` adapter for a backing CLI. Lives in
  `internal/provider/`: `cli/claudecode` (PTY-based), `cli/claudestream`
  (subprocess stream-json), `cli/goprovider` (wraps the `go-providers`
  CLIAdapter for claude/codex/kiro/opencode/etc.), `api/stub` (test no-op).
- **Broker** — typed envelope delivery between sessions
  (request/response/notice/escalation/handoff/status_update) with a
  request/reply blocking dispatcher. The legacy `/broker/*` surface.
- **Messaging Store** — Tether implements the `messaging.Store` contract from
  `go-messaging`, backing the `/messages/*` HTTP routes. This is the
  portfolio-wide post office that external clients (Nanite via
  `go-tether-client`) call.
- **Catalog** — user-managed YAML at `~/.tether/catalog/` defining projects,
  agent profiles, providers, and launch configurations. The daemon exposes a
  read-only projection at `/catalog/*`. See `examples/catalog/`.
- **Sandbox** — execution confinement: macOS `sandbox-exec` (SBPL) and Linux
  `bwrap`, driven by per-provider profiles (ADR 0013).
- **Event bus** — pub/sub `Bus` with a SQLite persister; `/events/*` SSE +
  historical query (session.state_changed, broker.envelope_*, daemon.*).
- **MCP adapter** — `mux mcp` exposes the runtime as MCP tools over stdio for
  LLM agents (ADR 0019); a proxy aggregator can flatten other MCP servers
  (ADR 0020/0026). Session-mutating tools route through the daemon (ADR 0035).
- **ACP surface** — Agent Client Protocol support (ADR 0034).
- **Boot prompts** — `generate-boot` / `boot` / `boot-exec` render and launch
  boot prompts from catalog boot profiles.

**Key data flow:** `CLI → daemon UDS → api.Handler → app.Service →
runtime.Manager → provider.Session`.

## Common operations

Build and quality gate (Go 1.26+):

```bash
make build      # produces bin/mux
make install    # installs mux to $GOBIN (use this so `mux` reflects current code)
make check      # fmt + vet + lint + test-race + vuln — the full gate
make test       # fast iteration loop (no race detector), writes coverage.out
make coverage   # print aggregate coverage from the last test run
```

Run the daemon and sessions:

```bash
mux daemon start                     # start muxd
mux daemon status                    # check daemon health
mux sessions launch <launch-profile> # launch from a catalog launch profile
mux sessions list                    # list sessions
mux sessions attach <session-id>     # attach to a running session
mux sessions stop <session-id>       # stop a session
```

Boot prompts and MCP:

```bash
mux generate-boot <profile_id>       # render a boot prompt to stdout
mux boot-exec <profile_id>           # direct boot-exec CLI mode
mux mcp                              # start the MCP stdio adapter
```

The MCP adapter needs `AGENT_MUX_MCP_TOKEN` and `AGENT_MUX_MCP_SCOPES` (e.g.
`session.write,message.write`) in its environment — see `docs/mcp.md`.

## Where to look for more

- **Architecture decisions:** `docs/adr/` — ADR 0002 (daemon transport),
  ADR 0013 (sandboxing), ADR 0018 (broker envelope types), ADR 0019 (MCP
  stdio adapter), ADR 0029 (api/provider reframe), ADR 0031 (TUI removal),
  ADR 0034 (ACP surface), ADR 0035 (MCP daemon routing) are load-bearing.
- **Roadmap & planning:** `planning/docs/roadmap.md`, `planning/docs/epics/`,
  `planning/docs/sprints/`. Note: v0.0.5 (MCP + provider integration) shipped;
  v0.1 (session routing + provider surface) is next.
- **API reference:** `docs/api/README.md`.
- **Sandboxing:** `docs/sandboxing.md`.
- **MCP:** `docs/mcp.md`. **ACP:** `docs/acp.md`.
- **Catalog schema & dev setup:** `docs/dev-setup.md`,
  `docs/catalog-launch-profiles.md`, `docs/agent-config-reference.md`.
- **Contribution workflow:** `CONTRIBUTING.md`.
- **Portfolio knowledge base:** `~/dev/agent-os/knowledge/projects/tether.md`.

> Historical TUI note: Tether once shipped a Bubble Tea TUI under
> `internal/tui/`. It was deleted in full (ADR 0031, sprint v005-06). The
> human-facing interactive surface is a separate native GUI track; do not
> reintroduce a TUI here.
