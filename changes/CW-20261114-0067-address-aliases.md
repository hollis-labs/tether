- Address aliases (CW-20261114-0067): an `agent` or `user` address can be declared an alias of a canonical address,
  making the two one mailbox. Mail sent, notified, typed-posted or group-mentioned to an alias is stored under the
  canonical address (metadata `tether.alias_to` keeps the address used; `from` is never rewritten); `inbox`, `list`, `watch`
  and `subscribe` on any form return the mail of every form; `get`, `thread`, `read`, `consume`, `archive`, `unarchive`
  and `claim` accept any form. Stored messages are never rewritten: mail under an address from before it became an alias
  keeps its `to`, read and archived state. A principal that owns one form may send and read as the others, so add and
  remove are for the operator credential only. `tether messages alias add|list|remove`, `tether_address_alias_add`,
  `tether_address_alias_list`, `tether_address_alias_remove`, `/messages/aliases`. An alias that already has mail of its
  own merges (as a view) rather than being refused. Migration `0103_address_aliases`. See `docs/messaging.md`.
- Registry lookup and `whoami` name the canonical address (CW-20261114-0067): looking up any form of an aliased address
  returns the one profile registered under it plus `canonical_urn` and `aliases`, and `tether registry lookup` /
  `tether_registry_lookup` accept an address the registry did not mint (a `msg://user/...` address) instead of failing
  with "cannot infer kind from URN". A `@handle` held by an alias resolves to the canonical address.
- No agent ever counts as Chrispian through an alias (CW-20261114-0067 review): the operator address and user addresses
  are held only by the operator credential or a user principal. An agent address aliased into his mailbox cannot send
  as his forms, read or settle his mailbox, render as `@chrispian`, set his status, or triage as `from_operator`.
- A device the operator paired that holds `read` (Tachyon) may still read his mailbox (GET list, watch, thread, item),
  never pull his inbox (the destructive read), send, mark read, archive, ack or consume as him (CW-20261114-0067).
