-- CW-20261016-0136 X1e: complete producer metadata. Nullable, additive only.
-- No indexes, no backfill. Origin time stays in at; events.id orders the log.
-- uint64 producer sequence/generation use decimal TEXT to avoid signed SQLite
-- integer overflow. on_behalf_of remains the legacy scalar column.
ALTER TABLE events ADD COLUMN schema_version TEXT;
ALTER TABLE events ADD COLUMN producer_app TEXT;
ALTER TABLE events ADD COLUMN subject TEXT;
ALTER TABLE events ADD COLUMN turn_id TEXT;
ALTER TABLE events ADD COLUMN parent_id TEXT;
ALTER TABLE events ADD COLUMN raw_offset INTEGER;
ALTER TABLE events ADD COLUMN process_pid INTEGER;
ALTER TABLE events ADD COLUMN process_provider TEXT;
ALTER TABLE events ADD COLUMN process_runtime TEXT;
ALTER TABLE events ADD COLUMN provider_session_id TEXT;
ALTER TABLE events ADD COLUMN source_channel TEXT;
ALTER TABLE events ADD COLUMN source_confidence TEXT;
ALTER TABLE events ADD COLUMN producer_sequence TEXT;
ALTER TABLE events ADD COLUMN producer_generation TEXT;
ALTER TABLE events ADD COLUMN traceparent TEXT;
ALTER TABLE events ADD COLUMN in_reply_to TEXT;
ALTER TABLE events ADD COLUMN actor_role TEXT;
ALTER TABLE events ADD COLUMN on_behalf_of_json TEXT;
