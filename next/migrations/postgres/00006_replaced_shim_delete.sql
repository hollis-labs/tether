-- replaced_shim_delete: a replaced session's shim custody row is history and cannot be deleted (the
-- SQLite trigger of the same name, created in Go at open by the old store). It uses the baseline's
-- shared replaced-session guard, as replaced_shim_insert and replaced_shim_update do.

-- +goose Up
CREATE TRIGGER replaced_shim_delete BEFORE DELETE ON session_shims
FOR EACH ROW EXECUTE FUNCTION tether_abort_if_replaced('OLD', 'session_id', 'replaced custody history is retained');

