- A hosted actor whose mailbox URN is an `agt_` id is now pulled as the recipient session
  (CW-20261021-0029): the inbox pull treated a session as the recipient of an agent mailbox only
  when the session's logical agent id equalled the URN's id, so an actor registered as
  `agt_...` but hosted under a different logical agent id got the plain pull (read mail
  re-presented, nothing consumed). The session is also the recipient when the actor's
  current registry binding resolves to exactly that session, the rule the mailbox wake already
  uses. Nothing looser: a different session, no binding, a superseded, revoked or pull-only
  binding, or no session in the request keeps the plain pull. No schema change.
