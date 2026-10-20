- Add an optional loopback events relay to `tether-worker` (CW-20261020-0032):
  with `events_relay` set in the worker's `config.json` to a loopback address,
  services on that host publish to `http://<address>/events` with no credential
  and the worker forwards to the hub's `POST /events` with its own device
  credential, passing the hub's status, `Retry-After` and body through. Off by
  default; binds to loopback only; never logs tokens or bodies. Dev risk: any
  local process can publish as any registered app (release fix: CW-20261020-0031).
  Also adds `docs/event-ingest-runbook.md` for the first end-to-end smoke test.
