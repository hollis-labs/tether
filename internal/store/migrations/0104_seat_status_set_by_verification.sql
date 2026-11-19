-- CW-20261114-0130: who set a seat's status, and whether the hub verified it,
-- the way mail records its sender (from_verification, from_principal).
-- set_by_verification: verified | stamped | unverified | '' (no principal).
ALTER TABLE seat_status ADD COLUMN set_by_verification TEXT NOT NULL DEFAULT '';
ALTER TABLE seat_status ADD COLUMN set_by_principal TEXT NOT NULL DEFAULT '';
