-- 0063_ai_events_provenance.sql
--
-- Adds usage provenance fields to ai_events.

ALTER TABLE ai_events ADD COLUMN cost_kind TEXT;
ALTER TABLE ai_events ADD COLUMN upstream_provider TEXT;
ALTER TABLE ai_events ADD COLUMN billed_cost_usd REAL;
ALTER TABLE ai_events ADD COLUMN generation_id TEXT;
