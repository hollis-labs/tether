# Sprint v005-06 — TUI Removal

**Epic:** post-v0.0.4 foundation work — narrow Mux to its long-lived CLI session control-plane role
**Status:** SHIPPED 2026-05-11 (FF-merged to `main`; branch deleted)
**Branch:** `feat/v005-06-tui-removal`
**Dependency:** v005-05 long-lived integration merged to `main`
**Date:** 2026-05-11

---

## Goal

Delete the Bubble Tea TUI from Mux. Pure delete sprint — no replacement in scope. The Mux native GUI (Clockwork-template-based) is on a separate track and is the eventual user-facing surface. Programmatic consumers (CLI / MCP / HTTP / future ACP) are unaffected.

51 Go files / ~9,287 LOC under `internal/tui/`, plus `cmd/mux tui` subcommand, plus 3 Bubble Tea / Charm dependency lines in `go.mod`, plus 2 design docs to relocate. One outside-import (`cmd/mux/tui.go` → `internal/tui/*`); 3 comment-only references in `cmd/mux/mcp.go`. Clean, bounded deletion.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-06-tui-removal.md` — concrete audit results, phase plan, acceptance.

---

## Tasks

- [x] **T-v005-s06-01** — Audit confirmed: 51 files / 9 subpackages under `internal/tui/`; only `cmd/mux/tui.go` imports from outside the tree.
- [x] **T-v005-s06-02** — `git rm -r internal/tui/` + `git rm cmd/mux/tui.go`; `tuiCmd` dropped from `cmd/mux/root.go:20`. `go build ./...` green.
- [x] **T-v005-s06-03** — `cmd/mux/mcp.go` comment block (~lines 113–128) reworded to frame the daemon event bus as the destination for any consumer, not specifically the TUI. No logic changes.
- [x] **T-v005-s06-04** — Both `docs/superpowers/{plans,specs}/2026-04-20-tui-split-panel*.md` moved to `docs/historical/` via `git mv`. Markdown TUI references updated in `README.md`, `docs/api/README.md`, `docs/mcp.md`; ADRs left as historical record (paper trail); `docs/sandboxing.md` "bubblewrap" (Linux sandbox) and `pkg/claudestream/README.md`'s reference to claude's own interactive TUI left as-is (unrelated to mux's TUI).
- [x] **T-v005-s06-05** — `go mod tidy` removed `charmbracelet/bubbles`, `charmbracelet/bubbletea`, `charmbracelet/lipgloss`, and four transitive Charm packages (`colorprofile`, `x/ansi`, `x/cellbuf`, `x/term`). `grep -E "charmbracelet|bubble|lipgloss" go.mod` returns nothing.
- [x] **T-v005-s06-06** — ADR 0031 written at `docs/adr/0031-tui-removal.md`, cross-referencing Vanta entries.
- [x] **T-v005-s06-07** — `make check` green (448 tests, 6 expected skips, 0 vulns). All non-TUI subcommands respond to `--help`. Parent boot prompt updated. FF-merged; branch deleted; `make install` reported.

---

## Acceptance

- [x] `internal/tui/` and `cmd/mux/tui.go` deleted
- [x] `cmd/mux/root.go` no longer references `tuiCmd`; build green
- [x] `cmd/mux/mcp.go` comments updated
- [x] Two TUI design docs moved to `docs/historical/`
- [x] `go.mod` no longer references Bubble Tea / Charm deps
- [x] No `*.md` recommends running `mux tui`
- [x] ADR 0031 committed
- [x] `make check` green; all non-TUI subcommands build + `--help` works
- [x] FF-merged to `main`; branch deleted; parent boot prompt updated
- [x] `make install` to `/Users/chrispian/go/bin/mux` reported

---

## Out of scope

- Mux native GUI MVP (separate track)
- Lib-tier adoption (v005-07)
- Agent Ops (v005-08)
- ACP surface (v005-09)
- Hardening / modernization (v005-10)
- Docs push (v005-11 planned)
- Any new functionality

---

## Notes

- Pure delete. If a refactor opportunity surfaces while deleting, capture as a follow-up and move on.
- Should be one focused session; possibly one commit cluster.
