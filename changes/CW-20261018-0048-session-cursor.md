- Fix session inventory pagination across equal creation timestamps with a stable
  created_at and ID cursor (CW-20261018-0048). Preserve legacy timestamp cursors
  and document live-scan completeness semantics without a snapshot guarantee.
