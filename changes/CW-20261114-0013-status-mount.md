- Mount `/status` and `/status/{@handle}` on the daemon (CW-20261114-0013): #540 registered the seat status routes in the API
  but not in the daemon mount list, so `tether_status_set` / `tether_status_get` answered 404 in production. The mount
  inventory test now lists the typed-message routes.
- Typed posts from a worker session are stamped from the seat (CW-20261114-0013): a session credential lists its session
  address first, so `tether_message_post` stamped `from` as `msg://session/...`; it now uses the first verified
  address that has a `@handle`.
