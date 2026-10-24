- Refresh `docs/consumers-hosted-wake.md` for Nanite (CW-20261024-0047): Nanite is no
  longer described as downstream-only. The doc now states the current delivery model for
  pull-only hosts (list/watch/claim, no push), URN issuance and retry-safe registration
  by Tether, refusal of sends to a deprecated URN, send idempotency, and that sender
  identity is observed rather than enforced until CW-20261024-0023.
