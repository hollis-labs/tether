- `tether hook claude --session <id> --spool <dir>` (CW-20261014-0143, slice 3a, hidden command): the command a Claude Code lifecycle
  hook runs. It reads one hook payload on stdin, maps it with `agentevents.ClaudeHook` and appends the events to the local
  spool. It is observe-only (silent, exit 0 on every path, so it never decides or blocks anything) and fail-open (a bad payload,
  an unknown event or an unwritable spool is one line on stderr and still exit 0), reads no catalog, database, network or token,
  and does nothing without both flags. Nothing plants it yet: that comes behind its own default-off flag, so no session runs it.
