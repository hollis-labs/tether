# Understand a protected Codex proxy

A protected local MCP proxy limits writes to Tether's control plane. It is
separate from the [server grant](limit-tools.md), and it does not make every
operation read-only.

## What the launched proxy protects

With control-plane protection enabled, Tether wraps the planted Codex proxy
and its stdio child process tree so the catalog, run and state trees are
read-only. A failed wrapper does not fall back to an unwrapped proxy.
The planted proxy also refuses catalog-writing native agent tools targeting
protected paths and uses `--daemon-only` to route native state operations to
the daemon rather than opening the state database itself.

This protects writes to those trees. Local upstreams retain host reads,
network access and writes elsewhere. Credentials may still be readable;
protection is not credential isolation or a general workspace sandbox.

## Remote upstreams require an explicit waiver

HTTP and SSE upstreams cannot inherit the proxy's filesystem protection,
even on loopback. A protected proxy excludes them unless their catalog entry
contains:

```yaml
allow_unconfined_remote: true
```

That is an operator opt-in to an unconfined remote server, not a way to protect
it. Prefer a stdio transport when the upstream must inherit local write
protection. Use [gateway status](troubleshoot.md) to see the exclusion reason.

## Codex itself still has limits

The whole Codex agent is currently reported as **not protected** by
`tether doctor`, despite the separately protected planted proxy. Its MCP config
is not pinned; substituted servers and host services can bypass that proxy.
`--confine` is a server allow-list, not a remedy for these paths.

Verified caller identity is in progress in CW-20260930-0253, initially in
observe mode; this guide does not assume identity-based rejection is active.
Daemon-side upstream execution is tracked in CW-20261001-0230. Check the
[security boundary](../../../SECURITY.md#agents-run-as-your-user) before treating
an agent's proxy grant as protection against a hostile process running as the
same user.
