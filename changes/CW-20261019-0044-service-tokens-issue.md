- Add service tokens (CW-20261019-0044): `tether service-token issue|list` and
  `POST|GET /auth/service-tokens` let the operator, over the local socket, issue a
  `kind: "service"` Ed25519 JWT (`msg://service/<app>/<env>`, one audience,
  explicit `<audience>.<verb>` scopes, `exp` ten years out) and list its metadata.
  The 60 minute issuer cap is unchanged for every other token. For audience
  `tether` a service token may hold only `tether.events`, `tether.read` and
  `tether.messages`; a wildcard scope needs `--allow-wildcard` (dev). Metadata is
  persisted in the issuer state, never the token. See `docs/service-tokens.md`.
