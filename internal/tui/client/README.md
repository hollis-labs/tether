# internal/tui/client

TUI-facing wrapper around `internal/client` (the muxd daemon HTTP client).

## Why a wrapper

The shared `internal/client` package serves the CLI too — adding TUI-shaped
conveniences there would bloat it and couple the CLI to TUI concerns.
This package gives the Bubble Tea model a stable typed surface:

- `ListProjects / ListAgents / ListProviders / ListLaunches / ListSessions`
- `CreateAndLaunch(CreateAndLaunchRequest) CreateAndLaunchResponse`
- `Ping` — for the startup "is the daemon up?" check
- `ErrDaemonUnreachable` re-exported so callers check a single sentinel

Every error is wrapped as `tui client: <op>: <underlying>` so messages
bubbled back into `Update` via `tea.Msg` read cleanly in toasts.

## Usage contract — never block Update

Bubble Tea runs `Update` on a single goroutine. Calling a list method
from inside `Update` blocks all input handling until the daemon responds.
Always wrap daemon calls in a `tea.Cmd`:

```go
func listProjectsCmd(c *client.Client) tea.Cmd {
    return func() tea.Msg {
        ps, err := c.ListProjects(context.Background())
        if err != nil {
            return ProjectsLoadErrMsg{Err: err}
        }
        return ProjectsLoadedMsg{Projects: ps}
    }
}
```

Bubble Tea runs the closure on its own goroutine and pipes the return
value back into `Update` as a `tea.Msg`. Input latency stays flat.

## Context cancellation

Every method accepts a `context.Context`. Pass a `ctx` the program can
cancel on quit (wire it from the Bubble Tea program's context, or from
a dedicated cancel on the root model). Cancelling the context aborts
the in-flight HTTP request.

## Transport

Transport selection (UDS vs TCP) lives in the underlying
`internal/client` and is driven by the `listen_addr` form passed to
`New`:

- `unix:/path/to/sock` — Unix domain socket (default muxd config)
- `tcp:host:port` — loopback HTTP

No other transports are supported.
