# claudestream

Parse the newline-delimited JSON event stream emitted by the `claude` CLI
when invoked with `--output-format stream-json --verbose`.

## Status: incubating, extractable

This package lives **inside agent-mux** today so there's a validating
consumer to stabilize its shape. The intent is to promote it to
`~/Projects-apps/framework/libs/go-claudestream` (or a standalone repo)
once the agent-mux implementation settles, at which point Nanite —
the original source of these parsers — will also migrate to consume
the promoted package rather than maintaining its own copy.

**Rules that preserve extractability** (enforce in code review):

1. No imports from `github.com/chrispian/agent-mux/internal/...`.
2. No agent-mux-specific naming on the public API (event types, field
   names, function signatures).
3. Test fixtures live alongside the package, not in agent-mux test
   helpers.
4. Dependencies limited to Go stdlib. Anything else must be load-bearing
   (which nothing currently is).

When all Sprint 2 tasks stabilize, the promotion plan runs:

1. Copy `pkg/claudestream/` to its new module home.
2. `go mod init` + tag v0.1.0.
3. agent-mux + Nanite both replace in-tree copies with the module.

## Usage

```go
import "github.com/chrispian/agent-mux/pkg/claudestream"

func readClaude(r io.Reader) error {
    sc := claudestream.NewScanner(r)
    for {
        ev, ok, err := sc.Next()
        if err != nil {
            return err
        }
        if !ok {
            return nil // EOF
        }
        switch ev.Kind {
        case claudestream.KindSessionID:
            persist(ev.SessionID) // for --resume later
        case claudestream.KindDelta:
            fmt.Print(ev.Text)
        case claudestream.KindToolUse:
            log.Printf("tool_use: %s %v", ev.ToolUse.Name, ev.ToolUse.Input)
        case claudestream.KindUsage:
            fmt.Printf("\n[%d in / %d out tokens]\n",
                ev.Usage.InputTokens, ev.Usage.OutputTokens)
        case claudestream.KindError:
            return fmt.Errorf("claude: %s", ev.ErrorMsg)
        case claudestream.KindDone:
            // run complete; loop for next turn
        }
    }
}
```

## Invoking claude correctly

Pair this parser with a subprocess invocation of:

```
claude --print --output-format stream-json --verbose [--input-format stream-json] [--resume <session_id>]
```

- `--print` is required to get the JSON output modes at all (without it,
  claude opens its interactive full-screen TUI and emits no events on
  stdout).
- `--input-format stream-json` is optional; use it when you want to send
  structured turns via stdin rather than plain text.
- `--resume <session_id>` continues a prior conversation keyed by the
  session_id you captured from the first `KindSessionID` event.

## Source lineage

Parser logic was copied from
[Nanite](https://github.com/chrispian/nanite)'s `pkg/provider/pty_claude.go`
on 2026-04-19 with the original author's permission (same author on both
projects). Event-type naming was reshaped (`Kind` string constants
instead of bare string literals; single `Event` type per claude
event rather than Nanite's multi-provider `StreamEvent`); the wire
protocol handling is unchanged.

## Supported claude CLI version

Tested against claude CLI version as of 2026-04-19. The stream-json
schema is not officially documented — if a future claude release adds
new event kinds, this library skips them silently (forward-compat).
If existing kinds gain new fields, those fields will round-trip
through the parser as long as they don't rename existing ones.

## Not included

This library does NOT:

- Invoke the `claude` binary itself — subprocess spawning is the
  consumer's responsibility (see agent-mux's `internal/provider/cli/claudestream/`
  for a reference implementation).
- Detect the `claude` binary in PATH.
- Generate `.mcp.json` or scaffold agent-discovery (Nanite-specific).
- Manage session-resume state — that's a consumer concern too.
