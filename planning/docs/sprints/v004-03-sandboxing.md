# Sprint v004-03 — Sandboxing

Epic: [v0.0.4](../epics/v0.0.4-dogfood-ready-infrastructure.md)

**Reordered 2026-04-19** — was v004-02; became v004-03 when the claudestream-provider sprint slotted in ahead of sandboxing. Scope unchanged. Sandbox enforcement applies to both the PTY-based provider (`internal/provider/cli/claudecode/`) and the new subprocess-based claudestream provider from Sprint v004-02.

**Goal:** Move sandboxing from spec-only to actually-enforced. Today the config schema has `AgentPermissions.Network bool` and `AgentPermissions.DefaultSandbox string` but no daemon code consumes them. This sprint defines the sandbox model, implements macOS enforcement via `sandbox-exec`, adds the provider-runtime hook, and ships integration tests that assert scope boundaries hold. Linux gets a best-effort pass with a clear unsupported-fallback story.

**Exit criteria:**
- [x] ADR 0013 defines the sandbox model: scopes (filesystem, network, subprocess), platform strategy, degradation policy. *(8e23ebb; implementation note added 1b69245)*
- [x] macOS sessions launched with a non-empty `DefaultSandbox` run under `sandbox-exec` with a generated profile. *(410734e)*
- [x] `AgentPermissions.Network == false` is enforced on macOS (sandbox profile blocks network syscalls). *(410734e — default-allow + network deny; see ADR impl note)*
- [x] Linux gets a best-effort implementation (`bwrap` if available, else document-and-skip). *(410734e)*
- [x] Integration tests verify: a sandboxed session can't read outside its workspace; can't open network connections when `Network == false`. *(d2c0398)*
- [x] Sandbox application failure at launch is a fatal error — the session does NOT start without enforcement. No "silent downgrade." *(ADR 0013; unsupported.go returns error; Apply errors propagate as launch failures)*
- [x] Catalog can reference named sandbox profiles (e.g. `workspace-only`, `workspace-plus-net`) resolved from a profile registry. *(69907ba — LoadProfiles + catalog integration + validation)*
- [x] `make check` green. *(399 tests under -race, 0 issues)*

## Context

Config schema already has the fields:

```go
type AgentPermissions struct {
    Network        bool   `yaml:"network" json:"network"`
    DefaultSandbox string `yaml:"default_sandbox" json:"default_sandbox,omitempty"`
}
```

TUI detail view reads them (`internal/tui/detail/agent.go`). No runtime code path consumes either value. No provider-contract hook exists. No ADR discusses enforcement.

Dogfooding a `claude` agent without sandboxing is risky — the agent has full filesystem + network access wherever the daemon runs. Sandboxing closes the gap for the common case (agent operates inside its workspace and explicitly-allowed paths).

Context-pack anti-goals say "do not build a GUI first" and "do not couple the runtime to one vendor SDK" but don't speak to sandboxing one way or the other. This sprint establishes the model.

## Tasks

### T-v004-s02-01: ADR 0013 — Sandbox model + platform strategy

**Priority:** 1. **Tags:** decision, adr, sandbox.

#### Problem
No document defines what sandboxing means for Agent Mux. Without a model, implementation will drift per-task.

#### Fix direction
Write ADR 0013 covering:

- **Scope model.** Three orthogonal scopes:
  - *Filesystem:* read + write path whitelists derived from workspace + explicit allows. Default-deny.
  - *Network:* boolean (allow / deny). Future versions may refine to per-host ACLs.
  - *Subprocess:* boolean (allow / deny spawning children). Off by default for agents.
- **Platform strategy.**
  - macOS: `sandbox-exec` with a generated SBPL profile. `sandbox-exec` is Apple-deprecated but still functional in current macOS; we accept the deprecation risk because it's the only portable user-space option.
  - Linux: `bwrap` (bubblewrap) if present, else `landlock` wrapper, else document-and-skip.
  - Windows: unsupported in v0.0.4.
