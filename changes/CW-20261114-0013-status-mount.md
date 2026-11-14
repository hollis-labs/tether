- Mount `/status` and `/status/{@handle}` on the daemon (CW-20261114-0013): #540 registered the seat status routes in the API
  but not in the daemon mount list, so `tether_status_set` / `tether_status_get` answered 404 in production. The mount
  inventory test now lists the typed-message routes.
