-- Launch-time correlation stamp (CW-20261012-0062a): the Torque task a session
-- was launched for. A bare, opaque Torque id (CW-...), asserted only by the
-- launcher at creation and never changed afterwards; NULL when the launcher did
-- not name one. There is no existence check against Torque.
ALTER TABLE sessions ADD COLUMN task_id TEXT;