- **Profile registry.** Catalog includes a set of named profiles (`workspace-only`, `workspace-plus-net`, etc.) resolved from `~/.agent-mux/sandbox-profiles/*.yaml`. `AgentPermissions.DefaultSandbox` references one by name. Missing profile = hard error at launch.
- **Degradation policy.** If the platform can't enforce a profile, launch fails. No silent downgrade. User can set `DefaultSandbox: ""` to opt out explicitly.
- **Failure mode.** Sandbox-apply errors at launch are surfaced to the client as `conflict` (typed-error code from ADR 0010) with a descriptive message.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0013-sandboxing.md` (new)

#### Acceptance criteria
- [ ] ADR written, dated, in accepted state.
- [ ] Context-pack §10 cross-link to sandboxing decision if helpful.
- [ ] Addresses every policy question above.

#### Scope fences
- Do not design per-host network ACLs in this ADR. Deferred.
- Do not design sandbox profile composition / inheritance. One profile per agent, flat.

---

### T-v004-s02-02: Sandbox profile registry + YAML schema

**Priority:** 1. **Tags:** config, sandbox.

#### Problem
Catalog needs to resolve `DefaultSandbox: "workspace-only"` to an actual profile definition.

#### Fix direction
- New directory: `~/.agent-mux/sandbox-profiles/` (relative to catalog root).
- Each profile is a YAML file with the schema:
  ```yaml
  id: workspace-only
  description: Default for most agents. Write workspace, read HOME minus secrets.
  fs:
    read: [workspace, ${HOME}/Library/Application Support/Claude]
    write: [workspace]
    deny: [${HOME}/.ssh, ${HOME}/.aws, ${HOME}/.config/gh]
  net: false
  subprocess: false
  ```
- `internal/sandbox/profile.go`: typed `Profile` struct + YAML loader.
- `internal/config/loader.go`: load sandbox profiles alongside other catalog types.
- Ship a seed set with the repo under `examples/sandbox-profiles/`:
  - `workspace-only` (no net, no subprocess, strict fs)
  - `workspace-plus-net` (net allowed; the common "can use npm/pip/git" shape)
  - `unrestricted` (for debugging only; explicit opt-in)

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/sandbox/` (new package)
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/loader.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go` (Catalog.SandboxProfiles map)
- `/Users/chrispian/Projects-apps/agent-mux/examples/sandbox-profiles/*.yaml` (new)

#### Acceptance criteria
- [ ] Three seed profiles ship.
- [ ] Loader handles missing dir (returns empty map, not error).
- [ ] Unknown-profile reference at agent load fails validation cleanly.

#### Test plan
- Unit: YAML round-trip.
- Unit: catalog validation rejects unknown `DefaultSandbox` references.

#### Scope fences
- Do not invent profile inheritance / composition.
- Do not hot-reload profiles (restart daemon to pick up new ones for v0.0.4).

---

### T-v004-s02-03: macOS enforcement via `sandbox-exec`

**Priority:** 1. **Tags:** macos, sandbox, provider.

#### Problem
No code applies a sandbox profile on macOS.

#### Fix direction
- `internal/sandbox/macos.go`:
  - `BuildSBPL(profile Profile, workspace string) (string, error)` — generates an `.sb` SBPL text from the profile.
  - `Apply(cmd *exec.Cmd, profile Profile, workspace string) error` — wraps `cmd` to run under `sandbox-exec -p <profile.sb> -- <original cmd>`.
- Provider contract: `provider.StartOptions` gains `Sandbox *sandbox.Profile` (optional pointer; nil means no enforcement).
- `internal/runtime/manager.go`: on launch, look up `AgentPermissions.DefaultSandbox`, resolve via `Catalog.SandboxProfiles`, pass to provider via `StartOptions`.
- Provider startup wraps the real `exec.Cmd` through `sandbox.Apply` before starting.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/sandbox/macos.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/runtime.go` (StartOptions extension)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudecode/adapter.go` (honor Sandbox field)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/stub/stub.go` (honor Sandbox field — no-op is OK for stub)
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go`

#### Acceptance criteria
- [ ] Session launched with `DefaultSandbox: "workspace-only"` runs under `sandbox-exec`.
- [ ] Attempting to `cat ~/.ssh/id_rsa` from inside the session fails with an operation-not-permitted error.
- [ ] Attempting to `curl https://example.com` from a `net: false` profile fails.
- [ ] Non-macOS platforms: the code path no-ops with a "sandbox not supported on this platform" warning logged, but — per ADR 0013 — if the profile is non-empty launch fails hard. Caller decides whether to retry with `DefaultSandbox: ""`.

#### Test plan
- Integration: launch a stub session with a profile; verify sandbox-exec is the parent process of the stub.
- Integration: `cat` test against a blocked path inside a sandboxed session — non-zero exit + EPERM.

#### Scope fences
- Do not try to make the SBPL generator extensible for Sprint 2. Hard-code the mapping from Profile → SBPL for the three seed profiles plus a simple "default-deny + explicit-allow" codegen for user-defined profiles.
- Do not support dynamic re-sandboxing after launch.

---

### T-v004-s02-04: Linux enforcement (best-effort) via `bwrap`

**Priority:** 2. **Tags:** linux, sandbox.

#### Problem
Linux is a viable deploy target but has no enforcement.

#### Fix direction
- `internal/sandbox/linux.go`:
  - Detect `bwrap` in `$PATH`. If absent, `Apply` returns an error; launch fails per ADR 0013 (no silent downgrade).
  - `BuildBwrapArgs(profile Profile, workspace string) ([]string, error)` — translates Profile → `bwrap` CLI args.
  - `Apply` wraps `exec.Cmd` to run `bwrap <args> -- <original cmd>`.
- Document the unsupported fallback: if user is on Linux without `bwrap`, the daemon logs a one-time warning and any profile-referencing agent fails to launch.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/sandbox/linux.go` (new)
- build tags: `//go:build linux` in linux.go; `//go:build darwin` in macos.go; a `stub.go` with `//go:build !darwin && !linux`.

#### Acceptance criteria
- [ ] `bwrap` detection works; missing → clear error.
- [ ] Profile translates to sensible `bwrap` args: `--ro-bind`, `--bind` for writable, `--unshare-net` for `net: false`, `--die-with-parent`.
- [ ] One integration test in a Linux-only CI job (or skipped with `t.Skip` if not on Linux).

#### Scope fences
- Do not implement landlock as a secondary backend for v0.0.4.
- Do not enumerate every Linux distro's bwrap flavor — just stock bwrap.

---

### T-v004-s02-05: Integration tests + docs

**Priority:** 2. **Tags:** tests, docs.

#### Problem
Sandbox correctness is the one thing we absolutely need to verify empirically. Without integration tests that assert scope boundaries, the implementation is untrustworthy.

#### Fix direction
- `internal/sandbox/integration_test.go`:
  - Launch a session under `workspace-only`. Assert: write inside workspace succeeds; read of `~/.ssh/` fails; read of allowed path succeeds.
  - Launch under `workspace-plus-net`. Assert: `curl` to localhost:8080 (or a test server) succeeds.
  - Launch under `unrestricted`. Assert: behaves like no sandbox.
- Tests skip cleanly on unsupported platforms (`t.Skipf`).
- User-facing doc: `docs/sandboxing.md` with the mental model, profile examples, common failure modes, how to add a custom profile.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/sandbox/integration_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/docs/sandboxing.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/README.md` (add sandboxing section pointer)

#### Acceptance criteria
- [ ] Integration tests pass on macOS.
- [ ] Integration tests skip cleanly elsewhere.
- [ ] `docs/sandboxing.md` walks through the three seed profiles + a custom-profile example.

#### Scope fences
- Do not write fuzz tests against the SBPL generator — out of scope for MVP.
- Do not add benchmarks; perf is unlikely to be load-bearing at this stage.

---

## Review / readiness notes

- **`sandbox-exec` deprecation.** Apple deprecated `sandbox-exec` years ago but hasn't removed it. Accept the risk; if it gets removed in a future macOS, v0.1+ can migrate to `xpc` or the newer `NSXPCConnection`-based sandbox APIs. Document this in ADR 0013.
- **Subprocess control.** Blocking subprocess spawn wholesale can break providers that legitimately need to shell out (git, npm). The default for `claude`-like agents is probably `subprocess: true` with network-scoped subprocess. Revisit if the seed profiles feel wrong after dogfooding.
- **Workspace-path resolution.** The workspace is per-session; the sandbox profile needs its path at launch time. Confirm workspace resolution order is settled before T-03 touches the provider.
- **CI impact.** macOS CI needs access to `sandbox-exec` (should be on every runner). Linux CI needs `apt-get install bubblewrap`. Update `.github/workflows/check.yml` accordingly.
