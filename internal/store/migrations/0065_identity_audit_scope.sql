-- Local-socket route-to-scope attribution (CW-20261010-0261). Observe only:
-- the table verdict is recorded, never applied. Rows without a verified
-- principal or without a table entry keep the defaults / NULL.
ALTER TABLE identity_audit ADD COLUMN route_pattern TEXT NOT NULL DEFAULT '';
ALTER TABLE identity_audit ADD COLUMN required_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE identity_audit ADD COLUMN would_allow INTEGER CHECK(would_allow IN (0, 1));
