- An agent JWT now carries the addresses recorded on its session's live credential (CW-20261024-0023). The JWT is bound to a
  session by its sid claim but carried only the session id, so a session could not author as the agent it was launched to
  act as (its logical-agent slug and registry URN). In observe mode that showed up as roughly 6,500 `from_unverified` /
  `as_unverified` audit events from legitimate sessions; under enforce every one of those messages would have been refused.
  The addresses are read from the daemon's own launch record, never from the request; a missing, revoked or unreadable
  record leaves the session id alone. No behaviour change in observe mode beyond the events stopping.
