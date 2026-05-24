# ADR 0013 — Sandboxing Model and Platform Strategy

**Status:** accepted
**Date:** 2026-04-21
**Supersedes:** —
**Superseded by:** —

## Context

Tether launches untrusted (or semi-trusted) autonomous agents. Without a
sandbox, every session has:

- Full filesystem read/write access to everything the daemon user owns.
- Unrestricted outbound network access.
- The ability to spawn arbitrary child processes.

This is acceptable for local development against a trusted catalog, but unacceptable for multi-project or multi-user dogfooding — a misbehaving agent can exfiltrate SSH keys, overwrite source trees, or reach internal services. We need a configurable enforcement layer that constrains what a session can do without preventing legitimate agent work (reading its workspace, calling allowed APIs, etc.).

The config schema already has:

```go
type AgentPermissions struct {
    Network        bool   `yaml:"network" json:"network"`
    DefaultSandbox string `yaml:"default_sandbox" json:"default_sandbox,omitempty"`
}
```

No runtime code path consumes either value in v0.0.3. This ADR defines the model, platform strategy, resolution flow, and degradation policy that v0.0.4 Sprint 3 will implement.

## Decision

### Scope model

Three orthogonal scopes, all controlled by a named **Profile**:

| Scope | Mechanism | Default |
|-------|-----------|---------|
| **Filesystem** | Path whitelists (read + write). Everything else: default-deny. | session workspace (read+write) |
| **Network** | Boolean allow/deny all outbound. Future versions may refine to per-host ACLs. | deny |
| **Subprocess** | Boolean allow/deny spawning children beyond the agent binary itself. | allow (providers legitimately shell out: git, npm) |

Profiles do not compose or inherit — one flat profile per agent. Composition is deferred.

### Profile registry

Profiles are YAML files under `<catalog-root>/sandbox-profiles/`:

```
~/.tether/catalog/
  sandbox-profiles/
    workspace-only.yaml
    workspace-plus-net.yaml
    unrestricted.yaml
```

Each file:

```yaml
id: workspace-only
description: "Default for most agents. Write workspace only."
fs:
  read:
    - workspace                          # session workspace root
    - ${HOME}/Library/Application Support/Claude  # Claude CLI config
  write:
    - workspace
  deny:
    - ${HOME}/.ssh
    - ${HOME}/.aws
    - ${HOME}/.config/gh
net: false
subprocess: true
```

`AgentPermissions.DefaultSandbox` holds a profile `id`. Empty string means "no sandbox" (unrestricted). The loader populates `Catalog.SandboxProfiles map[string]sandbox.Profile` at startup. A reference to an unknown profile fails catalog validation — `mux daemon start` refuses to start.

The catalog ships three seed profiles:

| id | filesystem | network | subprocess |
|----|-----------|---------|-----------|
| `workspace-only` | workspace r/w + Claude config r | deny | allow |
| `workspace-plus-net` | workspace r/w + Claude config r | allow | allow |
| `unrestricted` | no restrictions | allow | allow |

`unrestricted` is an explicit opt-in for debugging. It does NOT bypass the sandbox layer — it generates a profile that permits everything, so the sandbox binary is still in the process chain and can be removed later without a code change.

### Platform strategy

**macOS** — `sandbox-exec` with a generated SBPL profile written to a temp file per session. `sandbox-exec` is Apple-deprecated (as of macOS 12) but still functional in macOS 15 Sequoia. We accept the deprecation risk because:
- It is the only user-space option that does not require SIP bypass or a kernel extension.
- The migration path to XPC/App Sandbox is significant and out of scope for v0.0.4.
- A future ADR can supersede this one when Apple removes `sandbox-exec`.

**Linux** — `bwrap` (bubblewrap) if present in `$PATH`. `bwrap` translates `Profile.Fs` into `--bind`/`--ro-bind` args and `Profile.Net == false` into `--unshare-net`. If `bwrap` is absent, launch fails (see Degradation policy). `landlock` is a possible future fallback; deferred.

**Windows** — not supported in v0.0.4. Any profile reference on Windows triggers the degradation error.

**Unsupported platforms** — same hard error as Windows.

### Resolution flow

