- Document the one-delivery-path-per-hosted-session rules and settlement (CW-20261021-0029,
  step 4): `docs/messaging.md` now states the `TETHER_HOSTED_SESSION` marker, that mark-read
  settles the delivery, that a late wake of a read message is dropped, that a live recipient
  session's inbox pull is unread-only, and the offline-wake risk. The daemon-injected mailbox
  wake text now says the reminder is the only push and that a message marked read or consumed
  is never woken again.
