# Antigravity chat turns

With `daemon.chat_wire.enabled` enabled, a local `antigravity` seat using the
`subprocess` runtime supports `/chat/v1` turns through `chatbackend/agystdio`.
The flag remains off by default.

The backend borrows the launched runtime's session. Each turn runs the normal
`agy --output-format stream-json [--conversation <id>] -p=<prompt>` subprocess,
retaining the launch's command, args, environment, sandbox and permission posture:
`bypass` uses `--dangerously-skip-permissions`; other postures use `accept-edits`.
The runtime records the provider conversation ID and resumes it on subsequent
turns, including turns sent outside chat. Terminal success waits for process
exit and native conversation verification; a replacement ID is reported as
`run.error`. The runtime retains its authentication handling.

The agy chatstream decoder provides text deltas, whole tool arguments/results,
usage, and permission-denied activities. There is no approval bridge. App context
uses the input-only `<app-context>` block; it is not published as a user event.
Cancellation cancels the subprocess context, waits for the process to exit and
closes the run with `run.abort`. A reservation shared with ordinary seat input
rejects overlapping turns. Every run ends with exactly one terminal event.

`Observed()` stays empty and closes when the backend stops: turns submitted by
mail, CLI or other seat writers are not imported into the chat conversation.
Local shim-hosted seats, including reattached seats, use the same decoder on
raw bridge output. The internal shim worker reads an interrupt control while
agy runs, cancels and reaps that child, then emits an interrupted result while
retaining its conversation ID and the shim. Ordinary queued inputs remain ordered.

Worker-hosted seats currently return an explicit unsupported runtime error,
because their reduced output does not carry the raw agy frames needed by this
decoder. No subprocess is started on the hub for those seats.

Run the fake subprocess and continuity cases with:

```sh
go test -race ./internal/chatbackend/agystdio ./internal/provider/cli/antigravity
```

A real backend smoke turn is opt-in and uses the installed agy's normal login:

```sh
TETHER_TEST_REAL_AGY=1 go test -v ./internal/chatbackend/agystdio -run '^TestRealAgy$' -count=1
```
