- Let a session read its own agent instance mailbox, `msg://agent/<authority>/<agent>/<session id>`
  (CW-20261023-0018, option C(a)): the `?as=` identity of a verified session is accepted for the
  instance address of that very session when the session belongs to that logical agent, also under
  `identity: enforce`, and its live inbox pull of it is unread-only and consumed. A sibling session of
  the same agent or the holder of the agent's binding is not the recipient. Receive-only: sending as the
  instance address stays refused and the principal's address set is unchanged.
