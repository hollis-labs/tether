- Resolve agent instance addresses, `msg://agent/<authority>/<logical agent>/<session id>`
  (CW-20261023-0018, option C(a)): mail to one wakes exactly that session (the agent's home binding
  does not fence it), lands in its own mailbox, and never falls back. An unknown session or one of
  another agent is `404 instance_unknown`; an ended session is `409 instance_ended` with
  `details.superseded_by` (when known) and nothing is stored. Derived from the session row: no
  registry row or binding per instance. Receive-only; the inbox pull and mailbox reads follow in a
  separate change.
