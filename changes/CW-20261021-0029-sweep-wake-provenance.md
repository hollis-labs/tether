- A wake the sweep retries keeps its mail provenance (CW-20261021-0029): the first attempt of a
  mailbox wake records its turn input as a `mail_wake` with the message id and the sender class
  derived from the verified principal stamp, but the sweep's retry (for example after a busy first
  attempt) went through the lower-level wake without that tag and was recorded as a generic
  `session_turn` with no message id and basis `none`. The sweep now tags it like
  `Service.AttemptWake` does, so a busy-retried wake of mail from an agent principal is classified
  `agent`, not `unknown`. No schema change.
