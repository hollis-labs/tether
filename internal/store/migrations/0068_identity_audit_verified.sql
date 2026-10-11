-- One verified flag per decision-log row (CW-20261010-0264, RK-044): 1 when a
-- bearer credential resolved to a principal row, 0 for missing, invalid and
-- unavailable credentials. Rows written before this column are not backfilled
-- from authentication: they default to 0, so history never over-claims.
ALTER TABLE identity_audit ADD COLUMN verified INTEGER NOT NULL DEFAULT 0 CHECK(verified IN (0, 1));
