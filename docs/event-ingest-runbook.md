# Runbook: first end-to-end event smoke test (Cerberus on a worker host to the hub)

> Status: written from the code, **not yet executed**. The command shapes are checked against
> `cmd/tether` and `cmd/tether-worker`; the first real run may find details to correct here.

Goal: prove that an app on a worker host can publish an event to the hub and that
a subscriber on the hub sees it. This is the paired-device route ("D"). The
worker-hosted relay (`events_relay`, see `event-ingest.md`) replaces the
per-service credential later. Nothing here is done by the build; an operator runs
it, and a person with operator authority mints the pairing code.

## Preconditions (all must hold; none is done by this runbook)

1. The Tether build with `POST /events` is deployed on the hub and the libs
   `harness/core` it depends on is a tagged release.
2. On the hub, `global.yaml` has `daemon.event_ingest.enabled: true` and the daemon
   was restarted. Check: `curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:<port>/events`
   on the hub's local socket or loopback API answers 400 or 401, not 405 (405 means
   the route is still off).
3. The libs `mesh/eventbus` client (or the plain HTTP call below) is available on
   the host that runs the publisher.

## Steps

1. **Mint a one-time pairing code** (operator, on the hub; valid at most one hour):

   ```sh
   tether pair --scope worker --label cerberus-<host> --ttl 15m
   ```

   `worker` is the scope that contains `tether.events`; do not grant more.

2. **Exchange the code on the worker host** into a private state root for this
   publisher only (not the worker's own root), as the user the publisher runs as:

   ```sh
   umask 077
   tether-worker --root ~/.events-publisher enroll --hub <hub URL from the worker's config.json> \
     --code <code> --label cerberus-<host>
   ```

   Do NOT pass `--install`. This writes `~/.events-publisher/device.token`
   (mode 0600). Check: `ls -l ~/.events-publisher/device.token` shows `-rw-------`
   and the owner is the publisher's user. The `<hub URL>` is the `hub` value in the
   worker's `config.json` on that host.

3. **Point the publisher at it:**

   ```sh
   export HOLLIS_EVENTS_URL=<hub URL>
   export HOLLIS_AUTH_TOKEN_FILE_CERBERUS=$HOME/.events-publisher/device.token
   ```

   Events must carry `app: "cerberus"` (the adapter sets it from config).

4. **Send one event** (any HTTP client; this is what the client library does):

   ```sh
   curl -sS -X POST "$HOLLIS_EVENTS_URL/events" \
     -H "Authorization: Bearer $(cat $HOLLIS_AUTH_TOKEN_FILE_CERBERUS)" \
     -H 'Content-Type: application/json' -H 'Tether-Protocol: 1' \
     -d '{"schema_version":"1","id":"evt_<32 hex>","kind":"alert.raised","app":"cerberus","sequence":1,"generation":1,"time":"<now, RFC 3339>","source":{"channel":"smoke","confidence":"exact"},"subject":"msg://alert/cerberus/smoke","payload":{"rule_id":"cerberus.smoke"}}'
   ```

   Expect `200` and `{"results":[{"id":"evt_...","status":"accepted","cursor":"<n>"}], ...}`.
   Send the same body again: `status: duplicate` with the same cursor.

5. **Read it back on the hub** with a subscriber that has `tether.read`:
   `GET /environment/events?after_seq=<cursor-1>&kind=alert.raised&app=cerberus`
   (needs the stream-filter change), or `tether_events_history`. The event
   appears with `actor` = the paired device and `class.visibility` = `environment`.

6. **Keep the credential alive:** a device credential lasts 30 days.
   `tether auth renew --hub <hub URL> --credential-file ~/.events-publisher/device.token`
   extends it without changing the token; run it daily (cron or a systemd timer)
   until the relay replaces this setup.

## What a failure means

| Answer | Meaning |
|---|---|
| 405 | `daemon.event_ingest.enabled` is off on the hub |
| 401 | credential missing, wrong, expired or revoked |
| 403 `scope_required` | the device was not paired with `worker` |
| 403 `app_mismatch` | a service credential was used for another app (devices are not bound) |
| 200 with `rejected` `forbidden_kind` | the kind is not owned by the event's `app` in the catalog `producers.json` |
| 200 with `rejected` `schema_invalid` | the envelope failed the ingest schema (id format, app, types) |
| 429 / 503 | back off per `Retry-After`; retrying the same body is safe |

## Rollback

Everything this adds is removable without touching the hub's other state:

- Revoke the device on the hub: `tether auth list` to find it, then
  `tether auth revoke <device-id>`.
- On the host: `rm -rf ~/.events-publisher` and unset the two environment
  variables.
- To turn the route off for everyone: set `daemon.event_ingest.enabled: false` and
  restart the hub daemon. Already stored events stay in the log and age out with
  the normal retention window.
