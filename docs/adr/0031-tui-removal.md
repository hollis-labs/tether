# ADR 0031 — TUI Removal

**Status:** Accepted
**Date:** 2026-05-11
**Supersedes:** —
**Superseded by:** —

## Context

Mux shipped a Bubble Tea TUI under `internal/tui/` (~9.3K LOC across 51 Go files, 9 subpackages — `client`, `detail`, `externshell`, `layout`, `modal`, `palette`, `panel`, `screen`, `theme`) and exposed it via a top-level `mux tui` subcommand. The TUI grew incrementally during v0.0.3 (sprints 1–2 shipped 2026-04-19, sprints 3/5/6 reslotted to v0.1, sprint 4 pulled into v0.0.4). It surfaced session lists, attach panes, palettes, checkpoint modals, tool-call feeds, and detail forms — and quietly accumulated dependencies on the Charm cluster (`bubbles`, `bubbletea`, `lipgloss` plus transitive `colorprofile`, `x/ansi`, `x/cellbuf`, `x/term`).

The 2026-05-11 long-lived-session investigation surfaced a clearer architectural read on what Mux is: a **daemon-shaped session control plane** whose product surfaces are CLI / MCP / HTTP / (forthcoming) ACP / GUI. The TUI was a misunderstanding propagated by prior agent sessions — the user never intended `mux`'s launch path to be a human-facing interactive surface; it is a dev-only alias for boot-prompt testing. The replacement for human-facing interactive work is a separate native GUI track, built on the Clockwork GUI template (Vanta `followup_mux_native_gui_mvp`); not in scope here.

Keeping the TUI in-tree costs in three concrete ways:

1. **Cognitive surface area.** ~9.3K LOC and 9 subpackages of UI scaffolding that future agents must read past when navigating `internal/`, and that test runs, lints, and vuln checks must traverse on every gate.
2. **Dependency weight.** The Charm cluster pulls a chain of terminal-rendering deps with their own update cadence and security surface — paid for a feature we no longer ship.
3. **Architectural misdirection.** Every future contributor who reads the repo from the outside sees `internal/tui/` and assumes the TUI is part of the product. Boot prompts and ADRs already note "TUI is dead" but the code's continued presence contradicts the words.

The audit confirms a clean deletion boundary: only `cmd/mux/tui.go` imports `internal/tui/...`. No other package depends on it.

## Decision

Delete the TUI in full. No archive shadow, no compat shims, no deprecation alias — consistent with the pre-launch "no compat shims" rule.

Concretely:

1. `git rm -r internal/tui/` — all 51 files, 9 subpackages.
2. `git rm cmd/mux/tui.go` — the `mux tui` subcommand. Users invoking it after this change see Cobra's stock "unknown command" message; acceptable.
3. `cmd/mux/root.go` no longer references `tuiCmd` in the `rootCmd.AddCommand(...)` list.
4. `cmd/mux/mcp.go`'s observability comment block (around the `tool_call_end` event-forwarder) is rewritten to frame the daemon's event bus as the destination for any consumer — not specifically the TUI. The forwarding logic itself is generic and stays.
5. The two `docs/superpowers/{plans,specs}/2026-04-20-tui-split-panel*.md` design documents move to a new `docs/historical/` directory, preserving design intent + history but signalling "not the current product."
6. `go mod tidy` drops `bubbles`, `bubbletea`, `lipgloss`, and the four transitive Charm packages from `go.mod` and `go.sum`.
7. User-facing markdown (`README.md`, `docs/api/README.md`, `docs/mcp.md`) is reworded to remove TUI surface descriptions and replace TUI-as-example placeholders with the actual CLI's `client_kind` value (`"cli"`). ADRs retain their historical TUI references — they are the paper trail of decisions made when the TUI existed.

## Consequences

**Positive.**

- `internal/` surface area shrinks by ~9.3K LOC and 9 subpackages.
- `go.mod` loses 3 direct + 4 transitive dependencies. `make check` runs slightly faster; vuln-scan surface shrinks.
- The `mux` binary no longer carries terminal-rendering code in cold paths.
- The repo's outward-facing surface (CLI / MCP / HTTP) is now what `internal/` actually serves. No misleading "the TUI is also a thing" implication.
- Future contributors don't have to evaluate "should I add this to the TUI too?" for any new feature.

**Neutral.**

- Anyone who had a habit of running `mux tui` to inspect sessions will see "unknown command." The intended replacement is `mux sessions list` / `mux sessions get` / `mux sessions wait` for headless inspection, the MCP adapter for editor/agent integration, the HTTP API for programmatic consumers, and the forthcoming native GUI for interactive use. No migration shim because there was no public user base outside this workspace.
- The `client_kind` string column on `client_attachments` retains historical rows that may carry `"tui"` values. Not a concern: it's a free-form string descriptor, not an enum; downstream consumers tolerate unknown values.

**Negative.**

- The two design docs are now in `docs/historical/`, which means anyone navigating to the split-panel design has to know to look in `historical/`. Acceptable trade — the alternative (deleting them) loses the design lineage.
- Git history is the only path back to the TUI code. If a future GUI track ever wants reference material for the split-panel layout or palette logic, they go to `git log -- internal/tui/` and the historical design docs rather than reading live code.

## Implementation

Single sprint: `feat/v005-06-tui-removal`. Single ADR (this one). Two-to-three commit cluster grouping deletes, comment/doc cleanup, and ADR + boot-prompt update. FF-merged to `main` and branch deleted at sprint close.

## References

- Boot prompt: `agent-workspaces/boot/agent-mux/boot-prompt-v005-06-tui-removal.md`
- Sprint file: `agent-mux-v0-pack/docs/sprints/v005-06-tui-removal.md`
- Vanta: `mux_daemon_shaped_thin_wrapper_role`, `followup_mux_tui_full_removal`, `followup_mux_native_gui_mvp`, `mux_beta_push_roadmap_may_2026`
- Historical design docs: `docs/historical/2026-04-20-tui-split-panel.md`, `docs/historical/2026-04-20-tui-split-panel-design.md`
