- Let a service token administer the registry without reading mail (CW-20261024-0094, for Nanite's enrollment
  credential). A service token may now hold `tether.registry` (registry writes) in addition to events, read and
  messages, and there is a new narrow scope `tether.registry_read` for registry reads (`GET /registry/{kind}` and
  `/{id}`), held by `tether.read`, `tether.registry`, `tether.maintain` and `tether.admin`, so existing read-only
  credentials keep reading the registry and a registry-only credential no longer needs `tether.read` (which also
  opens `/messages/list`, `/inbox`, `/watch`, sessions and workers). Nothing else changes. These scopes are enforced
  on the remote surface and, for issued tokens on the local listener, only in identity `enforce` mode; in `observe`
  they are attributed, not enforced (see `docs/auth-scopes.md`).
