# Tether Release Readiness

This document tracks the current release-facing cleanup required to ship
Tether and Sysop as a coherent OSS release.

## Release Posture

- License: MIT
- Brand: Tether / Hollis Labs trademarks reserved separately
- Commercial model: software is free to use; revenue comes from services,
  support, consulting, and future adjacent offerings rather than enterprise
  license restrictions

See [../LICENSE](../LICENSE) and [../TRADEMARK.md](../TRADEMARK.md).

## Ship Criteria

The release is ready when all of the following are true:

- `mux` builds and tests cleanly from the repo root
- Sysop builds cleanly from `apps/sysop/` with its embedded frontend
- public docs consistently use the Tether name and `~/.tether/` paths
- release-facing docs explain that Sysop ships with Tether
- stale internal-only or historical material is either archived clearly or
  removed from public-facing paths

## Immediate Worklist

### 1. Licensing and branding

- [x] Replace placeholder root license with MIT
- [x] Add trademark guidance for Tether / Hollis Labs branding
- [x] Update root README to explain MIT plus trademark reservation

### 2. Clean install and build story

- [ ] Make the root release/build path clearly cover both `mux` and Sysop
- [x] Fix Sysop nested-module build so `cd apps/sysop && go build ./cmd/tether_sysop` is green
- [ ] Decide whether the root Makefile should gain explicit Sysop build/install targets
- [ ] Add one documented release build sequence for local verification

### 3. Public doc consistency

- [ ] Sweep remaining public docs for `Agent Mux` naming where it is no longer intentional
- [ ] Sweep remaining public docs for stale `~/.agent-mux/` paths where they are no longer intentional
- [ ] Remove or archive stale `mux tui` references from public documentation
- [ ] Refresh API and MCP docs to match the current Tether naming while preserving protocol details

### 4. Repo cruft cleanup

- [ ] Separate public documentation from internal planning/handoff/agentic scratch material
- [ ] Review `.nanite/`, planning handoffs, and historical notes for what should remain private, archived, or be rewritten
- [ ] Prune stale generated artifacts and obsolete references that make the repo look less shippable

## Verification

Core runtime:

```bash
go build ./cmd/mux
go test ./...
```

Sysop:

```bash
cd apps/sysop
make all
cd apps/sysop && go build ./cmd/tether_sysop
```
