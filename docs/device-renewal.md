# Device credential renewal

Paired device bearers expire 30 days after pairing or renewal. `POST /auth/renew`
extends the existing credential's server-side expiry. It does not issue a new
bearer. Empty requested scopes preserve the device's current scopes; renewal
cannot broaden scopes or revive an expired/revoked device.

`tether-worker run` attempts renewal at startup, then daily while connected.
A failed attempt retries hourly; disconnected scheduled checks defer hourly.
Each HTTP call is bounded to 15 seconds and cancellation follows worker shutdown.
Renewal runs separately from the command and socket paths: failures warn and do
not interrupt sessions. Each attempt re-reads the owned 0600 `device.token` file.
Offline devices must reconnect before expiry; expired devices need pairing again.

For non-worker devices, schedule the reusable command against the remote HTTP
listener (the local operator socket does not accept device self-renewal):

```sh
tether auth renew --hub http://127.0.0.1:REMOTE_PORT --host tether.example.invalid \
  --credential-ref file://tesseract/denylist-bearer
# Or: --credential-file /absolute/path/to/device.token
```

Replace the host placeholder with the remote listener's configured allowed hostname;
loopback connections may otherwise be rejected. `--host` sets only the HTTP Host
header; `--hub` still chooses the connection target.

The flags carry a path/reference, never the credential. File references honor
`XDG_STATE_HOME`; the hub uses `/var/lib/fabric/xdg/state`. The command prints
only the new expiry. It refuses redirects and never prints response bodies.
No `secrets set` or Tesseract restart is needed: renewal keeps the same bearer.

`/health` reports non-revoked device counts under `identity.device_expiry`:
`expiring_within_7d` includes the seven-day boundary, and `expired` includes now.
Lookup failures set `unavailable`. These aggregate fields contain no labels,
IDs, scopes or credentials. `tether doctor` warns on both counts or unavailable
checks. This warns about unattended devices even if no worker reconnect occurs.

## Deny-list observation

An expired Tesseract deny-list bearer causes polling failures while the verifier
keeps its last list (empty after a restart). Device renewal prevents this expiry
failure; it does not change the verifier's failure behavior.

The companion `infra/compute-hosting/credentials/denylist-watch.py` scans the
Cerberus stderr file with a durable cursor, bounded reads and atomic counters.
It never stores or emits log lines. A new file/truncation also records an
observation gap: unread rotated content might have been missed. Failures stay
visible until explicitly acknowledged after polling/revocation is verified;
silence alone is not recovery. The scanner acknowledgement clears only previously
observed counts, so a new failure read during acknowledgement remains visible.

Configure the hub daemon with
`TETHER_DENYLIST_WATCH_STATE=/var/lib/fabric/xdg/state/tether/denylist-watch.json`
to expose `deny_list_watch` in health. Without this explicit setting the field is
absent. Health reads only a bounded regular status file, never the log itself.
Missing/malformed state is unavailable; a timestamp older than five minutes is
stale. Doctor warns on missing/stale state, scan backlog, observation gaps or
unacknowledged failure counts. The infra templates include daily renewal and
minute file-scan user timers. Installing/enabling them and wiring the daemon's
environment are separate deployment steps; this change does not activate them.
Owner alert delivery remains pending the chosen channel.