```
CreateSession (app.Service)
  └─ Resolve(catalog, launchInput) → Plan   [permissions NOT in Plan]

LaunchSession (app.Service)
  ├─ Store.GetLaunchPlan(sessionID) → Plan
  ├─ Look up agent: Catalog.Agents[plan.LogicalAgentID]
  ├─ agent.Permissions.DefaultSandbox → profile name
  ├─ Catalog.SandboxProfiles[name] → *sandbox.Profile (nil if name == "")
  └─ runtime.StartRequest{…, SandboxProfile: profile}
       └─ runtime.Manager.Start → StartOptions{…, Sandbox: profile}
            └─ provider adapter: sandbox.Apply(cmd, *opts.Sandbox, workspace)
```

Profiles are resolved from the **live catalog at LaunchSession time**, not persisted in `launch_plans`. If the catalog changes between CreateSession and LaunchSession, the new profile applies. Acceptable tradeoff for v0.0.4; a future version may snapshot the profile into the plan.

### Degradation policy

**No silent downgrade.** If a profile is requested and the platform cannot enforce it, the launch is rejected with a `conflict` typed error (ADR 0010) and a descriptive message. The session is left in `created` state; the caller can retry with `DefaultSandbox: ""` to opt out explicitly.

This means:
- macOS without `sandbox-exec` → error (very unlikely; it ships with every macOS).
- Linux without `bwrap` → error.
- Windows / unsupported → error.
- Profile not found in registry → error (caught at catalog validation, before any launch).
- SBPL/bwrap-args generation error → error.
- `sandbox-exec` itself exits with an error before the agent starts → error surfaced as launch failure.

Operators who want unrestricted behavior must set `DefaultSandbox: ""` explicitly — omitting the field is the same as setting it to empty.

### Failure mode

Launch failures from sandbox enforcement are surfaced as `conflict` (HTTP 409, error code `conflict`) with a human-readable `message` field explaining the cause. This matches the typed-error contract from ADR 0010.

## Rationale

**Why not seccomp/landlock directly?** seccomp requires careful syscall-list curation; landlock is Linux-only and requires kernel ≥5.13. Both would give better long-term security properties, but their ergonomics are far worse for a v0 tool where the profile needs to be human-readable and easy to extend. `sandbox-exec`/`bwrap` are higher-level and ship with the platforms we target.

