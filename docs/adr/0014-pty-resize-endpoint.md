# ADR 0014 — PTY Resize Endpoint

**Status:** accepted
**Date:** 2026-04-19
**Supersedes:** —
**Superseded by:** —

## Context

v0.0.2 shipped the daemon with `pty.Start` at launch time only. The PTY's winsize is fixed at whatever `creack/pty` defaults to (usually 80×24) unless the provider plan overrides it. After launch, nothing updates the size — which is a problem for full-screen TUI providers (`claude` CLI, `vim`, `htop`) that compute cursor positions against the initial size and corrupt rendering when the outer terminal disagrees.

Sprint 2 smoke (2026-04-19) confirmed: running `claude` via `mux tui` → attach produced jumbled output because the PTY stayed at launch-time size while the user's terminal (and the TUI attach panel) were larger.

We need a way for the daemon to propagate resize events to the running session's PTY.

## Decision

Add a new local-API endpoint:

```
POST /sessions/{id}/resize
Content-Type: application/json

{
  "rows": 42,
  "cols": 120
}
```

- **Status 204** on success.
- **400 invalid_request** when rows or cols is zero or JSON is malformed.
- **404 not_found** when the session is not currently registered in the runtime (matches `ErrSessionNotRunning` semantics from `/sessions/{id}/stop`).
- **500 internal_error** for unexpected failures.

Implementation flows through: `api.Server.handleResizeSession` → `LaunchService.ResizeSession` → `runtime.Manager.Resize` → `provider.Session.Resize` → `session.Handle.Resize` → `pty.Setsize` (`creack/pty`). Providers that don't own a PTY (stub API provider) no-op at the `Session.Resize` layer.

`provider.Session` gains `Resize(ctx, rows, cols uint16) error` — an additive change compatible with ADR 0006 (provider contract is stable; additive changes allowed).

## Shape rationale

- **Single POST, not a subscription.** Terminal resizes are rare and idempotent. A subscription adds the wrong kind of complexity — WebSocket/SSE, reconnect logic, missed-event reconciliation — for no meaningful benefit.
- **uint16 for rows/cols.** Matches `creack/pty.Winsize` and the underlying `struct winsize` ioctl. JSON numbers are up-converted in the handler.
- **No `x_pixel`/`y_pixel` in v0.0.4.** Graphical terminal clients that need pixel dimensions can add them later — JSON field omission is back-compatible.
- **Zero-valued rows/cols rejected.** Accepting them would silently resize the PTY to 0x0 and make every subsequent child-process render operation fail. Clients that want to "unset" the size should not call the endpoint.
- **Not guarded by the per-session `inputMu`.** `pty.Setsize` is a single syscall on the master fd and does not race with the input/output goroutines. Serialization here would just add latency during window drag.

## Client invariant

TUI attach screens send a resize on attach-screen mount (not on daemon connect — multiple attach cycles should each correct the size) and on every `tea.WindowSizeMsg` while attached. The client debounces consecutive resizes (~50 ms) so dragging a window doesn't spam the daemon.

CLI callers can hit the endpoint manually (`mux sessions resize <id> --rows 42 --cols 120`) but we don't ship such a subcommand in v0.0.4 — the TUI is the only caller that currently needs it.

## Back-compat

Existing clients that never call `/resize` see no behavior change. Sessions still launch at the default PTY size; resize only happens on demand.

## Consequences

- `provider.Session` interface grows one method. All existing implementations (`cliSession` in claudecode adapter, `Session` in stub API provider) must add `Resize`. Build fails loudly on missing implementations so we can't accidentally skip one.
- The daemon becomes responsible for honoring resize requests. If `pty.Setsize` returns an error (very rare — EBADF on a closed PTY), we surface it as `internal_error` and leave the client to retry or detach.
- When Sprint v004-s01-02 lands the TUI-side resize forwarder, the `claude` CLI (and any similar full-screen TUI provider) should render correctly on terminal resize.
- A future version may extend the endpoint with pixel dimensions or with per-attachment-scoped resize (if two TUIs attach and disagree on size, the daemon currently honors the most recent call). Not a problem in v0.0.4 because the canonical attach is one TUI at a time.

## Alternatives considered

- **Send resize over the existing `/input` channel as an escape sequence.** Fragile — the PTY would interpret it as child-process input, not a winsize change. ioctl is the only correct path.
- **Embed resize events inside the attach stream (bidirectional).** `GET /sessions/{id}/attach` is a one-way octet stream per ADR 0011; adding backchannel framing would violate that contract. A separate POST is cleaner.
- **Subscribe via SSE or WebSocket.** Overkill for a rare event (see "shape rationale").

## Follow-ups

- Sprint v004-s01-02 — TUI wires the client method.
- Sprint v004-s01-03 — external-terminal escape hatch, which does NOT need resize propagation (the external terminal talks to the daemon directly and can size itself).
- When PTY winsize pixel dimensions become relevant (iTerm's inline-image protocol, kitty graphics), extend `ResizeRequest` with optional `x_pixel`/`y_pixel` fields. No ADR needed — purely additive.
