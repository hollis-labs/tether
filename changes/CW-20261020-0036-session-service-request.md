- A session can request a service token for its own app (CW-20261020-0036): the operator stamps `sessions.app` at launch
  (`POST /sessions {"app"}`, a registered event producer; migration 0098; children inherit and cannot widen it), and the session calls the local-only
  `POST /auth/service-tokens/request` for `msg://service/<app>/dev` with `tether.events`. `identity.service_request` is `auto`
  (issue and log) or, by default, approval (records the request, answers 202). Max 5 live per (app, session); audited without the
  token. See `docs/service-tokens.md`.
