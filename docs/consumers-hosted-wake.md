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

## Nanite and other pull-only hosts

Nanite is not downstream-only any more: it runs its own API and CLI agents, a
Tether delivery bridge, a wake reactor and the Tether MCP gateway for its agents.
What a host like that relies on from Tether:

- **Delivery is pull, never push.** Tether does not inject a turn into a pull-only
  bridge (see the fence above). The host reads its actors' mail with
  `GET /messages/list` (repeatable, no side effects) or the resumable
  `GET /messages/watch` (`id: <seq>` framing, replay strictly after `after_seq`, `gap`
  on a hub reset; see [Subscriptions](messaging.md#subscriptions)), and settles
  deliveries with claim, ack and nack. It owns its cursor.
- **Tether mints and owns URNs.** A durable actor is registered once in the Tether
  registry and the host stores the URN. Register with an external key
  (`substrate` + `external_id`, for Nanite `nanite` and `<install_id>/<host_settings_id>`)
  so a retried enrollment returns the same URN instead of minting a second one; see
  [Retry-safe registration](messaging-integration.md). Retiring an actor deregisters
  it (soft delete to `deprecated`; the URN is never reused), and a send to a
  deprecated agent URN is then refused with `409 recipient_deprecated` instead of
  queueing unread mail; see [Sends to a retired actor](messaging-integration.md).
- **Sends are retry-safe.** Send and notify accept an `Idempotency-Key`, so a host
  that retries after a timeout gets the stored message back instead of a duplicate.
- **Sender identity is observed, not enforced yet.** Tether stamps each message with
  the verified principal it saw, but the daemon runs in `observe` mode, so a caller
  holding the shared operator credential can still choose its `from`. A host that
  needs trustworthy attribution should require `from_verification=verified` on what it
  accepts, and use scoped credentials once enforce mode and per-service credentials
  exist (CW-20261024-0023). See [caller identity](caller-identity.md).

CW-20261008-0126's Tether slice covers the consumer and hosted-wake boundaries
above. Tangent and Torque adoption remains downstream work. The consumer
architecture ADR task CW-20261002-0131 remains a separate prerequisite for the
Tangent plugin's stage pipeline and reply design.
