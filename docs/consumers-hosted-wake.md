# Channel consumers and hosted wake

`internal/app/consumers.ConsumerService` provides the channel read contract
used by transports. It delegates identity and channel authorization to its
configured reader. Consumers pass their actor identity through `as`; a daemon
denial remains a denial through the facade.

Read committed routed output from channel history or the channel stream.
`session.turn_output` is an event notification: its `message_id` may still be
staged and unavailable through message lookup. Event-bus sequence numbers and
channel history cursors are separate. The [consumer routing guide](consumer-guide-session-routing.md#turn-output-events-and-bridge-migration)
describes the output event and attachment contract.

For polling, persist the channel page's `next_since` after handling that page,
and pass it as `since` on the next read. The cursor survives a daemon restart.
History and latest reads preserve the original message body and attribution;
they do not consume or acknowledge a publication. Consumers own their durable
cursor and message-ID idempotency. The facade does not implement a downstream
application's stage pipeline, annotations, or reply handling.

Hosted mailbox wake follows actor ownership rather than channel observation.
`AttemptWake` checks the current actor binding before claiming a delivery and
again immediately before submitting a turn. The final check includes the
claimed generation, even when a replacement binding reuses the same session ID.
Revocation, replacement, or a binding lookup failure prevents submission; an
already claimed delivery is released for retry.

A pull-only actor cannot be woken through an explicit notify session override
or an untracked legacy message. Its most recent pull-only binding retains that
fence after revocation or expiry. An authorized later hosted binding replaces
the fence. Never-bound actors and actors whose most recent lapsed binding was
hosted retain the existing compatibility lookup. Exact session addresses remain
pinned and independent of actor rebinding.

The final binding check and `SendTurn` are separate operations. Rebinding in
that interval remains a race; this check does not provide an atomic ownership
transaction with the runtime. Wake never redirects a failed attempt to another
session or starts an external session.

CW-20261008-0126's Tether slice covers these consumer and hosted-wake boundaries.
Tangent, Nanite, and Torque adoption remains downstream work. The consumer
architecture ADR task CW-20261002-0131 remains a separate prerequisite for the
Tangent plugin's stage pipeline and reply design.
