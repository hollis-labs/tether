-- The app a session speaks for (CW-20261020-0036): the one app id whose
-- service token the session may request. Stamped by the hub when the session
-- row is created and never changed afterwards. NULL means no app, which cannot
-- request a token. A session with a parent inherits the parent's app (or none)
-- and can never widen it; the value is never read from a token, an environment
-- variable or a request body the session controls.
ALTER TABLE sessions ADD COLUMN app TEXT;
