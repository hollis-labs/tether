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

## Control-plane protection (every launch)

Separately from profiles, on Linux every agent the daemon launches gets two
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

### Codex uses its own sandbox

Codex is the exception: Tether does not wrap it, and registers no protected
paths for it, **when nothing that shapes its sandbox can widen it**. Tether
starts Codex (`exec` and `app-server`) under the accept-edits posture, which
gives it `sandbox_mode = "workspace-write"`, and that sandbox is bubblewrap too.
Bubblewrap cannot nest where unprivileged user namespaces are restricted
(AppArmor's `kernel.apparmor_restrict_unprivileged_userns=1`, the Ubuntu
default), so wrapping Codex in Tether's sandbox would stop it running any shell
command (`bwrap: No permissions to create a new namespace`).

Codex's `workspace-write` sandbox makes the whole filesystem read-only except
its writable roots: its working directory, `/tmp` and `$TMPDIR`, and whatever is
configured. The catalog and run directory are outside those, so a shell command
that writes them fails (CW-20261001-0142 live evidence: a Codex turn's
`touch <catalog>/x` is denied and `touch <workdir>/ok` succeeds, in both `exec`
and `app-server`).

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
- **Environment.** No `CODEX_*` variable except `CODEX_HOME`, and `CODEX_HOME` is
  the session's own boot dir.
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
Tether looks again for a project `.codex/config.toml` in the work directories and
re-checks the `CODEX_HOME` `config.toml`. If one has appeared, the turn is refused
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
database lies inside a protected directory is refused like any other.

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
`mux mcp --scopes`). A `mux_agent_create` or `mux_agent_edit` that writes into the
protected catalog fails with a typed `catalog_read_only` error telling the agent to
ask the operator, instead of a raw read-only-filesystem error. An operator creates
and edits agents with `mux agents` or by editing the files.

### macOS, status and the off switch

**macOS:** not applied yet. go-sandbox's seatbelt protection has not been
verified on a real Mac (CW-20261001-0138), so darwin launches run as before.

**Status:** `mux doctor` asks the running daemon, whose environment decides
protection, and reports a `sandbox-protect` check: ok when protection is on and
usable, a warning when it is off, a failure when it is on but bubblewrap cannot
build a namespace. The daemon logs the same at startup, and `GET /health`
carries it as `sandbox_protect`. Without a daemon to ask, `mux doctor` says it
is reporting its own shell's environment.

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
[control-plane protection](#control-plane-protection-every-launch) needs it,
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

To run an agent without a sandbox profile, omit `default_sandbox` from the agent's YAML (or set it to `""`). The agent then runs with no profile's restrictions, but still, on Linux, under [control-plane protection](#control-plane-protection-every-launch).

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
