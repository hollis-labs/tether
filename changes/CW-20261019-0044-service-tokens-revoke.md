- Add service token revocation and a public minimal deny-list (CW-20261019-0044):
  `tether service-token revoke <jti>|--app <app>` and
  `POST /auth/service-tokens/revoke` (local operator only) put the token's `jti` on
  the deny-list until its own `exp` (not the 65 minute session horizon), keep the
  revocation time in the listing, and take effect in Tether at once and in any
  other verifier within one poll interval after a successful poll (a verifier
  that cannot poll keeps its last list and can accept the token until it can).
  `GET /auth/denylist` (and `HEAD`) is now
  readable with no credential on both listeners: it holds only opaque `jti`, `sub`
  and `sid` ids with an `ETag`; writing it still needs admin. Previously reading it
  needed `tether.maintain`. See `docs/service-tokens.md`.
