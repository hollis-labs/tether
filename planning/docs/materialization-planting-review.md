# Materialization / Planting Mechanisms — Review Input

## Ask

Review the file/directory planting ("materialization") mechanisms currently
implemented across the portfolio, and settle on one combined approach for the
shared libraries (agentkit / go-agent-wrapper). Confirm sandboxing
(workspace-scoped execution boundary) is supported by whatever is settled on,
whether it already exists in the shared libs or needs to be added.

## Three planting mechanisms found

### 1. go-agent-wrapper `plant.Spec` / `Planter`

- Location: `libs/go-agent-wrapper`, package `plant`.
- `Spec.Files`: flat `map[string][]byte` — individually named files, no
  directory-tree entries.
- No file-mode/exec-bit field on `Spec`.
- No recursive/arbitrary-directory planting.
- Called directly by `Wrapper.Run` immediately pre-exec (plant → adapter
  dispatch → session start → `sandbox.applied` event).
- Module version: v0.9.1 (go.mod). `plant` package unchanged since the
  module's initial v0.1.0 commit (single git log entry).

### 2. agentkit `agentlaunch.BootSpec` + `Materializer`

- Location: `libs/agentkit`, package `agentlaunch`, files `bootspec.go` /
  `materialize.go`.
- `BootFileSpec.Mode os.FileMode` (bootspec.go:242) and
  `BootInjectionSpec.Mode` (bootspec.go:291) — file-mode field present.
- Two phases: build / session-start.
- `Populate` / `Replant` API — idempotent, supports partial slot-level
  re-plant.
- No directory-tree/recursive-planting entry type — every entry is
  individually typed and IDed.
- Consumed today by Tether's launch path: `internal/app/shared_launch.go` and
  `internal/bootexec/claude.go` call `agentlaunch.launcher.Compile →
  launcher.Prepare → agentlaunch/providerplant.Plant`.
- Not consumed by Cairn (grep for `Materializer`/`BootSpec`/`Populate(`/
  `Replant(` across Cairn's repo returns zero hits).

### 3. Cairn `bootdir` (+ `profile.Spec.Trees()`)

- Location: `~/dev/projects/cairn`, packages `profile`, `bootdir`.
- `profile.Spec.Trees()` + `bootdir/trees.go`'s `CopyTree`: recursively walks
  a declared source directory and emits one file entry per file found —
  contents opaque to Cairn. This is the only recursive/arbitrary-directory
  planting found in the audit.
- Own `File.mode()` handling for exec bits, independent of agentkit's `Mode`
  fields.
- `template.go` / `templates.go`: assembles multiple slot outputs into one
  final rendered document (e.g. CLAUDE.md/AGENTS.md), owns document/section
  order; a profile only supplies slot content.
- `profile.go` / `merge.go` / `resolve.go`: implements an `extends` cascade
  for profile inheritance — not present in agentkit's flat, non-inheriting
  catalog.
- Produces a directory on disk and stops — no process-launch step (no
  `exec.Command`/`os/exec`/`syscall.Exec` anywhere in `cmd/` or `bootdir`).

## Capabilities found in Tether's current implementation

- `config.LaunchInjection` / `InjectedFile` (`internal/config/model.go:290-
  309`): generic single-file planting primitive — `Kind`, `RelPath`,
  `Content` or `Source`, plus a `Mode uint32` field. (Unverified whether
  `Mode` threads all the way through `providerplant.Plant`.)
- `mux launch --torque-task <id>` (`cmd/mux/torque_tasks.go:22-82`): plants a
  hand-enumerated set of files (`tasks/<id>/{task.json,task.md,process.md}` +
  `tasks/README.md`) into the materialized boot dir via `NativeFiles` —
  directory-shaped content planted today without a general tree-copy
  primitive (each file is individually named, not copied from an arbitrary
  unknown directory).
- Sandbox: `~/.tether/catalog/sandbox-profiles/workspace-only.yaml` scopes
  `fs.read`/`fs.write` to the materialized workspace directory (plus two
  app-config dirs), with an explicit deny-list (`.ssh`, `.aws`,
  `.config/gh`, `.gnupg`) and `net: false`. The sandbox filesystem boundary
  and the materialized workspace directory are the same boundary in this
  implementation. Wiring touches `internal/app/launch_prepare.go`,
  `session_lifecycle.go`, `service.go` (not traced line-by-line).

## Sandbox — open item

Confirm whether workspace-scoped sandboxing (as above) exists in
go-agent-wrapper/agentkit's own contracts, or in Cairn/Nanite/Torque's
current implementations, or only in Tether's implementation today. Add
native support for it to whichever combined approach is settled on if it is
not already present.

## Not yet reviewed

Nanite's and Torque's current planting/materialization implementations were
not audited in this pass.

## Related prior material

`planning/docs/agent-mux-vfuture-context-pack/` — an earlier (pre-rename)
vision document set for Tether (then "Agent Mux") naming "workspace
materialization" as an already-aligned v0 concern and describing Tether as
"the execution substrate" other systems (then-named Clockwork, Agent Ops,
Nanite, Vanta, Cerberus) build on. Not reviewed in depth in this pass;
flagged as potentially relevant prior art for the combined design.
