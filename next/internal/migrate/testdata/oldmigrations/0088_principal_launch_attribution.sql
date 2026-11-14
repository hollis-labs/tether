-- CW-20261016-0218 part B: record what vouched for a session credential's
-- scopes at launch. 'principal' and 'origin' are bounded by an authority;
-- 'internal_unattributed' and 'anonymous_external' were born with the full
-- profile ceiling and nothing to bound them. Rows from before this migration
-- hold '' (legacy); a legacy session credential with created_by='' is treated
-- as unbounded too. Nullable-by-default text, no backfill, no index.
ALTER TABLE principals ADD COLUMN launch_attribution TEXT NOT NULL DEFAULT '';
