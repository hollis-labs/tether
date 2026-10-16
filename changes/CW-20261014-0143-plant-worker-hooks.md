- Worker-placed Claude sessions can record their activity (CW-20261014-0143, slice 3b-1). When a launch carries the new
  `agent_events` flag (sent only to workers that advertise the `agent-events` feature; nothing sets it yet, the hub flag is the
  next slice and defaults off), the worker creates a private `events/` spool directory (0700) in the session workspace outside
  the git workroot, writes `agent-hooks.json` (0600) and starts claude with `--settings` pointing at it. Claude loads that as an
  extra layer, so the user's and the project's hooks keep firing beside it. The hooks run `tether-worker hook claude --session
  <id> --spool <dir>` (new hidden command, same observe-only, fail-open, content-free contract as `tether hook claude`) by the
  worker's own absolute path; no token is on the command line or in the environment. If the worker's executable or its directory
  is group- or other-writable, or the directory is not private, the session launches without hooks and the worker logs why.