**Why flat profiles, not per-permission overrides?** Per-permission overrides in agent configs (`network: false` overrides the profile's `net: true`) add merge logic that is hard to reason about. Flat named profiles are explicit, auditable, and easy to diff.

**Why hard-fail instead of warn-and-continue?** The point of sandboxing is to provide guarantees, not hints. A "warn-and-continue" mode would be silently disabled on any new platform, eroding trust in the guarantee. Hard-fail keeps the contract simple.

**Why profile name in Plan is not persisted?** The plan is the launch contract for the session. Permissions are catalog configuration, not session-specific state. Keeping them separate means a permission change takes effect on the next launch without migrating existing plan JSON.

## Implementation note (v0.0.4)

The macOS SBPL generator uses **default-allow + selective deny** rather than the default-deny posture described above. Default-deny on macOS requires enumerating a large, OS-version-dependent allowlist of system paths, dyld caches, and Mach/XPC services that every modern process implicitly needs. Characterizing that list correctly is out of scope for v0.0.4.

The practical effect: outbound network (when `net: false`) and sensitive FS paths (Deny list) are blocked; general filesystem and process operations are permitted. This still closes the primary threat vectors (SSH key exfiltration, cloud-credential reads, unintended network calls) while being reliable in practice.

A future sprint can tighten to default-deny with a well-tested allowlist. When that happens, supersede this ADR.

## Consequences

- `provider.StartOptions` gains `Sandbox *sandbox.Profile`. All provider adapters must handle it (no-op if nil). Build fails loudly on missing implementations.
- `runtime.StartRequest` gains `SandboxProfile *sandbox.Profile`. Passed through from `app.Service`.
- `config.Catalog` gains `SandboxProfiles map[string]sandbox.Profile`. Loaded from the catalog dir alongside agents/projects/providers/launches.
- `config.Validate()` gains a check: every agent's `DefaultSandbox` reference must exist in `SandboxProfiles`.
- New package `internal/sandbox/` with platform-specific build tags.
- New examples: `examples/sandbox-profiles/*.yaml`.
- CI: macOS runners need `sandbox-exec` (ships with macOS). Linux runners need `apt-get install bubblewrap`.

## Follow-ups

- When Apple removes `sandbox-exec`: supersede this ADR with an XPC/App Sandbox approach.
- Per-host network ACLs (e.g., allow only `api.anthropic.com`) — deferred. Sibling primitive `go-egress-proxy` is the planned mechanism (composes with `go-sandbox` via `HTTP_PROXY`/`HTTPS_PROXY` env injection).
- `landlock` as a Linux fallback for systems without `bwrap` — deferred.
- Profile composition / inheritance — deferred.
- Snapshot profile into plan JSON for deterministic replay — deferred.

---

## Addendum (2026-04-27): go-sandbox library adoption (Sprint v005-02)

**Migration:** `internal/sandbox/` was deleted in favor of `github.com/hollis-labs/go-sandbox` v0.1.0. This ADR's contract — `Profile`, `FSSpec`, `LoadProfile`/`LoadProfiles`, three seed profile names — is preserved verbatim by the library; the swap is implementation-level. Profile YAML files in `<catalog-root>/sandbox-profiles/` continue to work unchanged.

**API delta absorbed:** `Apply(cmd, Profile, workspace) error` → `Apply(cmd, Profile, workspace) (cleanup func(), error)`. The cleanup function removes the temp SBPL profile file on macOS (no-op on Linux). Per-adapter cleanup plumbing was added: `claudecode` stores the cleanup on `cliSession` and fires it via `sync.Once` in `Wait()`/`Stop()`; the per-turn adapters (`claudestream`, `opencode`, `goprovider`) capture cleanup in `SendInput` and defer it in `readTurn`/`SendInput` so it fires after each turn's `cmd.Wait()`.

**Hardening absorbed (free upgrades over v0.0.4 implementation):**

1. **`validateSeatbeltLiteral`** — non-optional validator rejects `"`, `\`, `()`, `;`, `'`, control chars in workspace path & FS entries, preventing SBPL profile-string injection. Mux's prior implementation had no literal validation.
2. **bwrap narrowed `--ro-bind`** — limited to `/usr`, `/lib*`, `/bin`, `/sbin`, and 8 specific `/etc/*` files (was: blanket `--ro-bind /etc /etc`).
3. **Linux namespace unsharing** — adds `--unshare-pid`, `--unshare-ipc`, `--unshare-uts`, `--unshare-cgroup-try`, `--unshare-user-try`. Mux's prior implementation was missing the unshares beyond `--unshare-net`.
4. **Per-invocation `--tmpfs /tmp`** — prevents cross-session temp-file leakage on Linux.
5. **`--die-with-parent`, `--new-session`** — prevent orphan escape and TTY hijacking on Linux.
6. **Temp SBPL profile file cleanup** — the prior `internal/sandbox/macos.go` self-documented a leak: every sandboxed session left a `/tmp/<random>.sb` file behind. The cleanup-func return value fixes the regression at the source.

**Why an addendum, not a supersede?** The decision-level content of this ADR — scope model, profile registry shape, seed profile names, platform strategy, resolution flow, degradation policy — is unchanged. The library swap is an implementation refactor that absorbs hardening; the ADR's contract still describes the system's behavior accurately.

**Sequencing context:** This is Track A of the portfolio-libs adoption (decision: `decisions.portfolio.go_agent_sessions_composition_library`). Phase 2 will lift the runtime layer onto `go-agent-sessions` v0.1.0 — at that point, the per-adapter `sandboxCleanup` plumbing introduced here dissolves into the library, and `Profile` is consumed via the same library re-export rather than directly from `go-sandbox`. No further ADR changes anticipated.

**Default-deny revisit deferred** as before — the v0.0.4 implementation note about default-allow + selective deny on macOS still stands. `go-sandbox` ships the same posture for the same reason. A future sprint that builds the well-tested allowlist will supersede this ADR.

---

## Addendum (2026-05-09): loopback enablement for workspace-plus-net

Mux now consumes `go-sandbox v0.2.0`, which added `Profile.AllowLoopback` and Linux `LoopbackForwardPorts`.

For mux's current launch model, the relevant behavior is simple: when the resolved sandbox profile is `workspace-plus-net`, mux force-sets `AllowLoopback=true` before launch. This keeps localhost MCP-style traffic available under the same profile shape even as the shared sandbox library evolves.

On profiles where `net: true`, `AllowLoopback` is effectively a no-op today, but mux still records the intent explicitly because the provider/session layer now assumes loopback is part of the allowed local control plane.
