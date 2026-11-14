- A worker reconnect can no longer kill live sessions (CW-20261114-0168). The welcome that answers a reconnecting
  worker ordered every session it held killed when the hub's lookup of it failed, so during a database stall 26 live
  sessions were SIGTERMed (exit 143/-1). A kill now needs a definite answer: the session is terminal on the hub, or
  the hub has no such session on that worker. A lookup that fails leaves the session running, and the hub asks for
  a replay once it can read it. The lookups run on a context detached from the connection, with a 10s bound, and a
  connection that goes away mid-handshake gets no welcome. Each kill is logged and published as
  `session.kill_ordered` with its reason.
- A remote device stream (a worker's connection, a Tachyon event stream) is no longer ended because its credential
  could not be re-verified while identity was unavailable (a stalled database). Before, any error, including the 1s
  verify timeout, read as revocation, and workers reconnected every 10–60s. An invalid, revoked, expired or narrowed
  credential still ends the stream at once. Verification that stays unavailable ends it after 2 minutes.
- Every terminal `session.state_changed` carries a `reason`:
  - a worker session: its stop reason, `stop`, `hub_kill:<why>` or `worker_exit`;
  - a local runtime with no reason of its own: `runtime_exit` or `stop`;
  - shim recovery: `shim_recovery`.
  Sessions closed while still created (`never_started`) and team-stopped worker sessions (`team_stop`), which ended
  without any event before, now publish one.
