-- 0069_ai_events_purpose.sql
--
-- Adds the caller-declared purpose tag to ai_events so decision (Decide) spend
-- can be budgeted and reported per purpose.

ALTER TABLE ai_events ADD COLUMN purpose TEXT;
CREATE INDEX IF NOT EXISTS idx_ai_events_operation_purpose
    ON ai_events(operation, purpose, timestamp DESC);
