- Claude PTY chat backend (CW-20261118-0010, PTY half): `internal/chatbackend` defines the backend seam for the chat wire
  (`Backend.Submit`/`Observed`, `Run.Events`/`Cancel`, `Info{Deltas, Approvals}`, `ErrBusy`) and a swappable `Finalizer`
  that fills `final` on every `part.end` (`{"text"}` for text, refusal and reasoning, arguments for tool calls, the block
  plus `text` and `is_error` for tool results) and can drop deltas. `claudepty` turns a Claude Code TUI session into
  chatstream/v1 runs: content from the vendor transcript (whole messages, no `part.delta`), turn boundaries from the
  hook spool, submit by bracketed paste + Enter, cancel by Esc; turns not submitted through it (typed, mail wakes,
  plugin messages) come out as observed runs with the prompt text. `claudetranscript` (transcript lines to claudejson
  frames) is Tether-independent and meant to move to libs chatstream/adapter. Not wired to any route yet; ui/ui-go is
  now v0.4.0 for `chatstream/adapter/claudejson`.
