-- Sender and mailbox-address verification events (CW-20261010-0262). event is
-- empty for ordinary request rows and names the finding otherwise
-- (from_unverified, as_unverified, from_refused, as_refused).
ALTER TABLE identity_audit ADD COLUMN event TEXT NOT NULL DEFAULT '';
