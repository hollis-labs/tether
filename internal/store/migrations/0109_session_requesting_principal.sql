-- CW-20261115-0084: creator provenance, never inherited from a parent.
ALTER TABLE sessions ADD COLUMN requesting_principal TEXT;
