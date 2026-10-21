- Let a service token publish events from another host (CW-20261019-0044): the
  remote listener now accepts an issued `kind: "service"` JWT on exactly
  `POST /events` (no variant of the path, no other route or method), in addition to
  paired devices. The token is verified against the issuer's keys (iss, aud
  `tether`, exp, the shared deny-list), must be a live record of the service token
  issuer and hold `tether.events`, and is bound to its app by the ingest. It exists
  only while `daemon.event_ingest.enabled` is on. Refused tokens share a failure
  budget (`event_ingest.failed_auth_per_second` / `failed_auth_burst`) and answer
  429 when it is spent. `POST /events` also no longer needs the `Tether-Protocol`
  header. See `docs/service-tokens.md`.
