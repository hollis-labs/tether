- A Tachyon chat launch of a catalog seat is bounded by the catalog (CW-20261114-0063): `POST /chat/{launch}/send`
  launching a worker session for an agent defined in the live catalog gives it the seat's catalog `mcp_scopes` (or the
  default set), never more, whichever device asked. It is the same rule as a relaunch (CW-20261114-0038) and is checked
  before the parent grant. The credential records `launch_attribution` `catalog` (scope basis `catalog`). It applies
  only when the caller has a principal: an anonymous chat launch (identity `observe`/`off`) stays `anonymous_external`,
  so `tether identity unbounded` and the enforce-flip guard still count it. Other launches, and chat launches of a seat
  the catalog does not define, are bounded by the launcher as before, and any dropped scope still publishes
  `session.scopes_narrowed`. Worker-hosted sessions only, as with 0038.
