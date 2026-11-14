- A relaunch create keeps the seat's scopes too (CW-20261114-0038): every catalog seat runs on a worker, where resume is
  off, so a restart is stop + create with `continuity: relaunch`, which took the launcher's scopes. Such a create is now
  capped by the grant of the seat's previous session (same logical agent and launch id, `scope basis parent_grant`,
  attribution `origin`) when that session has ended or has a stop requested; beside a still-running session it is
  bounded by the launcher, so a relaunch never copies a live seat's grant.
