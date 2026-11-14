-- Additive rollback guard: after coordinated identity seeding, even an older
-- binary cannot append an identity-less row. Existing IDs are never rewritten.
CREATE TRIGGER events_require_id_after_seed
BEFORE INSERT ON events
WHEN (NEW.event_id IS NULL OR NEW.event_id = '')
 AND EXISTS (SELECT 1 FROM event_identity WHERE singleton = 1)
BEGIN
 SELECT RAISE(ABORT, 'event_id_required_after_cutover');
END;
