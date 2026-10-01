-- CW-20260926-0007: rename the public instance field without changing identities.
-- Retain the old storage column so the previous binary can still read/write it
-- during rollback. Neither the HTTP nor Go API exposes the old field.
ALTER TABLE registry_entries ADD COLUMN tether_instance_id TEXT;
UPDATE registry_entries SET tether_instance_id = mux_instance_id;

-- Both binary versions can insert rows; the absent new value inherits the
-- existing authority. New writes mirror into the retained rollback column.
CREATE TRIGGER registry_instance_insert AFTER INSERT ON registry_entries
BEGIN
    UPDATE registry_entries
    SET tether_instance_id = COALESCE(NEW.tether_instance_id, NEW.mux_instance_id),
        mux_instance_id = COALESCE(NEW.tether_instance_id, NEW.mux_instance_id)
    WHERE urn = NEW.urn;
END;

CREATE TRIGGER registry_instance_update_new
AFTER UPDATE OF tether_instance_id ON registry_entries
WHEN NEW.tether_instance_id IS NOT NULL AND NEW.tether_instance_id != NEW.mux_instance_id
BEGIN
    UPDATE registry_entries SET mux_instance_id = NEW.tether_instance_id WHERE urn = NEW.urn;
END;

CREATE TRIGGER registry_instance_update_previous
AFTER UPDATE OF mux_instance_id ON registry_entries
WHEN NEW.tether_instance_id IS NULL OR NEW.tether_instance_id != NEW.mux_instance_id
BEGIN
    UPDATE registry_entries SET tether_instance_id = NEW.mux_instance_id WHERE urn = NEW.urn;
END;
