# Tether

Tether is the local agent session control plane: a per-user daemon (`muxd`) that
owns session lifecycle, PTY and process management, sandboxed execution,
checkpoint/resume, brokered messaging and event streams for CLI-backed agents.
Clients reach it over a Unix socket through the `mux` CLI, the HTTP API, the MCP
stdio adapter, the ACP surface or `go-tether-client`. It is the runtime, not the
orchestrator: it does not own tasks, workflows, agent authorship, or the business
meaning of the messages it delivers.

## Start Here

- `cmd/mux/root.go` wires the command tree; `main.go` only executes it.
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
- `docs/adr/` owns transport, provider, sandbox, MCP, registry and messaging
  decisions. Read the relevant record before changing that behavior.

## Commands

```bash
go test ./internal/<pkg>     # smallest run for the changed package
make check                   # fmt + vet + lint + test-race + vuln + coverage
make -C apps/sysop all       # separate module; the root gate does not reach it
```

`make check` is the gate CI runs, and is required before any commit that closes
a task.

## Boundaries

State lives under two roots, and the catalog decides which. `~/.tether/` holds
`catalog/` and `run/muxd.sock`; the state DB, tmp and a second workspaces tree
live under `~/tether/`, per `defaults.state_db` in `~/.tether/catalog/global.yaml`.
Read that file rather than assuming a path — two 0-byte `state.db` decoys sit
under `~/.tether/`, and opening one reports an empty database instead of an error.

Session-mutating operations route through the daemon (ADR 0035). A client that
mutates session state directly splits brain with the daemon's live runtime handles.

The registry holds identity and a callback URI, never operational content.
Substrate catalog YAMLs carry plaintext OAuth tokens in `resources[].config.env`,
so `registry_entries` deliberately has no `cached_payload_json` column (ADR 0041,
D18). Never reintroduce payload caching, and never copy `resources[]`, `config`
or `env` into the registry.

`apps/sysop/` is a separate Go module with a Vite frontend. The root `./...` does
not compile or test it, so a change to shared HTTP shapes can leave it red while
the root gate is green.
