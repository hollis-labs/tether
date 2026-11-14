CREATE TABLE execution_policy (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  policy_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
