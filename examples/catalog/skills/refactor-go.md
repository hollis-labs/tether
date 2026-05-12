---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns appropriate to this codebase.
triggers: [refactor, cleanup, simplify]
---

When refactoring Go code in this repository:

- Keep functions small and single-purpose. If a function grows past ~40 lines,
  look for a natural extraction point.
- Prefer table-driven tests with `t.Run(name, ...)` subtests over loose `if`
  ladders in `Test*` functions.
- Use `errors.Is` / `errors.As` for sentinel + typed error checks; avoid
  string-matching error messages.
- Reach for the standard library before adding a dependency. `strings`,
  `sort`, `slices`, `errors`, `io/fs`, and `path/filepath` cover most needs.
- When touching a package, run `go test -race ./<pkg>/...` before committing.
