- A refused worker hook says how to fix it (CW-20261114-0057): when the Stop gate or the activity hooks refuse to arm
  because the worker binary or its directory is group- or other-writable, `stop-gate.disabled` and
  `agent-events.disabled` now name the path, its mode and the fix (`chmod 755 <dir>`; install release dirs with
  `install -d -m 0755`). `docs/worker-service.md` documents the two checks and the 0755 release-dir requirement. The
  check itself is unchanged; a group-writable dir is still refused even for the owner's private group.
