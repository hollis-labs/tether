- The `events` table now persists the v1.1 envelope blocks (CW-20261016-0136, X1c; migration 0082,
  forward-only, additive, no backfill): the 17 scope dimensions as `scope_*` columns (the legacy `scope`
  string is untouched), the class, correlation and actor blocks, `content_type`/`truncated`, a log-assigned
  `recorded_at` and `epoch`, and the retention lane's `retention_class` (R-audit/R-state/R-activity/R-debug,
  NULL = legacy hold), `retention_policy_id` and `payload_digest` (`sha256:<hex>` or `withheld`, only ever
  set together with a NULL payload). `Bus.Publish` and `PublishTx` write whatever the event's `Envelope`
  carries and now persist `logical_agent_id`; nothing is stamped yet (that is the stamp stage). Legacy rows
  read as NULL. The first start scans the table once for four partial indexes.
