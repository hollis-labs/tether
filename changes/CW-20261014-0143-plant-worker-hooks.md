- Worker-placed Claude sessions can record their activity (CW-20261014-0143, slice 3b-1). When a launch carries the new
  `agent_events` flag (sent only to workers that advertise the `agent-events` feature; nothing sets it yet, the hub flag is the
  next slice and defaults off), the worker creates a private `events/` spool directory (0700) in the session workspace outside
  the git workroot, writes `agent-hooks.json` (0600) and starts claude with `--settings` pointing at it. Claude loads that as an
  extra layer, so the user's and the project's hooks keep firing beside it. The hooks run `tether-worker hook claude --session
  <id> --spool <dir>` (new hidden command, same observe-only, fail-open, content-free contract as `tether hook claude`) by the
  worker's own absolute path; no token is on the command line or in the environment. If the worker's executable or its directory
  is group- or other-writable, or the directory is not private, the session launches without hooks and the worker logs why.
  Nothing is followed or reused from the workspace the agent can write: the events directory is created with `Mkdir` and, on a
  resume, must be a real directory (not a symlink) owned by the worker user with mode 0700, and `agent-hooks.json` is written
  fresh at every launch through a temp file and rename. Each hook has a 5 second timeout on Claude's side besides the
  recorder's own 200 ms budget. A refusal is visible: a log line and an `agent-events.disabled` marker in the session workspace
  (`agent_events=disabled:<reason>`), cleared by the next launch that plants successfully.
