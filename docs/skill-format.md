# Skill Format (v005-08)

Skills are markdown files with YAML frontmatter, discoverable via the three-layer agent config stack (`<layer>/skills/<id>.md`).

## File shape

```markdown
---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns appropriate to this codebase.
triggers: [refactor, cleanup, simplify]
---

When refactoring Go code in this repository:

- Keep functions small and single-purpose...
- Prefer table-driven tests with t.Run subtests...
- Use errors.Is / errors.As for sentinel checks...
```

| Field | Type | Required | Purpose |
|---|---|---|---|
| `id` | string | yes | Unique skill identifier; matches the file basename (without `.md`). |
| `name` | string | no | Human-readable name used in rendered output. |
| `description` | string | no | One-line summary inserted near the top of the rendered skill. |
| `triggers` | string list | no | Keywords / activation hints; rendered as `**Triggers:** a, b, c`. |
| body | markdown | no | Free-form content after the closing `---`. EOF-terminated frontmatter (no closing delimiter) is legal but produces an empty body. |

## Per-provider compilation

The skills package compiles a `[]Skill` into `[]CompiledFile` (relpath + content + mode) per provider. Provider IDs are matched case-insensitively against a small alias table:

| Provider IDs | Compiler | Output |
|---|---|---|
| `claude`, `claude-code`, `claude-stream` | `CompileClaude` | One `.claude/skills/<id>.md` per skill. Body is preserved verbatim; the frontmatter is NOT re-emitted (Claude's skill loader expects markdown content). |
| `codex`, `codex-app-server`, `codex-cli` | `CompileCodex` | Single `AGENTS.md` aggregating every skill as a `## Skill: <name>` section. Sections sorted by skill ID for byte-stable output. |
| anything else | _none_ | `CompileForProvider` returns `ErrUnsupportedProvider`. Non-fatal — `applyAgentOps` skips skill compilation when the provider isn't supported, so the session still launches. |

## Transport (v005-08)

For v005-08, compiled skill content is appended to the assembled BootPrompt text (with `<!-- relpath -->` markers per section) and reaches the agent via the standard system-prompt pipeline. Native per-file placement (so Claude reads `.claude/skills/<id>.md` from the planted boot dir, and Codex picks up `AGENTS.md`) waits on a future `go-agent-sessions` enhancement that lets apps inject extra `PlantedFiles` into the BootDirSpec.

## Authoring round-trip

The `skills.WriteSkillFile(w, Skill)` helper emits the canonical frontmatter+body shape so authored files round-trip through `skills.Parse`. Useful in tests and any future `mux skills create` scaffolding.

## See also

- ADR 0033 — Two-Tier Agent Config (skills section #9)
- `internal/skills/skills.go` — implementation
- `examples/catalog/skills/refactor-go.md` — seed fixture
