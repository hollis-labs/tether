-- replaced_shim_delete: a replaced session's shim custody row is history and cannot be deleted. The old
-- store created this guard in Go at open (internal/store/session_replacement.go,
-- installReplacementDeliveryFences); the next/ store runs no DDL at open, so it is a migration here, with
-- the same body. Its two companions guard go-messaging's tables, which the library creates after these
-- migrations, so they stay ported code (sessionstore.InstallReplacementDeliveryFences).

-- +goose Up
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS replaced_shim_delete BEFORE DELETE ON session_shims
WHEN EXISTS(SELECT 1 FROM session_replacements WHERE source_session_id=OLD.session_id)
BEGIN SELECT RAISE(ABORT,'replaced custody history is retained'); END;
-- +goose StatementEnd

