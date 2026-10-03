# Tether

Tether is the local agent session control plane: a per-user daemon (`tetherd`) that
owns session lifecycle, PTY and process management, sandboxed execution,
checkpoint/resume, brokered messaging and event streams for CLI-backed agents.
Clients reach it over a Unix socket through the `tether` CLI, the HTTP API, the MCP
stdio adapter, the ACP surface or `go-tether-client`. It is the agent session, mesh
and orchestration layer; agent authorship and the business meaning of work remain
with its consumers.

## Start Here

- `cmd/tether/root.go` wires the command tree; `main.go` only executes it.
- `internal/app/` is the composition root (`Service`) — trace wiring from here.
- `internal/api/` owns the HTTP/UDS surface and the typed error envelope
  (ADR 0010); `docs/api/README.md` is its reference.
- `internal/registry/` is the federation directory service.
  `internal/launchresolve/` is a different thing wearing a similar name: the
  read-only catalog walk behind the launch path, renamed out of `registry` to
  free that name.
- `internal/messaging/` owns durable participants, canonical sessions and leased
  runtime bindings.
- `internal/store/migrations/` holds numbered SQL applied in order.
- Architecture decisions (ADRs) are cited by number in code comments and docs
  (for example ADR 0010, 0035, 0041). The records are no longer in this tree;
  where a comment cites one, treat the stated rule as binding and check the
  surrounding code and tests before changing that behavior.
- `docs/dev-setup.md` covers the toolchain; `docs/` holds the user and
  integration guides.

## Commands

```bash
go test ./internal/<pkg>     # smallest run for the changed package
make check                   # fmt + vet + lint + test-race + vuln + coverage
make -C apps/sysop all       # separate module; the root gate does not reach it
```

`make check` is the gate CI runs. Run it before opening a pull request; a
maintainer will review the PR.

## Boundaries

State lives under two roots, and the catalog decides which. `~/.tether/` holds
`catalog/` and `run/tetherd.sock`; the state DB, tmp and a second workspaces tree
live under `~/tether/`, per `defaults.state_db` in `~/.tether/catalog/global.yaml`.
Read that file rather than assuming a path — a stray 0-byte `state.db` under
`~/.tether/` opens as an empty database instead of an error.

Session-mutating operations route through the daemon (ADR 0035). A client that
mutates session state directly splits brain with the daemon's live runtime handles.

The registry holds identity and a callback URI, never operational content.
Substrate catalog YAMLs carry plaintext OAuth tokens in `resources[].config.env`,
so `registry_entries` deliberately has no `cached_payload_json` column (ADR 0041). Never reintroduce payload caching, and never copy `resources[]`, `config`
or `env` into the registry.

`apps/sysop/` is a separate Go module with a Vite frontend. The root `./...` does
not compile or test it, so a change to shared HTTP shapes can leave it red while
the root gate is green.
