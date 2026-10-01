# Sandboxing

Tether can constrain what a session is allowed to do by running it under a
**sandbox profile**. Profiles restrict filesystem writes to sensitive paths,
outbound network access, and subprocess spawning.

## How it works

Every agent in your catalog can reference a named profile:

```yaml
# ~/.tether/catalog/agents/my-agent.yaml
id: my-agent
name: My Agent
permissions:
  network: true
  default_sandbox: workspace-only   # ← profile name, or omit for no sandbox
```

When the daemon launches a session for that agent:

1. It looks up `workspace-only` in `<catalog-root>/sandbox-profiles/`.
2. It generates a platform-specific sandbox configuration (SBPL on macOS, bwrap args on Linux).
3. The provider wraps the agent process inside the sandbox before starting it.

If the profile is missing or the platform can't enforce it, the launch **fails hard** — there is no silent downgrade.

## Control-plane protection (every agent Tether wraps)

Separately from profiles, on Linux every agent Tether wraps, which is every
agent but Codex (see [Codex is not protected](#codex-is-not-protected)), gets two
of Tether's own directories as read-only protected paths (CW-20261001-0142):

- the catalog root;
- the daemon's run directory, which is the directory of `daemon.pid_file` and
  of a `unix:` `daemon.listen_addr`, when it lies inside a Tether root: the
  catalog root's parent, or `~/.tether`. A pid file configured into a shared
  directory such as `/tmp` is skipped, so that directory does not become
  read-only for every agent.

Both are registered by their real paths. An agent with a profile gets them
added to its profile. An agent without one runs under a minimal profile whose
only effect is the protection: the host filesystem under `bwrap`, writable
except the protected directories. The daemon is not sandboxed and still
writes both.

### Codex is not protected

**Tether's write-protection does not cover Codex.** Tether does not wrap Codex in
its sandbox, and registers no protected paths for it. Tether starts Codex (`exec`
and `app-server`) under the accept-edits posture, which gives it
`sandbox_mode = "workspace-write"`, and that sandbox is bubblewrap too.
Bubblewrap cannot nest where unprivileged user namespaces are restricted
(AppArmor's `kernel.apparmor_restrict_unprivileged_userns=1`, the Ubuntu
default), so wrapping Codex in Tether's sandbox would stop it running any shell
command (`bwrap: No permissions to create a new namespace`). Codex runs exactly
as it did before control-plane protection existed.

Codex's own `workspace-write` sandbox makes the whole filesystem read-only except
its writable roots: its working directory, `/tmp` and `$TMPDIR`, and whatever is
configured. The catalog and run directory are outside those, so a shell command
that writes them fails (CW-20261001-0142 live evidence: a Codex turn's
`touch <catalog>/x` is denied and `touch <workdir>/ok` succeeds, in both `exec`
and `app-server`). **That is not protection from Tether**, and it does not hold
against the rest of what a Codex agent can reach:

**Codex spawns every MCP server it is given outside its sandbox.** The planted
`mux` proxy and every upstream it starts run unsandboxed, with the operator's uid,
so a Codex agent can reach the catalog through MCP tools:

- `torque_session_launch` (`torque mcp`) takes a caller-chosen `workdir`, so
  `workdir=<catalog>` boots a Codex whose own workspace-write root is the catalog.
  Inferred from the real tool, not demonstrated end to end (CW-20261001-0464).
- `loom_export_bundle(dir=…)` (`loom mcp`) wrote `index.md` and `log.md` into an
  arbitrary directory in a reproduction (CW-20261001-0465).
- `nanite` (`dev_bash`, `dev_write`) and `cerberus` are host shells and
  infrastructure control by design, and `tangent.runner_launch` runs any host
  command the caller names, if a project grants them.

So the default MCP allow-list (`torque`, `tesseract`) is not safe for a Codex agent
either. **The structural fix is MCP upstreams that run daemon-side, outside the
agent's reach, or that confine what they write (CW-20261001-0230).** Until it
lands, `GET /health` (`sandbox_protect.codex`), `mux doctor`
(`sandbox-protect-codex`, a warning) and the daemon's startup log all say
`codex: not protected (CW-20261001-0230)`. Claude, OpenCode and every other agent
Tether wraps stay protected, and one guard still holds for Codex: the planted `mux`
server refuses to write the catalog and run directory (see below). The planted
Codex `config.toml` carries `--protect-path <catalog> --protect-path <run>` on its
`mux` server's arguments, so the `mux_agent_create`/`mux_agent_edit` refusal applies
to Codex too (seen live: `catalog_read_only`, nothing written). Otherwise "exactly
as on `main`" is true for Codex's sandbox and for wrapping.

*History.* Earlier revisions of this change left Codex to its own sandbox under an
allowlist and reported it as guarded. Three adversarial reviews found it could not
be made sound while Codex spawns MCP servers outside its sandbox, and that claim is
withdrawn.

**Operator notes.**

- A session already running keeps the MCP configuration planted when it was
  launched until it is relaunched or resumed.
- A session planned before this change carries no record of which environment keys
  came from a caller (`caller_env`), so its plan counts as having none. Nothing
  reads it while Codex is unprotected; it matters only if the dormant guard is
  switched on.

### The dormant Codex guard

The guard Tether built to leave Codex to its own sandbox safely stays in the tree,
**dormant**, behind one switch (`codexProtectionMode`,
`internal/app/protected_codex_mode.go`, `CodexNotProtected` as shipped). It does
nothing until CW-20261001-0230 makes it sound; it is documented here so that
switching it on is a decision with its gaps in view. What follows is what it
checks.

That holds only while the sandbox is Codex's default. Real Codex 0.159.x widens
its own sandbox from each of these, all of them demonstrated: a
`sandbox_workspace_write.writable_roots` override, a `sandbox_mode` of
`danger-full-access` (attached forms such as `-csandbox_mode=…` and `-s=…`
included), `--add-dir` (a relative one resolves against Codex's directory, not
Tether's), a `config.toml` in `CODEX_HOME`, a project `.codex/config.toml`, and a
working directory that contains the catalog. A caller with `session.write` can
reach most of them through `POST /sessions` (`agent_inline` provider overrides,
`injection`, `override.env`). So the exemption is an **allowlist**, and Tether
wraps Codex like any other agent unless all of this holds:

- **Flags.** Only `--model`/`-m`, and `-c`/`--config` overrides of `model`,
  `model_reasoning_effort`, `model_reasoning_summary`, `approval_policy` and a
  `sandbox_mode` of `workspace-write` or `read-only`. Any other flag, attached
  short forms included, is unrecognised.
- **Environment.** **None from a caller or an agent definition at all.** Variables
  such as `PATH` (a fake `bwrap` swaps Codex's read-only root for a writable one),
  `TMPDIR` (a relative one resolves against Codex's cwd; one above `CODEX_HOME`
  lets the agent plant its own `config.toml`) and `LD_PRELOAD` each keep an
  exemption and defeat the sandbox, so an environment cannot be judged variable by
  variable. The plan records which keys came from `override.env` or from any
  `provider_overrides.env` in the effective agent (an agent definition in a user or
  project layer is a file an agent can write, so none counts as the operator's);
  the operator's own catalog launch env does not count. Also no `CODEX_*` variable
  except `CODEX_HOME`, which is the session's own boot dir, and no relative
  `TMPDIR` anywhere.
- **MCP upstreams.** The planted allow-list must name only `torque` and `tesseract`
  (`codexSafeMCPUpstreams`, `internal/app/protected_sandbox.go`), the two upstreams
  with no host-exec and no arbitrary file-write tool; any other upstream wraps the
  agent. This screen is not sufficient (B1 below).
- **Injection and `CODEX_HOME`.** No `boot_dir_overlay`, and no native file outside
  `skills/`. The `config.toml` in `CODEX_HOME` has only the shape Tether plants
  (`approval_policy`, a `sandbox_mode` of `workspace-write` or `read-only`,
  `[mcp_servers.*]`) plus the `[projects."<path>"] trust_level` entry Codex adds
  itself; any other table or key is not known-safe. `CODEX_HOME` is not inside a
  directory Codex's sandbox lets the agent write (the work or repo root, `/tmp`,
  `$TMPDIR`), or the agent could plant its own `config.toml` there in one turn.
- **Project config.** No `.codex/config.toml` in any of the launch's work
  directories or above them up to the project root (the nearest `.git`; never the
  user's home, whose `~/.codex/config.toml` is the user's own).
- **Directories.** None of the process directory, the project's work root or repo
  root, the workspace, `/tmp` or `$TMPDIR` contains a protected directory.
- **Posture.** Codex still runs under the accept-edits posture, which plants the
  workspace-write sandbox.

A launch that fails any of these is wrapped, and the daemon logs why (flag
names, never their values).

**Re-checked before every turn.** The exemption is judged at launch, from what is
on disk then, but Codex starts a new process each turn (`codex exec`), and
Codex's sandbox keeps `.codex/` read-only only from Codex: a Codex agent cannot
create `.codex/config.toml`, write into an existing `.codex/`, or symlink it away
(real Codex 0.159.3, in `codex exec` and `codex sandbox`), but another agent
sharing the project directory, a `git checkout` or an operator can. So before each
turn on an exempted session, whichever way it is delivered (`SendTurn`,
`SendInput`, the app-server `turn/start`, the wake sweep, the HTTP and MCP routes),
Tether looks again for a project `.codex/config.toml` in the work directories,
re-checks the `CODEX_HOME` `config.toml`, and re-resolves each work directory, so a
project root that is a symlink retargeted since the launch (to a directory holding a
project config, or one that now contains the catalog) is judged by where it points
now. If one has appeared, the turn is refused
with 403 `forbidden`, naming the file; remove it, or relaunch the session so Tether
wraps the agent instead. A running session cannot be re-wrapped, so refusing is
the fail-closed choice. This **narrows** a window and does not close it: the file
could appear between the check and Codex reading its configuration, which takes a
co-located writer. Closing it takes caller identity (CW-20260930-0253) and a way to
make `.codex` unwritable to other agents. The `config.toml` check is a strict line
check over the planted shape, because no TOML library is in the module graph; it
can only over-refuse.

**A maintenance point for Codex upgrades.** If a newer Codex writes other keys into
that `config.toml` at runtime (today it appends only `[projects."<path>"]
trust_level`), exempted sessions will be refused with a 403 naming the file and the
line the check rejected. That is loud and fail-closed, and the fix is one line: add
the key to the allowlist in `codexConfigUnsafe` (`internal/app/protected_sandbox.go`)
once it is known not to widen the sandbox. Where Tether's sandbox cannot start either, as on a
host that cannot nest bubblewrap, the launch fails loudly: the fail-closed
outcome. A Codex launch whose work directory, project root, workspace or state
database lies inside a protected directory is refused like any other. Paths are
resolved through symlinks, including the existing prefix of one that does not
exist yet, so `link-to-the-catalog/new` is judged as the catalog.


**What it would need before it is switched on** (the third review's findings; the
guard passes its own tests and none of these is caught by them):

- **B1: the default `{torque, tesseract}` list is not safe.** An exempted Codex's
  `torque mcp` boots agents in-process with a caller-chosen `workdir`
  (CW-20261001-0464), and `loom_export_bundle` writes where it is told
  (CW-20261001-0465). An allowlist of upstreams cannot be sound until upstreams run
  daemon-side or confine what they write (CW-20261001-0230). The list is one
  constant, `codexSafeMCPUpstreams`, which screens out upstreams with a host-exec or
  arbitrary file-write tool and is not sufficient.
- **SF4: the per-turn check accepts any content inside `[mcp_servers.*]`.** A
  co-located wrapped agent can add `[mcp_servers.evil] command="sh" args=[…]`,
  drop `--protect-path` from the planted `mux` args, or add `LD_PRELOAD` to its
  environment, and Codex spawns it unsandboxed. The planted shape is known at launch
  and would have to be pinned.

**Known limit: the spec launch engine.** With `TETHER_LAUNCH_ENGINE=spec` (off by
default) argv, environment and injection come from files under
`~/.tether/launch-specs/`, which an agent can write, and the guard inspects only
`plan.Args`. Do not enable it for agents left to Codex's own sandbox.

### Refusals

These launches are refused with 403 `forbidden`:

- a launch whose work directory, workspace or state database lies inside a
  protected directory (move it out of that directory);
- any launch Tether must sandbox, while `bwrap` is not installed or cannot
  build a namespace (install bubblewrap and allow unprivileged user
  namespaces, or turn protection off);
- any ACP launch (Copilot, Pi), until CW-20261001-0162 lets go-agent-wrapper
  protect one.

### Side effects of the minimal profile

An agent run under the minimal profile (every provider but Codex) sees a
slightly different machine:

- **A private PID namespace.** Host processes are invisible to it, `ps` shows
  only its own tree, and it cannot signal them.
- **Background processes end with the turn.** For a per-turn CLI (`claude -p`,
  `opencode run`, `agy`) the sandbox exists only for the turn's process, so a
  process the agent starts in the background does not outlive the turn.
- **Separate mounts.** `/home`, `~` and `~/.tether` are separate mounts inside
  the sandbox, so a `rename` across them fails with `EXDEV`; copy and delete
  instead.
- **No `sudo`.** The sandbox runs in a user namespace with no privilege to
  gain.

### The state directory

This change does not protect the state directory. The `mux mcp` server planted in
each agent used to open the state database from inside the sandbox, which is why;
it no longer does (CW-20261001-0173), so the directory can now be protected, in a
change of its own. Until then an agent can still write it directly.

Planted workers keep the `catalog.write` MCP scope: it also gates
`mux_agent_create` with `scope=project`, which writes into the repo and is not
protected, and it is not a boundary anyway (a worker can start its own
`mux mcp --scopes`).

**The catalog refusal is a policy, for every runtime.** The planted `mux mcp` is
started with `--protect-path` for each directory Tether protects from the agent
(the catalog root and the run directory), from the same decision that registers
the agent's protected paths. `mux_agent_create` and `mux_agent_edit` go through one
guard, which refuses a write under a protected directory with a typed
`catalog_read_only` error telling the agent to ask the operator, whether or not a
sandbox also makes it read-only. That matters because a
read-only mount only exists inside Tether's sandbox: Codex spawns MCP servers
itself, outside it, and a Codex agent calling `mux_agent_create scope=system`
wrote `<catalog>/agents/x.yaml` before this guard. Those two are the only native
tools that write a file into the catalog tree (audited); the rest read it, or write
the database or the daemon. An operator creates and edits agents with
`mux agents` or by editing the files. The unprotected `scope=project` and
`scope=user` still work (the user layer is CW-20261001-0192).

**The refusal holds against a symlink re-pointed while the call runs.** Resolving a
path, checking it, and writing it later loses that race: an agent whose repository
root is writable can flip `<repo>/.tether` between two symlinks, one into the
catalog, in a shell loop (3861 of 20000 project-scope creates wrote into the
catalog). So on Linux the tools open the destination directory once, judge that
open directory's identity and every ancestor's (device and inode, compared with the
protected directories'), and create or rewrite the file relative to it with
`O_NOFOLLOW`, so no path is resolved a second time. Missing directories are made
relative to it without following a symlink, and each is judged as it is opened. A
symlinked directory that does not lead into a protected one (`.tether`, `agents/`)
is followed as before; an agent file that is itself a symlink is not written
through while protected paths are in force, and the tool answers with a typed
`agent_file_is_symlink` error ("edit the link's target, or replace the link with a regular file"), not an internal error. Off Linux the check is on the resolved path and the race is narrowed,
not closed; the protection is not applied there yet (CW-20261001-0138). The race
tests flip the symlink while calling the real tools thousands of times and assert
nothing lands in the catalog (`internal/agentops`, `internal/mcpadapter`).

### macOS, status and the off switch

**macOS:** not applied yet. go-sandbox's seatbelt protection has not been
verified on a real Mac (CW-20261001-0138), so darwin launches run as before.

**Status:** `mux doctor` asks the running daemon, whose environment decides
protection, and reports a `sandbox-protect` check: ok when protection is on and
usable, a warning when it is off, a failure when it is on but bubblewrap cannot
build a namespace. The daemon logs the same at startup, and `GET /health`
carries it as `sandbox_protect`. Without a daemon to ask, `mux doctor` says it
is reporting its own shell's environment. A second check, `sandbox-protect-codex`,
reports how Codex is protected: a warning, `codex: not protected
(CW-20261001-0230)`, as shipped, and ok only if the dormant guard is switched on.

**Turning it off:** set `TETHER_SANDBOX_PROTECT=0` (or `false`) in muxd's
environment and restart the daemon. The daemon logs `WARN: control-plane
protection DISABLED by TETHER_SANDBOX_PROTECT=0, so agents can write the
catalog and run/` at startup, and `mux doctor` reports a `sandbox-protect`
warning. Agents then run as they did before protection, ACP launches
included. See [SECURITY.md](../SECURITY.md) for what protection stops and
what it does not.

## Profiles

A profile is a YAML file at `<catalog-root>/sandbox-profiles/<id>.yaml`:

```yaml
id: workspace-only
description: "Write workspace only. No network."
fs:
  read:
    - workspace                          # magic token → session workspace root
    - ${HOME}/Library/Application Support/Claude
  write:
    - workspace
  deny:
    - ${HOME}/.ssh
    - ${HOME}/.aws
    - ${HOME}/.config/gh
    - ${HOME}/.gnupg
net: false          # deny all outbound network connections
subprocess: true    # allow spawning child processes (git, npm, etc.)
```

### Special tokens

| Token | Resolves to |
|-------|-------------|
| `workspace` | Absolute path to the session's workspace root |
| `${HOME}` | User's home directory |
| `~` | User's home directory |

### Seed profiles

Three seed profiles ship with `examples/sandbox-profiles/`:

| id | filesystem | network | subprocess | Typical use |
|----|-----------|---------|-----------|-------------|
| `workspace-only` | workspace r/w + Claude config r; denies .ssh/.aws/.gh | deny | allow | Default for most agents |
| `workspace-plus-net` | same as above | allow | allow | Agents that call APIs or use npm/pip/git |
| `unrestricted` | no restrictions | allow | allow | Debugging only |

Copy these into your catalog's `sandbox-profiles/` directory to get started.

## Platform notes

### macOS

Enforcement uses `sandbox-exec` with a generated SBPL profile. `sandbox-exec` is Apple-deprecated but still functional in macOS 15 Sequoia. The current implementation uses a **default-allow + selective deny** posture: outbound network and sensitive FS paths are blocked; everything else is permitted. A future version will tighten to default-deny once the required system-path allowlist is characterized.

### Linux

Enforcement uses `bwrap` (bubblewrap). Install it:
```sh
# Debian/Ubuntu
apt-get install bubblewrap
# Fedora/RHEL
dnf install bubblewrap
```

If `bwrap` is absent, or cannot build a namespace, every launch Tether must
sandbox is refused with 403 `forbidden`, because
[control-plane protection](#control-plane-protection-every-agent-tether-wraps) needs it,
unless the operator has turned that off with `TETHER_SANDBOX_PROTECT=0`.
Codex, which uses its own sandbox, is not refused. Any
agent with `default_sandbox` set also fails to launch. `bwrap` needs
unprivileged user namespaces too; where AppArmor restricts them
(`kernel.apparmor_restrict_unprivileged_userns=1`), the distribution's
`bwrap-userns-restrict` profile must allow `bwrap`.

**CI does not exercise the real-bubblewrap test.** The end-to-end protection test
(`TestProtectedLaunch_AgentCannotWriteCatalogOrRunDir`) skips itself where `bwrap`
cannot build a namespace, and on GitHub's `ubuntu-latest` runners it does: `bwrap:
setting up uid map: Permission denied`, even with bubblewrap installed. CI
therefore runs only the tests that need no namespace; the real-bubblewrap test and
the live checks run on a host that allows one.

### Other platforms

Not supported. Agents with a non-empty `default_sandbox` will fail to launch on Windows and other platforms.

## Opting out

To run an agent without a sandbox profile, omit `default_sandbox` from the agent's YAML (or set it to `""`). The agent then runs with no profile's restrictions, but still, on Linux, under [control-plane protection](#control-plane-protection-every-agent-tether-wraps).

## Adding a custom profile

Create `~/.tether/catalog/sandbox-profiles/my-profile.yaml`:

```yaml
id: my-profile
description: "My custom profile"
fs:
  read:
    - workspace
    - /usr/local/share/my-tool
  write:
    - workspace
    - /tmp/my-agent-scratch
  deny:
    - ${HOME}/.ssh
    - ${HOME}/.aws
net: true
subprocess: true
```

Then reference it in an agent:

```yaml
permissions:
  default_sandbox: my-profile
```

Restart the daemon to pick up the new profile.

An `agent_file` or `agent_inline` override on session create can name a
profile. The override's profile is the one applied at launch; if the catalog
doesn't define it, the session is refused with 404. It never silently falls
back to the catalog agent's profile.

**Interim rule: an override can tighten, but not change, a pinned profile.**
- If the catalog agent names no profile, an override may name any defined
  profile.
- If the catalog agent is pinned to a profile, an override may name only that
  same profile. Any other profile is refused with 403 `forbidden`.

The reason: an agent Tether launches holds `session.write` through its `mux`
MCP, so without this rule it could create a child session that loosens its
own sandbox. Tether cannot yet tell an operator from an agent caller. The rule
is lifted when per-caller identity (CW-20260930-0253) lands.

## Failure modes

| Scenario | Behavior |
|----------|----------|
| Profile name references a file that doesn't exist | The daemon starts, logs a warning naming the agent and profile, and `mux doctor` fails `catalog-sandbox-profiles`. Creating, launching or resuming a session of that agent is refused with 404 `not_found` naming the profile. The same applies to an `agent_file`/`agent_inline` override that names one. It never runs without a sandbox. |
| Profile name set but platform has no enforcement tool | Launch fails with `conflict` error |
| Sandbox application error (SBPL syntax, bwrap arg error) | Launch fails with `conflict` error |
| Agent has no `default_sandbox` field | No profile; on Linux the session runs under control-plane protection only |
| Work directory, workspace or state database inside a protected directory | Launch refused with 403 `forbidden` |
| Control-plane protection on and `bwrap` not installed or unable to build a namespace | Launch refused with 403 `forbidden` (not for Codex) |

## Follow-ups

- Default-deny SBPL posture for macOS (requires enumerated allowlist) — deferred to v0.1
- Per-host network ACLs — deferred
- `landlock` as Linux fallback when bwrap is absent — deferred
