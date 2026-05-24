# Contributing to Tether

Tether is pre-release software. Contributions are welcome; the bar is
correct, minimal, well-tested Go that passes the full quality gate.

## Before you start

- Read [`docs/dev-setup.md`](docs/dev-setup.md) to get your toolchain in order.
- Skim [`docs/api/README.md`](docs/api/README.md) for the public HTTP/UDS
  surface area and [`docs/adr/`](docs/adr/) for the architectural decisions
  that shape this codebase.
- License: see [`LICENSE`](LICENSE) and [`TRADEMARK.md`](TRADEMARK.md).
  Code contributions are accepted under the repository's MIT licensing posture;
  the Tether and Hollis Labs names remain protected marks.

## Workflow

1. **Open an issue or discussion first** for anything larger than a small bugfix.
2. **Branch from `main`.** Names like `feat/<topic>`, `fix/<topic>`, `docs/<topic>`.
3. **Keep commits small and coherent.** One logical change per commit.
4. **Run `make check` locally** before opening a PR — it is the same gate CI runs.
5. **Fast-forward merges** are preferred. Avoid merge commits when possible.

## Commit messages

Conventional Commits, scoped by package where useful. Examples from the
existing history:

```
feat(runtime): extract Manager owning session handles
fix(provider): merge-by-default env composition
docs(adr): daemon transport scheme-prefixed listen_addr
refactor(app): drop unused attach parameter from Launch
test(runtime): adversarial interleaved lifecycle
```

Keep the subject under ~70 chars. Use the body to explain *why*, not *what*.

## Coding conventions

- **Go version:** track the `go` + `toolchain` directives in
  [`go.mod`](go.mod). If you need a newer Go feature, bump both and note
  the reason in the commit body.
- **Formatting:** `gofmt` is mandatory. `goimports` is encouraged but not
  enforced by CI. `make fmt` runs both.
- **Error handling:**
  - Wrap context with `%w`, not `%v`. Example: `fmt.Errorf("read config: %w", err)`.
  - Test for wrapped errors with `errors.Is` / `errors.As`, never `==` or `!=`.
  - Don't silently discard errors. If you truly don't care (e.g., deferred
    `Close` on a read-only resource), use `_ = x.Close()` or a named
    `defer func() { _ = x.Close() }()`.
- **Concurrency:** Manager-owned state is guarded by mutex. Long-lived
  goroutines hang off `runtime.Manager`, not `app.Service`. If you add a
  goroutine, it needs a lifecycle contract (ctx cancel, explicit close,
  or a WaitGroup hand-off).
- **Comments:** write them when the *why* is non-obvious. Don't narrate
  the *what* — the code already does that. See the `CLAUDE.md` / Go
  idiom of short, declarative godoc at the top of exported identifiers.
- **Package layout:** new primitives go in a small sibling package, not
  in `internal/app`. `app` is a composition root and should stay thin.

## Tests

- Use the standard library `testing` package + `t.Run` subtests.
- Prefer real types over mocks where practical. Fakes live in `_test.go`
  files alongside the code they exercise.
- New code should have tests. Race-sensitive code (attach broker, runtime
  manager, event bus) needs explicit race tests.
- `make test` runs the fast path with coverage. `make test-race` is the
  correctness gate. Both must pass before a PR is ready.

## The quality gate

```
make check
```

This runs:

1. `go fmt ./...`
2. `go vet ./...`
3. `golangci-lint run` — config in [`.golangci.yml`](.golangci.yml)
4. `go test -race ./...` via `gotestsum`
5. `govulncheck ./...`

All five must be green. CI enforces the same gate.

If a lint finding feels like a false positive, prefer a narrow `//nolint:<linter> // <reason>` suppression over disabling the linter globally. Broad config changes to silence noise are reviewed on a case-by-case basis.

## Reporting security issues

Do **not** open a public issue for security reports. Email the maintainer
directly. The contact is in `git log` on this repository.
