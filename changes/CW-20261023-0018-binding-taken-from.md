- Report when a launch displaces a running session from a logical agent's mail address
  (CW-20261023-0018): the launch response (`POST /sessions/{id}/launch`, create-and-launch, resume)
  carries `binding_taken_from` (the displaced session id, omitted when none) and the hub logs the
  takeover. Behaviour is unchanged: the newest launch still takes the address. Documented in
  `docs/messaging.md` ("One address, one session"): address parallel workers of a shared agent by
  session URN.
