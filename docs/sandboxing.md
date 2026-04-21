# Sandboxing

Agent Mux can constrain what a session is allowed to do by running it under a **sandbox profile**. Profiles restrict filesystem writes to sensitive paths, outbound network access, and subprocess spawning.

## How it works

Every agent in your catalog can reference a named profile:

```yaml
# ~/.agent-mux/catalog/agents/my-agent.yaml
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

If `bwrap` is absent, any agent with `default_sandbox` set will fail to launch.

### Other platforms

Not supported. Agents with a non-empty `default_sandbox` will fail to launch on Windows and other platforms.

## Opting out

To run an agent without a sandbox, omit `default_sandbox` from the agent's YAML (or set it to `""`). This is equivalent to running with no enforcement.

## Adding a custom profile

Create `~/.agent-mux/catalog/sandbox-profiles/my-profile.yaml`:

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

## Failure modes

| Scenario | Behavior |
|----------|----------|
| Profile name references a file that doesn't exist | Catalog validation fails at `mux daemon start` |
| Profile name set but platform has no enforcement tool | Launch fails with `conflict` error |
| Sandbox application error (SBPL syntax, bwrap arg error) | Launch fails with `conflict` error |
| Agent has no `default_sandbox` field | No enforcement — session runs unrestricted |

## Follow-ups

- Default-deny SBPL posture for macOS (requires enumerated allowlist) — deferred to v0.1
- Per-host network ACLs — deferred
- `landlock` as Linux fallback when bwrap is absent — deferred
