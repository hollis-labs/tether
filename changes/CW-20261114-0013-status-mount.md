- Mount `/status` and `/status/{@handle}` on the daemon (CW-20261114-0013): #540 registered the seat status routes in the API
  but not in the daemon mount list, so `tether_status_set` / `tether_status_get` answered 404 in production. The mount
  inventory test now lists the typed-message routes.
- Typed posts from a worker session are stamped from the seat (CW-20261114-0013): a session credential lists its session
  address first, so `tether_message_post` stamped `from` as `msg://session/...`; it now uses the seat's catalog `urn:` (where seats receive), or
  its `msg://agent/agent-mux/<id>` address when it has none.
- One seat, two addresses (CW-20261114-0013): a seat's catalog `urn:` and its logical form
  `msg://agent/agent-mux/<id>` are the same principal. `as`/`to` are mapped to the catalog urn after the claim is
  verified, message reads compare both forms, a claim of either form is accepted when the credential holds the other,
  mail sent to the logical form is stored under the catalog urn, and `/auth/context` (whoami) and worker sessions'
  `TETHER_AGENT_URN` report the catalog urn (new launch-spec field `agent_urn`; the binding target is unchanged).
  Before, a seat using its logical form got 403 reading its own mail.
